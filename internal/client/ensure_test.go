package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/docbank/internal/version"
)

func TestCreateTimeMatches(t *testing.T) {
	rec := NewRecord("127.0.0.1:1", "key", "tok")
	require.Equal(t, os.Getpid(), rec.PID)
	assert.True(t, createTimeMatches(rec), "own record must match")

	// Simulate PID reuse: same live PID, different recorded create time.
	rec.Metadata[metaCreateTime] = strconv.FormatInt(1, 10)
	assert.False(t, createTimeMatches(rec), "mismatched create_time must read as dead")

	// Records without the key (older daemons) match trivially.
	delete(rec.Metadata, metaCreateTime)
	assert.True(t, createTimeMatches(rec))
}

// A version-matched record without a published API key is a pre-key daemon:
// Ensure's discovery must reject it (so the replace path stops it) instead of
// returning a client that is either unauthenticated or doomed to 401s. The
// any-version discovery used by status/stop must keep accepting it, or the
// stale daemon could never be stopped.
func TestEnsureDiscoveryRejectsKeylessRecords(t *testing.T) {
	rec := NewRecord("127.0.0.1:1", "key", "tok")
	info := kitdaemon.PingInfo{Version: version.Version}

	require.True(t, discoverOptions(true).Accept(rec, info))

	delete(rec.Metadata, metaAPIKey)
	assert.False(t, discoverOptions(true).Accept(rec, info),
		"version-matched but keyless record must be replaced, not used")
	assert.True(t, discoverOptions(false).Accept(rec, info),
		"status/stop discovery must still see keyless daemons")
}

// Same-version development builds can still have incompatible HTTP behavior.
// A missing or mismatched protocol revision must therefore force replacement;
// status/stop discovery remains permissive so the old daemon can be stopped.
func TestEnsureDiscoveryRejectsProtocolMismatch(t *testing.T) {
	rec := NewRecord("127.0.0.1:1", "key", "tok")
	info := kitdaemon.PingInfo{Version: version.Version}

	require.True(t, discoverOptions(true).Accept(rec, info))

	delete(rec.Metadata, metaProtocolVersion)
	assert.False(t, discoverOptions(true).Accept(rec, info),
		"same-version record without a protocol revision must be replaced")
	assert.True(t, discoverOptions(false).Accept(rec, info),
		"status/stop discovery must still see incompatible daemons")

	rec.Metadata[metaProtocolVersion] = "0"
	assert.False(t, discoverOptions(true).Accept(rec, info),
		"same-version record with a mismatched protocol revision must be replaced")
}

// TestEnsureWaitsForLiveUnresponsiveRuntime models the interval after a
// daemon closes its listener but before a blocked background job lets the
// process exit. A concurrent command must wait for that verified owner rather
// than spawning a replacement that will collide with its vault lock.
func TestEnsureWaitsForLiveUnresponsiveRuntime(t *testing.T) {
	root, rec := startUnresponsiveRuntime(t)

	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := EnsureDaemon(ctx, root)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.GreaterOrEqual(t, time.Since(started), 200*time.Millisecond)
	assert.True(t, kitdaemon.ProcessAlive(rec.PID),
		"context cancellation must not force-kill the draining daemon")
	records, err := RuntimeStore(root).List()
	require.NoError(t, err)
	assert.Len(t, records, 1, "no replacement runtime record should be published")
}

func TestStopContextDoesNotKillDrainingDaemon(t *testing.T) {
	root, rec := startUnresponsiveRuntime(t)
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	stopped, err := Stop(ctx, root)
	assert.True(t, stopped, "the live runtime PID must be recognized as stopping")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.True(t, kitdaemon.ProcessAlive(rec.PID),
		"an interrupted client wait must not force-kill valid cleanup")
}

func TestEnsureReusesPinglessDaemonThatBecomesReady(t *testing.T) {
	root, rec := startUnresponsiveRuntime(t)
	var probes atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Ensure probes once before and twice inside launch coordination. Model a
		// slow startup that becomes healthy only during transition waiting.
		if probes.Add(1) <= 3 {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(kitdaemon.PingInfo{
			OK: true, Service: Service, Version: version.Version, PID: rec.PID,
		}); err != nil {
			t.Errorf("encoding ping: %v", err)
		}
	}))
	t.Cleanup(ts.Close)
	rec.Address = strings.TrimPrefix(ts.URL, "http://")
	_, err := RuntimeStore(root).Write(rec)
	require.NoError(t, err)

	result, err := EnsureDaemon(t.Context(), root)
	require.NoError(t, err)
	assert.False(t, result.Started)
	assert.Nil(t, result.Replaced)
	assert.Equal(t, rec.PID, result.Record.PID)
	assert.True(t, kitdaemon.ProcessAlive(rec.PID),
		"a compatible daemon that finishes startup must be reused")
}

func TestShutdownHTTPRejectionUsesProcessStop(t *testing.T) {
	_, rec := startUnresponsiveRuntime(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"title":"Unauthorized","status":401,"code":"unauthorized"}`))
	}))
	t.Cleanup(ts.Close)
	rec.Address = strings.TrimPrefix(ts.URL, "http://")
	rec.Metadata[metaShutdownToken] = "rejected-token"

	started := time.Now()
	require.NoError(t, stopRecord(t.Context(), rec))
	assert.Less(t, time.Since(started), 5*time.Second,
		"a definitive rejection must not consume the already-stopping grace window")
	assert.False(t, kitdaemon.ProcessAlive(rec.PID))
}

func TestStopMissingDaemonDoesNotCreateVault(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	stopped, err := Stop(t.Context(), root)
	require.NoError(t, err)
	assert.False(t, stopped)
	_, err = os.Stat(root)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func startUnresponsiveRuntime(t *testing.T) (string, kitdaemon.RuntimeRecord) {
	t.Helper()
	// The exact current test executable is intentional: the selected helper
	// provides a portable child PID on Unix and Windows without a shell.
	//nolint:gosec // os.Args[0] is not user-controlled command text in this test.
	cmd := exec.Command(os.Args[0], "-test.run=^TestUnresponsiveDaemonHelper$")
	cmd.Env = append(os.Environ(), "DOCBANK_UNRESPONSIVE_HELPER=1")
	require.NoError(t, cmd.Start())
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("helper process did not exit")
		}
	})

	var created int64
	require.Eventually(t, func() bool {
		var ok bool
		created, ok = processCreateTimeMillis(cmd.Process.Pid)
		return ok
	}, 5*time.Second, 10*time.Millisecond)

	root := t.TempDir()
	rec := kitdaemon.NewRuntimeRecord(Service, version.Version, kitdaemon.Endpoint{
		Network: kitdaemon.NetworkTCP, Address: "127.0.0.1:1",
	})
	rec.PID = cmd.Process.Pid
	rec.Metadata = map[string]string{
		metaCreateTime: strconv.FormatInt(created, 10),
		metaAPIKey:     "key", metaProtocolVersion: daemonProtocolVersion,
	}
	_, err := RuntimeStore(root).Write(rec)
	require.NoError(t, err)
	return root, rec
}

func TestUnresponsiveDaemonHelper(_ *testing.T) {
	if os.Getenv("DOCBANK_UNRESPONSIVE_HELPER") != "1" {
		return
	}
	time.Sleep(30 * time.Second)
}
