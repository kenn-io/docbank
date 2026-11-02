package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

func TestNewPingHandlerEmitsRequiredPingInfo(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: "roborev",
		Version: "dev",
		PID:     123,
	}))
	defer server.Close()

	resp, err := doRequest(t, server.Client(), http.MethodGet, server.URL, "", nil)
	require.NoError(err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(http.StatusOK, resp.StatusCode)
	assert.Equal("application/json", resp.Header.Get("Content-Type"))
	var info daemon.PingInfo
	require.NoError(json.NewDecoder(resp.Body).Decode(&info))
	assert.True(info.OK)
	assert.Equal("roborev", info.Service)
	assert.Equal("dev", info.Version)
	assert.Equal(123, info.PID)
}

func TestNewPingHandlerRejectsNonGET(t *testing.T) {
	server := httptest.NewServer(daemon.NewPingHandler(daemon.PingHandlerOptions{}))
	defer server.Close()

	resp, err := doRequest(t, server.Client(), http.MethodPost, server.URL, "application/json", nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	assert.Equal(t, http.MethodGet, resp.Header.Get("Allow"))
}

func TestProbeHTTPRequiresOKTrue(t *testing.T) {
	assert := assert.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(daemon.DefaultPingPath, r.URL.Path)
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"roborev","version":"dev","pid":123}`)
	}))
	defer server.Close()

	info, err := daemon.ProbeHTTP(t.Context(), server.Client(), server.URL, daemon.ProbeOptions{
		ExpectedService: "roborev",
	})
	require.NoError(t, err)
	assert.True(info.OK)
	assert.Equal("roborev", info.Service)
	assert.Equal("dev", info.Version)
	assert.Equal(123, info.PID)
}

func TestProbeHTTPRejectsOKOmitted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"service":"kata"}`)
	}))
	defer server.Close()

	_, err := daemon.ProbeHTTP(t.Context(), server.Client(), server.URL, daemon.ProbeOptions{
		ExpectedService: "kata",
	})
	require.Error(t, err)
}

func TestProbeHTTPRejectsOKFalse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":false,"service":"kata"}`)
	}))
	defer server.Close()

	_, err := daemon.ProbeHTTP(t.Context(), server.Client(), server.URL, daemon.ProbeOptions{
		ExpectedService: "kata",
	})
	require.Error(t, err)
}

func TestProbeHTTPAppliesTimeoutOption(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond) //nolint:kennlint // sleeptest: server-side delay drives the real HTTP client timeout
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"kata"}`)
	}))
	defer server.Close()

	_, err := daemon.ProbeHTTP(t.Context(), server.Client(), server.URL, daemon.ProbeOptions{
		Timeout: time.Millisecond, //nolint:kennlint // deadlinetest: the stalled HTTP response must trigger the probe timeout
	})
	require.Error(t, err)
}

func TestDiscoverFindsResponsiveRuntime(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":"v1","pid":%d}`, os.Getpid())
	}))
	defer server.Close()

	addr := listenerAddr(t, server)
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   daemon.NetworkTCP,
		Address:   addr,
		Service:   "kata",
		Version:   "v1",
		StartedAt: time.Now(),
	})
	require.NoError(err)

	rec, info, ok, err := daemon.Discover(t.Context(), store, daemon.DiscoverOptions{
		Probe:           daemon.ProbeOptions{ExpectedService: "kata"},
		RequirePIDAlive: true,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(addr, rec.Address)
	assert.Equal("kata", info.Service)
}

func TestDiscoverRejectsPIDMismatchWhenRequiringLivePID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","pid":%d}`, os.Getpid()+1)
	}))
	defer server.Close()

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   daemon.NetworkTCP,
		Address:   listenerAddr(t, server),
		Service:   "kata",
		StartedAt: time.Now(),
	})
	require.NoError(t, err)

	_, _, ok, err := daemon.Discover(t.Context(), store, daemon.DiscoverOptions{
		Probe:           daemon.ProbeOptions{ExpectedService: "kata"},
		RequirePIDAlive: true,
	})
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestDiscoverSkipsMismatchedProcessIdentityWithoutProbing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	livePID := daemon.StartLivePIDHelperForTest(t)
	recordedIdentity, ok := daemon.ReadProcessIdentity(os.Getpid())
	require.True(ok)
	require.Equal(daemon.ProcessIdentityMismatch,
		daemon.CompareProcessIdentity(livePID, recordedIdentity))

	var probes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		http.Error(w, "must not be probed", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(daemon.RuntimeRecord{
		PID:             livePID,
		ProcessIdentity: recordedIdentity,
		Network:         daemon.NetworkTCP,
		Address:         listenerAddr(t, server),
		Service:         "kata",
		StartedAt:       time.Now(),
	})
	require.NoError(err)

	_, _, found, err := daemon.Discover(t.Context(), store, daemon.DiscoverOptions{
		Probe:           daemon.ProbeOptions{ExpectedService: "kata"},
		RequirePIDAlive: true,
	})
	require.NoError(err)
	assert.False(found)
	assert.Zero(probes.Load())
}

