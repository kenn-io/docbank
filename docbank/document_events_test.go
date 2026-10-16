package docbank

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmbeddedDocumentEventRebuildDrainsOnceAndReplaysCompletedReceipt(t *testing.T) {
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	content := []byte("synthetic timeline content")
	_, err = vault.Create(t.Context(), "/timeline.txt", bytes.NewReader(content), CreateOptions{
		MediaType: "text/plain", Expected: contentIdentity(content),
	})
	require.NoError(t, err)
	const operationID = "50000000-0000-4000-8000-000000000005"

	first, err := vault.RebuildDocumentEvents(t.Context(), operationID)
	require.NoError(t, err)
	assert.Equal(t, operationID, first.OperationID)
	assert.Equal(t, "completed", first.State)
	assert.Equal(t, int64(1), first.Scanned)
	assert.Equal(t, int64(1), first.Published)

	coverage, err := vault.DocumentEventCoverage(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(1), coverage.Selected)
	assert.Equal(t, int64(1), coverage.Indexed)
	assert.Equal(t, int64(1), coverage.MissingMetadata)
	assert.Equal(t, int64(1), coverage.UnboundProvenance)
	assert.Equal(t, int64(1), coverage.OperationalFallbacks)
	assert.Equal(t, first.TargetEpoch, coverage.InputEpoch,
		"the embedded rebuild must not allocate a second epoch while draining")

	secondContent := []byte("created after the completed receipt")
	_, err = vault.Create(t.Context(), "/later.txt", bytes.NewReader(secondContent), CreateOptions{
		MediaType: "text/plain", Expected: contentIdentity(secondContent),
	})
	require.NoError(t, err)

	replayed, err := vault.RebuildDocumentEvents(t.Context(), operationID)
	require.NoError(t, err)
	assert.Equal(t, first, replayed)
	replayedCoverage, err := vault.DocumentEventCoverage(t.Context())
	require.NoError(t, err)
	assert.Equal(t, coverage.InputEpoch, replayedCoverage.InputEpoch,
		"a completed replay must not bump or drain again")
	assert.Equal(t, int64(2), replayedCoverage.Selected)
	assert.Equal(t, int64(1), replayedCoverage.Pending,
		"a completed replay must not drain work created after its receipt")
}
