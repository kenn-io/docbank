package daemonconn

import (
	"context"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net"
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

	"go.kenn.io/docbank/internal/daemon"
	"go.kenn.io/docbank/internal/daemonauth"
	"go.kenn.io/docbank/internal/version"
)

func TestMain(m *testing.M) {
	if ready := os.Getenv("DOCBANK_START_TEST_CHILD"); ready != "" && os.Getenv(EnvBackgroundDaemon) == "1" {
		if err := os.WriteFile(ready, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			os.Exit(2)
		}
		// Stand in for a child that has not published its runtime record yet.
		time.Sleep(time.Minute) //nolint:kennlint // runs in a child process that stands in for an unready daemon
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestStartCancellationStopsUnreadyChild(t *testing.T) {
	root := t.TempDir()
	ready := filepath.Join(t.TempDir(), "child-pid")
	t.Setenv("DOCBANK_START_TEST_CHILD", ready)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Start(ctx, root)
		done <- err
	}()
	var pid int
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(ready)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(string(data))
		return err == nil
	}, 10*time.Second, 10*time.Millisecond)
	child, err := os.FindProcess(pid)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = child.Kill()
		_ = child.Release()
		_, _ = waitDead(context.Background(), kitdaemon.RuntimeRecord{PID: pid}, daemon.ForcedExitTimeout)
	})
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(daemon.GracefulExitTimeout + daemon.ForcedExitTimeout + 5*time.Second):
		t.Fatal("launcher did not return after cancellation")
	}
	assert.False(t, kitdaemon.ProcessAlive(pid), "failed startup must release its child process")
}

func TestCreateTimeMatches(t *testing.T) {
	rec := NewRecord("127.0.0.1:1", "key", "tok", "")
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
	rec := NewRecord("127.0.0.1:1", "key", "tok", "")
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
	rec := NewRecord("127.0.0.1:1", "key", "tok", "")
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

func TestWebDiscoveryRequiresAdvertisedCapability(t *testing.T) {
	info := kitdaemon.PingInfo{Version: version.Version}
	fallback := NewRecord("127.0.0.1:1", "key", "tok", "")
	enabled := NewRecord("127.0.0.1:1", "key", "tok", "127.0.0.1:2")

	assert.False(t, webDiscoverOptions().Accept(fallback, info),
		"fallback-only or web-disabled daemons cannot serve docbank web")
	assert.True(t, webDiscoverOptions().Accept(enabled, info))
	assert.True(t, discoverOptions(true).Accept(fallback, info),
		"ordinary data clients do not require web assets")

	for _, address := range []string{
		"", "127.0.0.1:0", "127.0.0.1:nope", "127.0.0.1:99999",
		"localhost:43210", "192.0.2.1:43210", "not-an-address",
	} {
		rec := NewRecord("127.0.0.1:1", "key", "tok", address)
		assert.False(t, webDiscoverOptions().Accept(rec, info), address)
	}
}

func TestEnsureWebReplacesWebIncapableDaemon(t *testing.T) {
	root, rec := startUnresponsiveRuntime(t)
	result, err := ensureDaemonWithOptions(t.Context(), root,
		func(_ context.Context, _ string) (kitdaemon.RuntimeRecord, error) {
			assert.False(t, kitdaemon.ProcessAlive(rec.PID))
			return NewRecord("127.0.0.1:2", "new-key", "new-token", "127.0.0.1:3"), nil
		}, webDiscoverOptions())
	require.NoError(t, err)
	require.NotNil(t, result.Replaced)
	assert.Equal(t, rec.PID, result.Replaced.PID)
	assert.Equal(t, "127.0.0.1:3", result.Record.Metadata[metaWebAddress])
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
			return NewRecord("127.0.0.1:2", "new-key", "new-token", ""), nil
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
			_ = json.MarshalWrite(w, kitdaemon.PingInfo{
				OK: true, Service: Service, Version: version.Version, PID: rec.PID,
			})

		case daemonauth.ChallengePath:
			_ = json.MarshalWrite(w, map[string]string{"proof": "forged"})
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
			return NewRecord("127.0.0.1:2", "new-key", "new-token", ""), nil
		})
	require.NoError(t, err)
	require.NotNil(t, result.Replaced)
	assert.Equal(t, rec.PID, result.Replaced.PID)
	assert.False(t, leaked.Load(), "forged endpoint must receive no runtime secret")
}

type delayedProofWriteConn struct {
	net.Conn

	response      <-chan struct{}
	first         atomic.Bool
	deadline      time.Time
	proofDeadline time.Time
}

func (c *delayedProofWriteConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if !c.first.Swap(true) {
		select {
		case <-c.response:
		case <-time.After(time.Second):
			return n, errors.New("waiting for proof response")
		}
		// Keep write completion behind the response past Go's 50 ms reuse budget.
		timer := time.NewTimer(100 * time.Millisecond)
		<-timer.C
	}
	if err != nil {
		return n, fmt.Errorf("writing proof request: %w", err)
	}
	return n, nil
}

func (c *delayedProofWriteConn) SetDeadline(deadline time.Time) error {
	c.deadline = deadline
	if !deadline.IsZero() {
		c.proofDeadline = deadline
	}
	if err := c.Conn.SetDeadline(deadline); err != nil {
		return fmt.Errorf("setting proof deadline: %w", err)
	}
	return nil
}

