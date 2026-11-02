package embedserver

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

func TestServeDiscoveryAndCancellation(t *testing.T) {
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	stderr := os.Stderr
	os.Stderr = writer
	defer func() { os.Stderr = stderr; writer.Close(); reader.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered, canceled, finished, release := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	finish := sync.OnceFunc(func() { close(release) })
	defer finish()
	closed := make(chan struct{})
	closedAfterInference := false
	e := &fakeEngine{run: func(ctx context.Context, _ string) ([]float32, int, error) {
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-release
		close(finished)
		return nil, 0, ctx.Err()
	}, onClose: func() {
		close(closed)
		select {
		case <-finished:
			closedAfterInference = true
		default:
		}
	}}
	store, record, done := startServe(t, ctx, t.TempDir(), e, daemon.Endpoint.ListenContext)
	requestDone := startEmbeddingRequest(t, record)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "inference did not start")
	}
	cancel()
	require.ErrorIs(t, <-requestDone, io.EOF)
	<-canceled
	select {
	case <-closed:
		require.FailNow(t, "model closed before inference returned")
	default:
	}
	finish()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "cancellation did not drain inference")
	}
	assert.True(t, closedAfterInference)
	require.NoError(t, writer.Close())
	printed, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, "Loading nomic-embed-text model...\n"+record.Endpoint().BaseURL()+"/v1\n", string(printed))
	records, err := store.List()
	require.NoError(t, err)
	assert.Empty(t, records)
}

func TestServeRejectsStartup(t *testing.T) {
	called := false
	err := serve(t.Context(), Options{DataDir: t.TempDir(), Listen: "0.0.0.0:0"}, func(context.Context, string, string) (engine, error) {
		called = true
		return nil, errors.New("model must not load")
	}, daemon.Endpoint.ListenContext)
	require.ErrorContains(t, err, "loopback")
	assert.False(t, called)
}

func TestServeRejectsSecondOwner(t *testing.T) {
	store := daemon.RuntimeStore{Dir: t.TempDir(), Prefix: "kit-embed"}
	release, err := store.AcquireOwnerLock(t.Context())
	require.NoError(t, err)
	defer release()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	loaded := false
	err = serve(ctx, Options{DataDir: store.Dir}, func(context.Context, string, string) (engine, error) {
		loaded = true
		return nil, errors.New("model must not load")
	}, daemon.Endpoint.ListenContext)
	require.ErrorContains(t, err, "cannot own "+store.Dir+", another kennembed may be running")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoError(t, ctx.Err(), "owner lock wait exceeded its startup timeout")
	assert.False(t, loaded)
}

func TestServeRemovesStaleRecords(t *testing.T) {
	const stalePID = 999999
	if daemon.ProcessAlive(stalePID) {
		t.Skipf("pid %d is alive on this host", stalePID)
	}
	dir := t.TempDir()
	stale, err := daemon.RuntimeStore{Dir: dir, Prefix: "kit-embed"}.Write(daemon.RuntimeRecord{PID: stalePID, Network: daemon.NetworkTCP, Address: "127.0.0.1:9"})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	store, record, done := startServe(t, ctx, dir, new(fakeEngine), daemon.Endpoint.ListenContext)
	records, err := store.List()
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, record.PID, records[0].PID)
	assert.NoFileExists(t, stale)
	cancel()
	require.NoError(t, <-done)
}

func startServe(t *testing.T, ctx context.Context, dir string, e engine, listen func(daemon.Endpoint, context.Context) (net.Listener, error)) (daemon.RuntimeStore, daemon.RuntimeRecord, <-chan error) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, Options{DataDir: dir}, func(context.Context, string, string) (engine, error) { return e, nil }, listen)
	}()
	store := daemon.RuntimeStore{Dir: dir, Prefix: "kit-embed"}
	var record daemon.RuntimeRecord
	require.Eventually(t, func() bool {
		found, _, ok, err := daemon.Discover(t.Context(), store, daemon.DiscoverOptions{})
		if err != nil || !ok {
			return false
		}
		record = found
		return true
	}, 5*time.Second, 10*time.Millisecond)
	return store, record, done
}

func startEmbeddingRequest(t *testing.T, record daemon.RuntimeRecord) <-chan error {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, record.Endpoint().BaseURL()+"/v1/embeddings", strings.NewReader(`{"model":"nomic-embed-text","input":"x"}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	done := make(chan error, 1)
	go func() {
		res, err := http.DefaultClient.Do(req)
		if err == nil {
			res.Body.Close()
		}
		done <- err
	}()
	return done
}

type pausedError struct {
	formatting chan struct{}
	resume     chan struct{}
	once       sync.Once
}

func (e *pausedError) Error() string {
	e.once.Do(func() { close(e.formatting) })
	<-e.resume
	return "model unavailable"
}

func TestServePrintsFailureBeforeOwnerRelease(t *testing.T) {
	output, err := os.CreateTemp(t.TempDir(), "stderr")
	require.NoError(t, err)
	defer output.Close()
	stderr := os.Stderr
	os.Stderr = output
	defer func() { os.Stderr = stderr }()
	store := daemon.RuntimeStore{Dir: t.TempDir(), Prefix: "kit-embed"}
	failure := &pausedError{formatting: make(chan struct{}), resume: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- serve(t.Context(), Options{DataDir: store.Dir}, func(context.Context, string, string) (engine, error) { return nil, failure }, daemon.Endpoint.ListenContext)
	}()
	defer func() {
		close(failure.resume)
		require.ErrorIs(t, <-done, failure)
		printed, err := os.ReadFile(output.Name())
		require.NoError(t, err)
		require.Contains(t, string(printed), "model unavailable")
	}()
	select {
	case <-failure.formatting:
	case err := <-done:
		done <- err
		require.FailNow(t, "Serve returned before printing its error")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond) //nolint:kennlint // deadlinetest: probes the OS lock while error formatting is paused
	defer cancel()
	release, err := store.AcquireOwnerLock(ctx)
	if release != nil {
		release()
	}
	require.ErrorIs(t, err, context.DeadlineExceeded, "failure must be published before ownership is released")
}
