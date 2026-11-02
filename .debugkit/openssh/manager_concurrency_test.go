//go:build unix

package openssh

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type observedDoneContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (c *observedDoneContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}

func TestConcurrentSameDestinationConnectsReuseCompletedOperation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fake := newFakeSSH()
	spawnStarted := make(chan struct{}, 1)
	releaseSpawn := make(chan struct{})
	fake.setSpawnGate(spawnStarted, releaseSpawn)
	manager := newTestManager(t, newSocketDir(t), fake)
	results := make(chan Generation, 2)
	errorsChannel := make(chan error, 2)
	connect := func(ctx context.Context) {
		generation, err := manager.Connect(ctx, "studio", testTarget("wes@studio"))
		results <- generation
		errorsChannel <- err
	}
	go connect(t.Context())
	<-spawnStarted
	waiterContext := &observedDoneContext{
		Context: t.Context(), observed: make(chan struct{}),
	}
	go connect(waiterContext)
	<-waiterContext.observed
	close(releaseSpawn)

	require.NoError(<-errorsChannel)
	require.NoError(<-errorsChannel)
	first, second := <-results, <-results
	assert.Positive(first)
	assert.Equal(first, second)
	assert.Equal(1, fake.spawnCount())
	fake.closeAll()
}

func TestConnectWaitingBehindDisconnectReevaluatesState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fake, manager, _ := newConnectedTestManager(t, "studio", "wes@studio")
	exitStarted := make(chan struct{}, 1)
	releaseExit := make(chan struct{})
	fake.setExitGate(exitStarted, releaseExit)
	disconnectResult := make(chan error, 1)
	go func() { disconnectResult <- manager.Disconnect(t.Context(), "studio") }()
	<-exitStarted
	connectGeneration := make(chan Generation, 1)
	connectResult := make(chan error, 1)
	waiterContext := &observedDoneContext{
		Context: t.Context(), observed: make(chan struct{}),
	}
	go func() {
		generation, err := manager.Connect(waiterContext, "studio", testTarget("wes@studio"))
		connectGeneration <- generation
		connectResult <- err
	}()
	<-waiterContext.observed
	close(releaseExit)

	require.NoError(<-disconnectResult)
	require.NoError(<-connectResult)
	assert.Positive(<-connectGeneration)
	assert.Equal(2, fake.spawnCount())
	assert.Equal(StateConnected, manager.State("studio"))
}

func TestConnectionArgumentsRejectGenerationReservedForTeardown(t *testing.T) {
	require := require.New(t)
	fake, manager, generation := newConnectedTestManager(t, "studio", "wes@studio")
	exitStarted := make(chan struct{}, 1)
	releaseExit := make(chan struct{})
	fake.setExitGate(exitStarted, releaseExit)
	disconnectResult := make(chan error, 1)
	go func() {
		disconnectResult <- manager.Disconnect(t.Context(), "studio")
	}()
	<-exitStarted

	_, err := manager.ConnectionArguments("studio", generation)
	close(releaseExit)
	require.NoError(<-disconnectResult)

	require.ErrorIs(err, ErrConnectionChanged)
}

func TestStateAccessDoesNotBlockBehindMuxProbe(t *testing.T) {
	fake, manager, generation := newConnectedTestManager(t, "studio", "wes@studio")
	probeStarted := make(chan struct{}, 1)
	releaseProbe := make(chan struct{})
	fake.setCheckGate(probeStarted, releaseProbe)
	result := make(chan error, 1)
	go func() {
		_, aliveErr := manager.IsAlive(t.Context(), "studio", generation)
		result <- aliveErr
	}()
	<-probeStarted
	touchResult := make(chan bool, 1)
	go func() { touchResult <- manager.TouchActivity("studio", generation) }()

	select {
	case applied := <-touchResult:
		assert.True(t, applied)
	case <-time.After(time.Second):
		require.Fail(t, "TouchActivity blocked behind mux probe")
	}
	close(releaseProbe)
	require.NoError(t, <-result)
}

func TestProbeResultCannotApplyAfterDisconnect(t *testing.T) {
	fake, manager, generation := newConnectedTestManager(t, "studio", "wes@studio")
	probeStarted := make(chan struct{}, 1)
	releaseProbe := make(chan struct{})
	fake.setCheckGate(probeStarted, releaseProbe)
	result := make(chan error, 1)
	go func() {
		_, aliveErr := manager.IsAlive(t.Context(), "studio", generation)
		result <- aliveErr
	}()
	<-probeStarted

	require.NoError(t, manager.Disconnect(t.Context(), "studio"))
	close(releaseProbe)

	require.ErrorIs(t, <-result, ErrConnectionChanged)
	assert.Equal(t, StateDisconnected, manager.State("studio"))
}

func TestConnectRetriesProbeErrorAfterConnectionReplacement(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fake, manager, _ := newConnectedTestManager(t, "studio", "wes@studio")
	probeStarted := make(chan struct{}, 1)
	releaseProbe := make(chan struct{})
	sentinel := errors.New("obsolete probe failure")
	var probeMu sync.Mutex
	probeCount := 0
	fake.onCheck = func(context.Context) (int, error) {
		probeMu.Lock()
		probeCount++
		currentProbe := probeCount
		probeMu.Unlock()
		if currentProbe == 1 {
			probeStarted <- struct{}{}
			<-releaseProbe
			return -1, sentinel
		}
		return 0, nil
	}
	result := make(chan struct {
		generation Generation
		err        error
	}, 1)
	go func() {
		generation, err := manager.Connect(t.Context(), "studio", testTarget("wes@studio"))
		result <- struct {
			generation Generation
			err        error
		}{generation: generation, err: err}
	}()
	<-probeStarted

	require.NoError(manager.Disconnect(t.Context(), "studio"))
	replacementGeneration, err := manager.Connect(
		t.Context(), "studio", testTarget("wes@studio"),
	)
	require.NoError(err)
	close(releaseProbe)
	obsoleteResult := <-result

	require.NoError(obsoleteResult.err)
	assert.Equal(replacementGeneration, obsoleteResult.generation)
}

func TestIsAliveRejectsStaleGeneration(t *testing.T) {
	_, manager, oldGeneration := newConnectedTestManager(t, "studio", "wes@old")
	_, err := manager.Connect(t.Context(), "studio", testTarget("wes@new"))
	require.NoError(t, err)

	_, err = manager.IsAlive(t.Context(), "studio", oldGeneration)

	require.ErrorIs(t, err, ErrConnectionChanged)
	assert.NotErrorIs(t, err, ErrProbeIndeterminate)
}
