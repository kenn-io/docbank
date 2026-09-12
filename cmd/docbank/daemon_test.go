package main

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"
	"go.kenn.io/kit/packstore"

	docbank "go.kenn.io/docbank"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/store"
)

func TestWebOriginUsesDedicatedEphemeralLoopbackListeners(t *testing.T) {
	first, firstURL, err := listenWebOriginWithIdentity(
		t.Context(), "00000000000000000000000000000000")
	require.NoError(t, err)
	require.NoError(t, first.Close())
	second, secondURL, err := listenWebOriginWithIdentity(
		t.Context(), "01010101010101010101010101010101")
	require.NoError(t, err)
	t.Cleanup(func() { _ = second.Close() })

	assert.NotEqual(t, firstURL, secondURL)
	for _, test := range []struct {
		listener net.Listener
		rawURL   string
		host     string
	}{
		{first, firstURL, "docbank-00000000000000000000000000000000.localhost"},
		{second, secondURL, "docbank-01010101010101010101010101010101.localhost"},
	} {
		u, err := url.Parse(test.rawURL)
		require.NoError(t, err)
		assert.Equal(t, test.host, u.Hostname())
		assert.Equal(t, "/", u.Path)
		assert.Empty(t, u.RawQuery)
		assert.Empty(t, u.Fragment)

		listener := test.listener
		host, port, err := net.SplitHostPort(listener.Addr().String())
		require.NoError(t, err)
		assert.Equal(t, "127.0.0.1", host)
		assert.NotEqual(t, "0", port)
		assert.Equal(t, port, u.Port())
	}

	disabled, disabledURL, err := listenWebOrigin(t.Context(), false)
	require.NoError(t, err)
	assert.Nil(t, disabled)
	assert.Empty(t, disabledURL)
}

func TestServeLocksBeforeInitializingVault(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "restore-target")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	lock, err := (home.Layout{Root: dir}).TryLockExclusive()
	require.NoError(t, err)
	t.Cleanup(func() { _ = lock.Release() })
	t.Setenv("DOCBANK_HOME", dir)

	err = runServe(context.Background())
	require.ErrorIs(t, err, home.ErrVaultLocked)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "vault.lock", entries[0].Name(),
		"daemon startup must not initialize a restore-owned target")
}

func TestServeRejectsOwnedProcessingSpoolBeforeCleanup(t *testing.T) {
	layout := home.Layout{Root: t.TempDir()}
	require.NoError(t, os.MkdirAll(layout.BlobTmpDir(), 0o700))
	embedded, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(),
		Processing: docbank.ProcessingOptions{SpoolDirectory: layout.BlobTmpDir()}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, embedded.Close()) })
	active := filepath.Join(layout.BlobTmpDir(), ".docbank-upload-active")
	require.NoError(t, os.Mkdir(active, 0o700))
	source := filepath.Join(active, "source")
	require.NoError(t, os.WriteFile(source, []byte("active upload"), 0o600))
	t.Setenv("DOCBANK_HOME", layout.Root)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	require.ErrorIs(t, runServe(ctx), docbank.ErrProcessingSpoolLocked)
	content, err := os.ReadFile(source)
	require.NoError(t, err)
	require.Equal(t, "active upload", string(content))
}

func TestServeRecoversInterruptedRestoreBeforeInitializingVault(t *testing.T) {
	dir := t.TempDir()
	digest := ""
	handoff, err := blob.NewPrimaryRestoreHandoff(
		filepath.Join(dir, "blobs"),
		packstore.Ownership{
			Format: packstore.OwnershipFormatV1,
			Vault:  "50000000-0000-4000-8000-000000000001",
			Store:  "50000000-0000-4000-8000-000000000002",
			Epoch:  "50000000-0000-4000-8000-000000000003",
		},
		&digest,
	)
	require.NoError(t, err)
	require.NoError(t, handoff.Prepare(t.Context()))
	t.Setenv("DOCBANK_HOME", dir)

	startServe(t)
	waitForDaemon(t, dir)
	pending, err := blob.PrimaryRestoreHandoffPending(filepath.Join(dir, "blobs"))
	require.NoError(t, err)
	assert.False(t, pending)
}

func TestConfiguredWatchStoresRejectOverlapBeforeDaemonStartup(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	cfg.Watches = []config.WatchConfig{{
		Name: "inbox", Source: root, Destination: "/inbox",
	}}
	cfg.StoreBindings = map[string]config.StoreBindingConfig{
		"archive": {Kind: "filesystem", Path: root},
	}
	stores := []store.BlobStore{{
		ID:   "10000000-0000-4000-8000-000000000001",
		Name: "archive", Kind: "filesystem", Role: "secondary",
		Lifecycle: "active", Binding: "archive",
	}}

	err := validateConfiguredWatchStores(cfg, stores)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "overlaps filesystem store")
}

