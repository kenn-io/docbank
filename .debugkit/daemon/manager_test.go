package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

func TestManagerEnsureStartsAndPollsForCompatibleDaemon(t *testing.T) {
	assert := assert.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"tool","version":"v2"}`)
	}))
	defer server.Close()

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	started := false
	manager := daemon.Manager{
		Store: store,
		Discover: daemon.DiscoverOptions{
			Probe: daemon.ProbeOptions{ExpectedService: "tool"},
		},
		Compatible: func(_ daemon.RuntimeRecord, info daemon.PingInfo) bool {
			return info.Version == "v2"
		},
		Start: func(context.Context) error {
			started = true
			_, err := store.Write(daemon.RuntimeRecord{
				PID:       os.Getpid(),
				Network:   daemon.NetworkTCP,
				Address:   listenerAddr(t, server),
				Service:   "tool",
				Version:   "v2",
				StartedAt: time.Now(),
			})
			return err
		},
	}

	rec, info, err := manager.Ensure(t.Context(), time.Second)
	require.NoError(t, err)
	assert.True(started)
	assert.Equal("tool", rec.Service)
	assert.Equal("v2", info.Version)
}

func TestManagerFindSkipsIncompatibleDaemon(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"tool","version":"old"}`)
	}))
	defer server.Close()

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   daemon.NetworkTCP,
		Address:   listenerAddr(t, server),
		Service:   "tool",
		Version:   "old",
		StartedAt: time.Now(),
	})
	require.NoError(t, err)
	manager := daemon.Manager{
		Store:    store,
		Discover: daemon.DiscoverOptions{Probe: daemon.ProbeOptions{ExpectedService: "tool"}},
		Compatible: func(_ daemon.RuntimeRecord, info daemon.PingInfo) bool {
			return info.Version == "new"
		},
	}

	_, _, ok, err := manager.Find(t.Context())
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestManagerEnsureDoesNotStartWhenDiscoveryIsUnreachable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(daemon.NewRuntimeRecord("tool", "v1", daemon.Endpoint{
		Network: daemon.NetworkTCP,
		Address: listenerAddr(t, server),
	}))
	require.NoError(err)

	started := false
	manager := daemon.Manager{
		Store: store,
		Discover: daemon.DiscoverOptions{
			Probe:           daemon.ProbeOptions{ExpectedService: "tool"},
			RequirePIDAlive: true,
		},
		Start: func(context.Context) error {
			started = true
			return nil
		},
	}

	_, _, err = manager.Ensure(t.Context(), time.Second)
	require.Error(err)
	require.ErrorIs(err, daemon.ErrDaemonUnreachable)
	assert.False(started)
}

func TestManagerFindScansPastIncompatibleDaemon(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	oldServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"tool","version":"old"}`)
	}))
	defer oldServer.Close()
	newServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"tool","version":"new"}`)
	}))
	defer newServer.Close()

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   daemon.NetworkTCP,
		Address:   listenerAddr(t, oldServer),
		Service:   "tool",
		Version:   "old",
		StartedAt: time.Now().Add(-time.Minute),
	})
	require.NoError(err)
	_, err = store.Write(daemon.RuntimeRecord{
		PID:       os.Getpid() + 1,
		Network:   daemon.NetworkTCP,
		Address:   listenerAddr(t, newServer),
		Service:   "tool",
		Version:   "new",
		StartedAt: time.Now(),
	})
	require.NoError(err)

	manager := daemon.Manager{
		Store:    store,
		Discover: daemon.DiscoverOptions{Probe: daemon.ProbeOptions{ExpectedService: "tool"}},
		Compatible: func(_ daemon.RuntimeRecord, info daemon.PingInfo) bool {
			return info.Version == "new"
		},
	}

	rec, info, ok, err := manager.Find(t.Context())
	require.NoError(err)
	require.True(ok)
	assert.Equal(listenerAddr(t, newServer), rec.Address)
	assert.Equal("new", info.Version)
}

func TestManagerFindUsesCustomDiscovery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-Probe") != "present" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"tool","version":"v1"}`)
	}))
	defer server.Close()

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   daemon.NetworkTCP,
		Address:   listenerAddr(t, server),
		Service:   "tool",
		Version:   "v1",
		StartedAt: time.Now(),
	})
	require.NoError(err)

	manager := daemon.Manager{
		Store:    store,
		FindFunc: discoverWithHeader(store, "X-Test-Probe", "present"),
	}
	rec, info, ok, err := manager.Find(t.Context())
	require.NoError(err)
	require.True(ok)
	assert.Equal(listenerAddr(t, server), rec.Address)
	assert.Equal("tool", info.Service)
}

