//go:build linux

package daemon_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/daemon"
)

func TestStopUnjudgedIdentityWithDeadLocalPID(t *testing.T) {
	pid := deadPID(t)
	proof := newDaemonProof(t, []byte("stop-secret-key"))
	rec := daemon.RuntimeRecord{PID: pid, Service: "test", ProcessIdentityV2: "future-v1:12345"}
	server, rec := startProofServer(t, rec, proof)
	defer server.Close()
	rec = storedStopRecord(t, rec)
	requests := 0
	err := daemon.Stop(t.Context(), rec, daemon.StopOptions{
		Proof:   proof,
		Timeout: time.Second,
		Request: func(_ context.Context, got daemon.RuntimeRecord) error {
			assert.Equal(t, rec, got)
			requests++
			server.Close()
			return nil
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, requests)
}