func TestServeLocksExistingAncestorBeforeCreatingVault(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "restore-target")
	require.NoError(t, os.Mkdir(parent, 0o700))
	lock, err := (home.Layout{Root: parent}).TryLockExclusive()
	require.NoError(t, err)
	t.Cleanup(func() { _ = lock.Release() })

	nested := filepath.Join(parent, "docbank.db")
	t.Setenv("DOCBANK_HOME", nested)
	err = runServe(context.Background())
	require.ErrorIs(t, err, home.ErrVaultLocked)
	_, err = os.Lstat(nested)
	require.ErrorIs(t, err, os.ErrNotExist,
		"daemon startup must not create a nested root beneath a restore-owned target")
}

func TestServeServesAndShutsDownGracefully(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCBANK_HOME", dir)

	stop := startServe(t)
	// Discover via the runtime record like a real client would.
	waitForDaemon(t, dir)

	// Second daemon on the same vault must refuse.
	err := runServe(context.Background())
	require.Error(t, err)

	embeddedConfig := docbank.Config{Root: t.TempDir(),
		Processing: docbank.ProcessingOptions{SpoolDirectory: filepath.Join(dir, "blobs", "tmp")}}
	embedded, err := docbank.New(t.Context(), embeddedConfig)
	if embedded != nil {
		t.Cleanup(func() { require.NoError(t, embedded.Close()) })
	}
	require.ErrorIs(t, err, docbank.ErrProcessingSpoolLocked)

	stop()
	// Record removed on shutdown.
	recs, err := client.RuntimeStore(dir).List()
	require.NoError(t, err)
	assert.Empty(t, recs)
	embedded, err = docbank.New(t.Context(), embeddedConfig)
	require.NoError(t, err)
	require.NoError(t, embedded.Close())
}

// TestServeRequiresKeyEvenWhenConfigIsKeyless is the regression test for the
// keyless-loopback finding: an unconfigured api_key must not mean
// "unauthenticated," even though it's still valid to leave api_key unset on
// a loopback bind. The daemon must generate an ephemeral key and require it
// on every authenticated route regardless of who's asking.
func TestServeRequiresKeyEvenWhenConfigIsKeyless(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCBANK_HOME", dir)

	startServe(t)
	rec := waitForDaemon(t, dir)

	// No X-Api-Key at all: any local OS user reaching the loopback port
	// without a key must be refused, not silently served.
	resp, err := http.Get("http://" + rec.Address + "/api/v1/nodes/1")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// The daemon still generated a per-run key and published it: the same-
	// user CLI path (runtime record) must be able to use it successfully.
	key := rec.Metadata["api_key"]
	require.NotEmpty(t, key, "runtime record must carry the ephemeral api key")
	req, err := http.NewRequest(http.MethodGet, "http://"+rec.Address+"/api/v1/nodes/1", nil)
	require.NoError(t, err)
	req.Header.Set("X-Api-Key", key)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.NotEqual(t, http.StatusUnauthorized, resp.StatusCode)
}

// Fresh SQLite initialization and shutdown can each exceed ten seconds on busy
// Windows CI runners. These tests check daemon behavior, not performance.
const (
	daemonStartTimeout    = time.Minute
	daemonShutdownTimeout = 30 * time.Second
)

// startServe runs the daemon until stop or test cleanup, whichever comes first,
// and waits for it to exit so TempDir removal never races a live daemon.
func startServe(t *testing.T) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx) }()
	stop = sync.OnceFunc(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(daemonShutdownTimeout):
			require.Fail(t, "daemon did not shut down")
		}
	})
	t.Cleanup(stop)
	return stop
}

// waitForDaemon discovers the daemon through its runtime record like a real
// client and waits until it serves health checks, since discovery is published
// before worker registration finishes.
func waitForDaemon(t *testing.T, dir string) kitdaemon.RuntimeRecord {
	t.Helper()
	healthClient := &http.Client{Timeout: time.Second}
	var rec kitdaemon.RuntimeRecord
	require.Eventually(t, func() bool {
		recs, err := client.RuntimeStore(dir).List()
		if err != nil || len(recs) != 1 {
			return false
		}
		rec = recs[0]
		resp, err := healthClient.Get("http://" + rec.Address + "/health")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, daemonStartTimeout, 50*time.Millisecond)
	return rec
}