func TestManagerEnsureSerializesConcurrentStarts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"tool","version":"v1"}`)
	}))
	defer server.Close()

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	var starts atomic.Int32
	manager := daemon.Manager{
		Store:    store,
		Discover: daemon.DiscoverOptions{Probe: daemon.ProbeOptions{ExpectedService: "tool"}},
		Start: func(context.Context) error {
			starts.Add(1)
			time.Sleep(20 * time.Millisecond) //nolint:kennlint // sleeptest: simulates a slow start hook the manager must wait on
			_, err := store.Write(daemon.RuntimeRecord{
				PID:       os.Getpid(),
				Network:   daemon.NetworkTCP,
				Address:   listenerAddr(t, server),
				Service:   "tool",
				Version:   "v1",
				StartedAt: time.Now(),
			})
			return err
		},
	}

	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, _, err := manager.Ensure(t.Context(), time.Second)
			errs <- err
		}()
	}
	for range 2 {
		err := <-errs
		require.NoError(t, err)
	}
	assert.Equal(t, int32(1), starts.Load())
}

func TestRuntimeStoreOwnerLockExcludesASecondOwner(t *testing.T) {
	store := daemon.RuntimeStore{Dir: t.TempDir(), Prefix: "tool"}
	release, err := store.AcquireOwnerLock(t.Context())
	require.NoError(t, err)
	defer release()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond) //nolint:kennlint // deadlinetest: the deadline is the expected result; the first owner's file lock blocks the second acquire until it fires
	defer cancel()
	_, err = store.AcquireOwnerLock(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestRuntimeStoreStartLockIsPubliclyAcquirable(t *testing.T) {
	store := daemon.RuntimeStore{Dir: t.TempDir(), Prefix: "tool"}
	release, err := store.AcquireStartLock(t.Context())
	require.NoError(t, err)
	release()
}

func TestRuntimeStoreOwnerLockDoesNotBlockAnotherPrefixStartLock(t *testing.T) {
	dir := t.TempDir()
	ownerStore := daemon.RuntimeStore{Dir: dir, Prefix: "tool"}
	releaseOwner, err := ownerStore.AcquireOwnerLock(t.Context())
	require.NoError(t, err)
	defer releaseOwner()

	startStore := daemon.RuntimeStore{Dir: dir, Prefix: "tool.owner"}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	releaseStart, err := startStore.AcquireStartLock(ctx)
	require.NoError(t, err)
	releaseStart()
}

func TestManagerEnsureSerializesConcurrentStartsWithCustomDiscovery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var customProbes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-Probe") != "present" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		customProbes.Add(1)
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"tool","version":"v1"}`)
	}))
	defer server.Close()

	discoveryStore := daemon.RuntimeStore{Dir: t.TempDir()}
	lockStore := daemon.RuntimeStore{Dir: t.TempDir()}
	findCalls := make(chan struct{}, 8)
	find := discoverWithHeader(discoveryStore, "X-Test-Probe", "present")
	startEntered := make(chan struct{})
	releaseStart := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-releaseStart:
		default:
			close(releaseStart)
		}
	})

	var starts atomic.Int32
	manager := daemon.Manager{
		Store: lockStore,
		FindFunc: func(ctx context.Context) (daemon.RuntimeRecord, daemon.PingInfo, bool, error) {
			rec, info, ok, err := find(ctx)
			findCalls <- struct{}{}
			return rec, info, ok, err
		},
		Start: func(ctx context.Context) error {
			if starts.Add(1) == 1 {
				close(startEntered)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-releaseStart:
			}
			_, err := discoveryStore.Write(daemon.RuntimeRecord{
				PID:       os.Getpid(),
				Network:   daemon.NetworkTCP,
				Address:   listenerAddr(t, server),
				Service:   "tool",
				Version:   "v1",
				StartedAt: time.Now(),
			})
			return err
		},
	}

	type result struct {
		rec  daemon.RuntimeRecord
		info daemon.PingInfo
		err  error
	}
	results := make(chan result, 2)
	ensure := func() {
		rec, info, err := manager.Ensure(t.Context(), 2*time.Second)
		results <- result{rec: rec, info: info, err: err}
	}

	go ensure()
	requireSignal(t, startEntered)
	requireSignal(t, findCalls)
	requireSignal(t, findCalls)
	go ensure()
	requireSignal(t, findCalls)
	close(releaseStart)

	for range 2 {
		result := <-results
		require.NoError(result.err)
		assert.Equal(listenerAddr(t, server), result.rec.Address)
		assert.Equal("tool", result.info.Service)
	}
	assert.Equal(int32(1), starts.Load())
	assert.Equal(int32(2), customProbes.Load())
}

func TestManagerEnsureAppliesTimeoutToStartLock(t *testing.T) {
	require := require.New(t)

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	lockPath, err := store.LockPath()
	require.NoError(err)
	require.NoError(os.MkdirAll(store.Dir, 0o700))
	lock := flock.New(lockPath)
	require.NoError(lock.Lock())
	defer func() { _ = lock.Unlock() }()

	manager := daemon.Manager{
		Store: store,
		Start: func(context.Context) error {
			require.FailNow("start should not run while lock is held")
			return nil
		},
	}

	startedAt := time.Now()
	_, _, err = manager.Ensure(t.Context(), 50*time.Millisecond) //nolint:kennlint // deadlinetest: the held start lock forces the timeout being asserted
	require.Error(err)
	assert.Less(t, time.Since(startedAt), 500*time.Millisecond)
}

func discoverWithHeader(store daemon.RuntimeStore, name, value string) func(context.Context) (daemon.RuntimeRecord, daemon.PingInfo, bool, error) {
	return func(ctx context.Context) (daemon.RuntimeRecord, daemon.PingInfo, bool, error) {
		records, err := store.List()
		if err != nil {
			return daemon.RuntimeRecord{}, daemon.PingInfo{}, false, err
		}
		for _, rec := range records {
			ep := rec.Endpoint()
			client := ep.HTTPClient(daemon.HTTPClientOptions{DisableKeepAlives: true})
			client.Transport = headerTransport{
				base:  client.Transport,
				name:  name,
				value: value,
			}
			info, err := daemon.ProbeHTTP(ctx, client, ep.BaseURL(), daemon.ProbeOptions{ExpectedService: "tool"})
			if err == nil {
				return rec, info, true, nil
			}
		}
		return daemon.RuntimeRecord{}, daemon.PingInfo{}, false, nil
	}
}

type headerTransport struct {
	base  http.RoundTripper
	name  string
	value string
}

func (t headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set(t.name, t.value)
	return t.base.RoundTrip(req)
}

func requireSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		require.FailNow(t, "timed out waiting for test signal")
	}
}