func TestDiscoverWithoutPIDCheckKeepsFailedProbeAsAbsence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(daemon.NewRuntimeRecord("kata", "v1", daemon.Endpoint{
		Network: daemon.NetworkTCP,
		Address: listenerAddr(t, server),
	}))
	require.NoError(t, err)

	_, _, found, err := daemon.Discover(t.Context(), store, daemon.DiscoverOptions{
		Probe: daemon.ProbeOptions{ExpectedService: "kata"},
	})
	require.NoError(t, err)
	assert.False(t, found)
}

func TestDiscoverReturnsUnreachableErrorForLiveRuntime(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	rec := daemon.NewRuntimeRecord("kata", "v1", daemon.Endpoint{
		Network: daemon.NetworkTCP,
		Address: listenerAddr(t, server),
	})
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(rec)
	require.NoError(err)

	_, _, ok, err := daemon.Discover(t.Context(), store, daemon.DiscoverOptions{
		Probe:           daemon.ProbeOptions{ExpectedService: "kata"},
		RequirePIDAlive: true,
	})
	require.Error(err)
	assert.False(ok)
	require.ErrorIs(err, daemon.ErrDaemonUnreachable)
	unreachable, ok := errors.AsType[*daemon.UnreachableError](err)
	if !ok || unreachable == nil {
		require.FailNow("expected UnreachableError")
		return
	}
	assert.Equal(rec.PID, unreachable.Record.PID)
	assert.Equal(rec.Endpoint(), unreachable.Endpoint)
	require.Error(unreachable.Err)
	require.ErrorIs(err, unreachable.Err)
}

