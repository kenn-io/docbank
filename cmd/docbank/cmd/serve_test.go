//go:build unix

package cmd

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/docbank/internal/client"
)

func TestServeServesAndShutsDownGracefully(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCBANK_HOME", dir)

	ctx, cancel := context.WithCancel(context.Background())
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
