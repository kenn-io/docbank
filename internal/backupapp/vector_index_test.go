package backupapp

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
	"path/filepath"
	"testing"
)

func TestRebuildRestoredVectorIndexesAcceptsVaultWithoutEmbeddingAuthority(t *testing.T) {
	metadata, err := store.Open(filepath.Join(t.TempDir(), "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, metadata.Close()) })
	report, err := rebuildRestoredVectorIndexes(t.Context(), metadata, nil)
	require.NoError(t, err)
	require.Empty(t, report.Rebuilt)
	require.Empty(t, report.Unavailable)
}