func TestDiscoverScansPastUnreachableLiveRuntime(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	unreachableServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer unreachableServer.Close()
	reachableServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","pid":%d}`, os.Getpid())
	}))
	defer reachableServer.Close()

	livePID := daemon.StartLivePIDHelperForTest(t)
	identity, ok := daemon.ReadProcessIdentity(livePID)
	require.True(ok)
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(daemon.RuntimeRecord{
		PID:             livePID,
		ProcessIdentity: identity,
		Network:         daemon.NetworkTCP,
		Address:         listenerAddr(t, unreachableServer),
		Service:         "kata",
		StartedAt:       time.Now().Add(-time.Minute),
	})
	require.NoError(err)
	reachable := daemon.NewRuntimeRecord("kata", "v1", daemon.Endpoint{
		Network: daemon.NetworkTCP,
		Address: listenerAddr(t, reachableServer),
	})
	_, err = store.Write(reachable)
	require.NoError(err)

	found, info, ok, err := daemon.Discover(t.Context(), store, daemon.DiscoverOptions{
		Probe:           daemon.ProbeOptions{ExpectedService: "kata"},
		RequirePIDAlive: true,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(reachable.PID, found.PID)
	assert.Equal("kata", info.Service)
}

func TestManagerFindDoesNotDiscloseCredentialBeforeProof(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	livePID := daemon.StartLivePIDHelperForTest(t)
	assert.NotEqual(os.Getpid(), livePID)
	token := []byte("persistent-daemon-token")
	proof := newDaemonProof(t, token)
	var credentialDisclosed atomic.Bool
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			credentialDisclosed.Store(true)
		}
		if r.Header.Get("Authorization") != "" ||
			requestContainsSecret(r, body, token) {
			credentialDisclosed.Store(true)
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"service":"tool","version":"v1","pid":%d}`, livePID)
	}))
	defer attacker.Close()

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(daemon.RuntimeRecord{
		PID:       livePID,
		Network:   daemon.NetworkTCP,
		Address:   listenerAddr(t, attacker),
		Service:   "tool",
		Version:   "v1",
		StartedAt: time.Now().Add(-time.Hour),
	})
	require.NoError(err)

	manager := daemon.Manager{
		Store: store,
		Discover: daemon.DiscoverOptions{
			Probe:           daemon.ProbeOptions{ExpectedService: "tool"},
			RequirePIDAlive: true,
			Proof:           proof,
		},
	}

	_, _, ok, err := manager.Find(t.Context())
	require.Error(err)
	assert.False(ok)
	require.ErrorIs(err, daemon.ErrDaemonUnreachable)
	assert.False(credentialDisclosed.Load(), "credential material reached an unproved endpoint")
}

func TestDiscoverAcceptsValidRuntimeProof(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	key := []byte("valid-proof-key")
	serverProof := newDaemonProof(t, key)
	server, rec := startProofServer(t, daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Service:   "tool",
		Version:   "v1",
		StartedAt: time.Now(),
	}, serverProof)
	defer server.Close()

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(rec)
	require.NoError(err)

	found, info, ok, err := daemon.Discover(t.Context(), store, daemon.DiscoverOptions{
		Probe:           daemon.ProbeOptions{ExpectedService: "tool"},
		RequirePIDAlive: true,
		Proof:           newDaemonProof(t, key),
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(rec.Address, found.Address)
	assert.Equal(rec.PID, info.PID)
	assert.Equal(rec.Service, info.Service)
}

func TestProofPingHandlerPreservesStandardReadiness(t *testing.T) {
	assert := assert.New(t)

	server, rec := startProofServer(t, daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Service: "tool",
		Version: "v1",
	}, newDaemonProof(t, []byte("readiness-proof-key")))
	defer server.Close()

	info, err := daemon.Probe(t.Context(), rec.Endpoint(), daemon.ProbeOptions{
		ExpectedService: "tool",
	})
	require.NoError(t, err)
	assert.True(info.OK)
	assert.Equal(rec.PID, info.PID)
	assert.Equal(rec.Version, info.Version)
}

func TestProofProbeRejectsWrongKey(t *testing.T) {
	serverProof := newDaemonProof(t, []byte("server-proof-key"))
	server, rec := startProofServer(t, daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Service: "tool",
		Version: "v1",
	}, serverProof)
	defer server.Close()

	clientProof := newDaemonProof(t, []byte("different-proof-key"))
	_, err := clientProof.Probe(t.Context(), rec, daemon.ProbeOptions{
		ExpectedService: "tool",
	})
	require.Error(t, err)
}

func TestProofFormattingDoesNotDiscloseKey(t *testing.T) {
	assert := assert.New(t)
	key := []byte("format-sensitive-key")
	proof := newDaemonProof(t, key)
	manager := daemon.Manager{Discover: daemon.DiscoverOptions{Proof: proof}}
	byteList := fmt.Sprintf("%d", key)

	values := []struct {
		name  string
		value any
	}{
		{name: "pointer", value: proof},
		{name: "value", value: *proof},
		{name: "manager", value: manager},
	}
	for _, value := range values {
		for _, format := range []string{"%v", "%+v", "%#v"} {
			rendered := fmt.Sprintf(format, value.value)
			disclosed := strings.Contains(rendered, string(key)) || strings.Contains(rendered, byteList)
			assert.False(disclosed, "%s formatting disclosed proof key material", value.name)
		}
	}
}

func TestDiscoverPropagatesContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, _, _, err := daemon.Discover(ctx, daemon.RuntimeStore{Dir: t.TempDir()}, daemon.DiscoverOptions{})
	require.ErrorIs(t, err, context.Canceled)
}

func TestDiscoverSkipsPerProbeTimeouts(t *testing.T) {
	require := require.New(t)

	slowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer slowServer.Close()
	fastServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"kata","version":"fast"}`)
	}))
	defer fastServer.Close()

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   daemon.NetworkTCP,
		Address:   listenerAddr(t, slowServer),
		Service:   "kata",
		StartedAt: time.Now().Add(-time.Minute),
	})
	require.NoError(err)
	_, err = store.Write(daemon.RuntimeRecord{
		PID:       os.Getpid() + 1,
		Network:   daemon.NetworkTCP,
		Address:   listenerAddr(t, fastServer),
		Service:   "kata",
		StartedAt: time.Now(),
	})
	require.NoError(err)

	_, info, ok, err := daemon.Discover(t.Context(), store, daemon.DiscoverOptions{
		Probe: daemon.ProbeOptions{ExpectedService: "kata", Timeout: 100 * time.Millisecond}, //nolint:kennlint // deadlinetest: the first HTTP endpoint stalls until its probe times out
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(t, "fast", info.Version)
}

func TestProbeDialsUnixEndpoint(t *testing.T) {
	require := require.New(t)

	socketDir, err := os.MkdirTemp("", "kitd") //nolint:usetesting // unix socket paths must stay short, so the test needs a fixed OS temp root
	require.NoError(err)
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "daemon.sock")
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socketPath)
	require.NoError(err)
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	})

	server := http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"kata"}`)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Shutdown(context.WithoutCancel(t.Context())) })

	_, err = daemon.Probe(t.Context(), daemon.Endpoint{
		Network: daemon.NetworkUnix,
		Address: socketPath,
	}, daemon.ProbeOptions{ExpectedService: "kata"})
	require.NoError(err)
}

func listenerAddr(t *testing.T, server *httptest.Server) string {
	t.Helper()
	addr := server.Listener.Addr().String()
	require.NoError(t, func() error {
		_, _, err := net.SplitHostPort(addr)
		return err
	}(), "server address %q", addr)
	return addr
}

func requestContainsSecret(r *http.Request, body, secret []byte) bool {
	if bytes.Contains([]byte(r.URL.String()), secret) || bytes.Contains(body, secret) {
		return true
	}
	for _, values := range r.Header {
		for _, value := range values {
			if strings.Contains(value, string(secret)) {
				return true
			}
		}
	}
	return false
}

func newDaemonProof(t *testing.T, key []byte) *daemon.Proof {
	t.Helper()
	proof, err := daemon.NewProof(key)
	require.NoError(t, err)
	return proof
}

func startProofServer(t *testing.T, rec daemon.RuntimeRecord, proof *daemon.Proof) (*httptest.Server, daemon.RuntimeRecord) {
	t.Helper()
	require := require.New(t)

	server := httptest.NewUnstartedServer(nil)
	rec.Network = daemon.NetworkTCP
	rec.Address = server.Listener.Addr().String()
	handler, err := proof.NewPingHandler(rec)
	require.NoError(err)
	server.Config.Handler = handler
	server.Start()
	return server, rec
}

// doRequest issues a request bound to the test context so the linters see
// cancellation plumbed through instead of the context-free Get/Post helpers.
func doRequest(t *testing.T, client *http.Client, method, url, contentType string, body io.Reader) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, body)
	require.NoError(t, err)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return client.Do(req)
}
