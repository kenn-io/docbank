package client

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/docbank/internal/daemonauth"
	"go.kenn.io/docbank/internal/daemonlife"
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

// TestEnsureStopsPinglessRuntimeBeforeReplacement models both a daemon whose
// listener closed during job draining and a startup that published its record
// before answering. Public ping fields cannot authenticate a rebound port, so
// Ensure signals only the verified PID and starts replacement after it exits.
func TestEnsureStopsPinglessRuntimeBeforeReplacement(t *testing.T) {
	root, rec := startUnresponsiveRuntime(t)
	canonicalRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	started := false
	result, err := ensureDaemon(t.Context(), root,
		func(_ context.Context, gotRoot string) (kitdaemon.RuntimeRecord, error) {
			started = true
			assert.Equal(t, canonicalRoot, gotRoot)
			assert.False(t, kitdaemon.ProcessAlive(rec.PID),
				"replacement must wait for the recorded owner to exit")
			return NewRecord("127.0.0.1:2", "new-key", "new-token"), nil
		})
	require.NoError(t, err)
	assert.True(t, started)
	require.NotNil(t, result.Replaced)
	assert.Equal(t, rec.PID, result.Replaced.PID)
}

func TestEnsureRejectsForgedPingWithoutSendingRuntimeSecrets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows intentionally consumes the full graceful wait before force termination")
	}
	root, rec := startUnresponsiveRuntime(t)
	var leaked atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "" ||
			r.Header.Get("X-Docbank-Daemon-Token") != "" {
			leaked.Store(true)
		}
		switch r.URL.Path {
		case kitdaemon.DefaultPingPath:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(kitdaemon.PingInfo{
				OK: true, Service: Service, Version: version.Version, PID: rec.PID,
			})
		case daemonauth.ChallengePath:
			_ = json.NewEncoder(w).Encode(map[string]string{"proof": "forged"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	rec.Address = strings.TrimPrefix(ts.URL, "http://")
	_, err := RuntimeStore(root).Write(rec)
	require.NoError(t, err)

	result, err := ensureDaemon(t.Context(), root,
		func(_ context.Context, _ string) (kitdaemon.RuntimeRecord, error) {
			assert.False(t, kitdaemon.ProcessAlive(rec.PID))
			return NewRecord("127.0.0.1:2", "new-key", "new-token"), nil
		})
	require.NoError(t, err)
	require.NotNil(t, result.Replaced)
	assert.Equal(t, rec.PID, result.Replaced.PID)
	assert.False(t, leaked.Load(), "forged endpoint must receive no runtime secret")
}

func TestProvenClientRefusesRedialAfterChallengeConnectionCloses(t *testing.T) {
	const token = "connection-proof-token"
	var leaked atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "" ||
			r.Header.Get("X-Docbank-Daemon-Token") != "" {
			leaked.Store(true)
		}
		if r.URL.Path != daemonauth.ChallengePath {
			http.NotFound(w, r)
			return
		}
		nonce, err := hex.DecodeString(r.URL.Query().Get("nonce"))
		if err != nil {
			http.Error(w, "bad nonce", http.StatusBadRequest)
			return
		}
		w.Header().Set("Connection", "close")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"proof": daemonauth.Proof(token, nonce),
		})
	}))
	t.Cleanup(ts.Close)
	rec := NewRecord(strings.TrimPrefix(ts.URL, "http://"), "private-api-key", token)

	c, err := newProvenClientFor(t.Context(), rec)
	require.NoError(t, err)
	require.Error(t, c.Health(t.Context()),
		"a closed proven connection must fail instead of redialing")
	assert.False(t, leaked.Load(), "redial target must receive no runtime secret")
}

func TestStopSignalsPinglessDaemonWithoutSendingSecrets(t *testing.T) {
	root, rec := startUnresponsiveRuntime(t)
	stopped, err := Stop(t.Context(), root)
	assert.True(t, stopped, "the live runtime PID must be recognized as stopping")
	require.NoError(t, err)
	assert.False(t, kitdaemon.ProcessAlive(rec.PID))
}

func TestShutdownHTTPRejectionUsesProcessStop(t *testing.T) {
	_, rec := startUnresponsiveRuntime(t)
	rec.Metadata[metaShutdownToken] = "rejected-token"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == daemonauth.ChallengePath {
			nonce, err := hex.DecodeString(r.URL.Query().Get("nonce"))
			if err != nil {
				http.Error(w, "bad nonce", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"proof": daemonauth.Proof(rec.Metadata[metaShutdownToken], nonce),
			})
			return
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"title":"Unauthorized","status":401,"code":"unauthorized"}`))
	}))
	t.Cleanup(ts.Close)
	rec.Address = strings.TrimPrefix(ts.URL, "http://")

	started := time.Now()
	require.NoError(t, stopRecord(t.Context(), rec))
	elapsed := time.Since(started)
	if runtime.GOOS == "windows" {
		assert.GreaterOrEqual(t, elapsed, daemonlife.GracefulExitTimeout-time.Second,
			"Windows must preserve graceful draining before native termination")
	} else {
		assert.Less(t, elapsed, 5*time.Second,
			"a definitive rejection must take the graceful process-signal path immediately")
	}
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
		metaShutdownToken: "token",
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
