package store

import (
	"encoding/json/v2"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The browser harness copies this synthetic, stopped vault into a real daemon.
func TestProductionJobScreenshotFixture(t *testing.T) {
	ready := os.Getenv("DOCBANK_PRODUCTION_JOB_FIXTURE_READY")
	done := os.Getenv("DOCBANK_PRODUCTION_JOB_FIXTURE_DONE")
	if ready == "" || done == "" {
		t.Skip("opt-in production job screenshot fixture")
	}
	vault, root, setID, jobID := ProductionJobHTTPFixture(t)
	require.NoError(t, vault.CancelProductionJob(t.Context(), jobID))
	require.NoError(t, vault.Close())
	data, err := json.Marshal(struct {
		Root  string `json:"root"`
		SetID string `json:"set_id"`
		JobID string `json:"job_id"`
	}{Root: root, SetID: setID, JobID: jobID})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(ready, data, 0o600))
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(done); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("screenshot harness did not finish copying the synthetic vault")
}
