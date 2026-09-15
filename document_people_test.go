package docbank

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEmbeddedDocumentPeopleRebuildDrainsAndReplays(t *testing.T) {
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	content := []byte("synthetic person attribution content")
	_, err = vault.Create(t.Context(), "/people.txt", bytes.NewReader(content), CreateOptions{
		MediaType: "text/plain", Expected: contentIdentity(content),
	})
	require.NoError(t, err)
	_, err = vault.RebuildDocumentEvents(t.Context(), "50000000-0000-4000-8000-000000000015")
	require.NoError(t, err)

	const operationID = "50000000-0000-4000-8000-000000000016"
	first, err := vault.RebuildDocumentPeople(t.Context(), operationID)
	require.NoError(t, err)
	require.Equal(t, "completed", first.State)
	require.Equal(t, int64(1), first.Scanned)
	require.Equal(t, int64(1), first.Published)

	coverage, err := vault.DocumentPeopleCoverage(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(1), coverage.Published)
	require.Zero(t, coverage.Pending)

	replayed, err := vault.RebuildDocumentPeople(t.Context(), operationID)
	require.NoError(t, err)
	require.Equal(t, first, replayed)
}