func TestProvenClientDelayedChallengeWrite(t *testing.T) {
	const token = "synthetic-proof-token"
	var challenges, requests, dials atomic.Int64
	response := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == daemonauth.ChallengePath {
			challenges.Add(1)
			assert.Empty(t, r.Header.Get("X-Api-Key"))
			assert.Empty(t, r.Header.Get("X-Docbank-Daemon-Token"))
			nonce, err := hex.DecodeString(r.URL.Query().Get("nonce"))
			if err != nil {
				http.Error(w, "bad nonce", http.StatusBadRequest)
				return
			}
			_ = json.MarshalWrite(w, map[string]string{"proof": daemonauth.Proof(token, nonce)})
			flusher, ok := w.(http.Flusher)
			if !assert.True(t, ok) {
				return
			}
			flusher.Flush()
			if challenges.Load() == 1 {
				close(response)
			}
			return
		}
		requests.Add(1)
		assert.Equal(t, "synthetic-api-key", r.Header.Get("X-Api-Key"))
		w.Header().Set("Connection", "close")
		_ = json.MarshalWrite(w, map[string]string{"status": "ok"})
	}))
	t.Cleanup(ts.Close)
	rec := NewRecord(strings.TrimPrefix(ts.URL, "http://"), "synthetic-api-key", token, "")
	var delayed *delayedProofWriteConn
	c, err := newProvenClientForDial(t.Context(), rec, func(ctx context.Context, network, address string) (net.Conn, error) {
		dials.Add(1)
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, fmt.Errorf("dialing proof connection: %w", err)
		}
		delayed = &delayedProofWriteConn{Conn: conn, response: response}
		return delayed, nil
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, c.Close()) }()
	require.False(t, delayed.proofDeadline.IsZero(), "proof has a socket deadline")
	require.True(t, delayed.deadline.IsZero(), "handoff clears the proof deadline")
	_, err = c.API().Health(t.Context())
	require.NoError(t, err)
	_, err = c.API().Health(t.Context())
	require.ErrorContains(t, err, "proven daemon connection is closed; refusing to redial")
	assert.Equal(t, int64(1), dials.Load())
	assert.Equal(t, int64(1), requests.Load())
	assert.Equal(t, int64(1), challenges.Load())
}

func TestProvenClientRejectsIncompleteProofHandoff(t *testing.T) {
	for _, scenario := range []string{"forged", "malformed", "oversized body", "oversized headers", "truncated", "close", "leftover", "redirect", "canceled", "deadline"} {
		t.Run(scenario, func(t *testing.T) {
			const token = "synthetic-proof-token"
			var dials, requests atomic.Int64
			timeout := probeOptions().Timeout
			if scenario == "deadline" {
				timeout = 100 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(t.Context(), timeout)
			defer cancel()
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				assert.Empty(t, r.Header.Get("X-Api-Key"))
				assert.Empty(t, r.Header.Get("X-Docbank-Daemon-Token"))
				nonce, err := hex.DecodeString(r.URL.Query().Get("nonce"))
				if err != nil {
					http.Error(w, "bad nonce", http.StatusBadRequest)
					return
				}
				body, err := json.Marshal(map[string]string{"proof": daemonauth.Proof(token, nonce)})
				if !assert.NoError(t, err) {
					return
				}
				switch scenario {
				case "forged":
					body = []byte(`{"proof":"forged"}`)
				case "malformed":
					body = []byte(`{"proof":`)
				case "oversized body":
					body = append(body, []byte(strings.Repeat(" ", 4<<10))...)
				case "oversized headers":
					w.Header().Set("X-Proof-Padding", strings.Repeat("x", 10<<20))
				case "truncated":
					w.Header().Set("Content-Length", strconv.Itoa(len(body)+1))
				case "close":
					w.Header().Set("Connection", "close")
				case "leftover":
					hijacker, ok := w.(http.Hijacker)
					if !assert.True(t, ok) {
						return
					}
					conn, buffered, err := hijacker.Hijack()
					if !assert.NoError(t, err) {
						return
					}
					defer func() { _ = conn.Close() }()
					_, _ = fmt.Fprintf(buffered, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%sleftover", len(body), body)
					assert.NoError(t, buffered.Flush())
					return
				case "redirect":
					http.Redirect(w, r, "/other", http.StatusFound)
					return
				case "canceled":
					cancel()
					return
				case "deadline":
					<-ctx.Done()
					return
				}
				_, _ = w.Write(body)
			}))
			t.Cleanup(ts.Close)
			rec := NewRecord(strings.TrimPrefix(ts.URL, "http://"), "synthetic-api-key", token, "")
			c, err := newProvenClientForDial(ctx, rec, func(ctx context.Context, network, address string) (net.Conn, error) {
				dials.Add(1)
				conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
				if err != nil {
					return nil, fmt.Errorf("dialing proof connection: %w", err)
				}
				return conn, nil
			})
			require.Error(t, err)
			require.Nil(t, c)
			switch scenario {
			case "canceled":
				require.ErrorIs(t, err, context.Canceled)
			case "deadline":
				require.ErrorIs(t, err, context.DeadlineExceeded)
			}
			assert.Equal(t, int64(1), dials.Load())
			assert.Equal(t, int64(1), requests.Load(), "failed proof must not send another request")
		})
	}
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
			_ = json.MarshalWrite(w, map[string]string{
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
		assert.GreaterOrEqual(t, elapsed, daemon.GracefulExitTimeout-time.Second,
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

func TestRunningMissingDaemonDoesNotCreateVault(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	t.Setenv("DOCBANK_HOME", root)
	c, ok, err := Running(t.Context())
	require.NoError(t, err)
	assert.Nil(t, c)
	assert.False(t, ok)
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
	identity, ok := kitdaemon.ReadProcessIdentity(rec.PID)
	require.True(t, ok, "read helper process identity")
	rec.ProcessIdentity = ""
	rec.ProcessIdentityV2 = identity
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
	time.Sleep(30 * time.Second) //nolint:kennlint // runs in a helper process that stands in for an unresponsive daemon
}
