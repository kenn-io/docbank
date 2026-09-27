package store_test

import (
	"encoding/json/v2"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

// The browser harness copies this closed synthetic vault and finalizes it
// through the real daemon. The source vault stays inside Go's temporary test root.
func TestProductionFinalizationScreenshotFixture(t *testing.T) {
	ready := os.Getenv("DOCBANK_PRODUCTION_FINALIZE_FIXTURE_READY")
	done := os.Getenv("DOCBANK_PRODUCTION_FINALIZE_FIXTURE_DONE")
	if ready == "" || done == "" {
		t.Skip("opt-in production finalization screenshot fixture")
	}
	vault, root, setID, revision, etag, namespaceID := store.ProductionFinalizeHTTPFixture(t)
	require.Positive(t, revision)
	require.Positive(t, etag)
	require.NotEmpty(t, namespaceID)
	require.NoError(t, vault.Close())
	data, err := json.Marshal(struct {
		Root  string `json:"root"`
		SetID string `json:"set_id"`
	}{Root: root, SetID: setID})
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
