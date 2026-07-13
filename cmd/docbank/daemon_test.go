package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/home"
)

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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runServe(ctx) }()

	// Discover via the runtime record like a real client would.
	var rec kitdaemon.RuntimeRecord
	require.Eventually(t, func() bool {
		recs, err := client.RuntimeStore(dir).List()
		if err != nil || len(recs) == 0 {
			return false
		}
		rec = recs[0]
		resp, err := http.Get("http://" + rec.Address + "/health")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 10*time.Second, 50*time.Millisecond)

	// Second daemon on the same vault must refuse.
	err := runServe(context.Background())
	require.Error(t, err)

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not shut down")
	}
	// Record removed on shutdown.
	recs, err := client.RuntimeStore(dir).List()
	require.NoError(t, err)
	assert.Empty(t, recs)
}

// TestServeRequiresKeyEvenWhenConfigIsKeyless is the regression test for the
// keyless-loopback finding: an unconfigured api_key must not mean
// "unauthenticated," even though it's still valid to leave api_key unset on
// a loopback bind. The daemon must generate an ephemeral key and require it
// on every authenticated route regardless of who's asking.
func TestServeRequiresKeyEvenWhenConfigIsKeyless(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCBANK_HOME", dir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runServe(ctx) }()

	var rec kitdaemon.RuntimeRecord
	require.Eventually(t, func() bool {
		recs, err := client.RuntimeStore(dir).List()
		if err != nil || len(recs) == 0 {
			return false
		}
		rec = recs[0]
		resp, err := http.Get("http://" + rec.Address + "/health")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 10*time.Second, 50*time.Millisecond)

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

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not shut down")
	}
}
