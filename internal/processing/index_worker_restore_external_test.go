package processing_test

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorindex"
)

func init() {
	processing.SetTestOperationGateFactory(func() processing.TestOperationGate {
		return api.NewOperationGate()
	})
}

func TestVectorIndexRestoreRebuildsRealPublishedEmbeddings(t *testing.T) {
	for _, kind := range []document.EmbeddingInputKind{
		document.EmbeddingInputOriginalFile,
		document.EmbeddingInputRenditionChunk,
	} {
		t.Run(string(kind), func(t *testing.T) {
			fixture := processing.NewVectorIndexRestoreTestFixture(t, kind)
			processed, err := fixture.Worker.ScanOnce(t.Context())
			require.NoError(t, err)
			require.Equal(t, 1, processed)
			providerCalls := fixture.ProviderCallCount()
			spaces, err := fixture.Catalog.ListVectorIndexSpaces(t.Context())
			require.NoError(t, err)
			require.Len(t, spaces, 1)
			source, err := fixture.Catalog.CaptureVectorIndexSource(t.Context(), spaces[0])
			require.NoError(t, err)
			repo, err := backup.Init(filepath.Join(t.TempDir(), "backup"))
			require.NoError(t, err)
			_, err = backupapp.Create(
				t.Context(), repo, "test-version", fixture.Catalog, fixture.Blobs,
				backup.CreateOptions{Jobs: 1},
			)
			require.NoError(t, err)
			target := filepath.Join(t.TempDir(), "restored")
			_, err = backupapp.Restore(
				t.Context(), repo, "test-version",
				backup.RestoreOptions{TargetDir: target, Jobs: 1},
			)
			require.NoError(t, err)
			restored, err := store.Open(filepath.Join(target, "docbank.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, restored.Close()) })
			active, err := restored.ActiveVectorIndexGeneration(t.Context(), spaces[0])
			require.NoError(t, err)
			require.Equal(t, source.ManifestChecksum, active.SourceManifestChecksum)
			index, err := vectorindex.OpenGeneration(
				bytes.NewReader(active.Bytes), int64(len(active.Bytes)),
			)
			require.NoError(t, err)
			query := make([]float32, index.Metadata().Dimension)
			query[0] = 1
			hits, err := index.Search(query, 1)
			require.NoError(t, err)
			require.Len(t, hits, 1)
			require.Equal(t, source.Members[0].VectorSetID, hits[0].SetID)
			require.Equal(t, providerCalls, fixture.ProviderCallCount(),
				"restore uses retained vectors without invoking the provider")
		})
	}
}
