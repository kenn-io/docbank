package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/telemetry"
	"go.kenn.io/kit/telemetry/posthog"
)

func TestIngestCapturesAfterReleasingMutationGate(t *testing.T) {
	t.Setenv("TELEMETRY_ENABLED", "0")
	root := t.TempDir()
	metadata, err := store.Open(filepath.Join(root, "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, metadata.Close()) })
	blobsDir := filepath.Join(root, "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(blobsDir, "tmp"), 0o700))
	blobs, err := blob.New(store.NewPackCatalog(metadata), blobsDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	gate := NewOperationGate()
	captures := 0
	reporter, err := posthog.NewReporter(posthog.Options{}, posthog.WithAllowedEvent("document_added",
		posthog.RequireProperty("surface", func(value any) (any, bool) {
			captures++
			assert.Equal(t, "cli", value)
			acquired := gate.mu.TryAcquire(operationGateExclusiveWeight)
			assert.True(t, acquired, "telemetry runs after the mutation gate is released")
			if acquired {
				gate.mu.Release(operationGateExclusiveWeight)
			}
			return value, true
		})))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reporter.Close()) })
	source := filepath.Join(t.TempDir(), "synthetic.txt")
	require.NoError(t, os.WriteFile(source, []byte("synthetic content"), 0o600))
	selection, err := ingest.CompileSelection(ingest.Options{})
	require.NoError(t, err)
	report, err := runIngest(t.Context(), Deps{Store: metadata, Blobs: blobs, TelemetryCapture: telemetry.CaptureHandler(reporter, t.TempDir())}, gate, []string{source}, "/inbox", ingest.Options{}, selection, "cli")
	require.NoError(t, err)
	assert.Equal(t, 1, report.Added)
	assert.Equal(t, 1, captures)
	_, err = metadata.NodeByPath(t.Context(), "/inbox/synthetic.txt")
	require.NoError(t, err)
}
