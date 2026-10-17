package processing

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/redactiontest"
	"go.kenn.io/kit/packstore"
)

//nolint:paralleltest // Qualified PDFium accounts for process-wide RSS.
func TestRedactionPreparationMissingOriginal(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	f := newPreparationFixture(t, "SECRET KEEP SECRET")
	layout, err := packstore.NewLayout(filepath.Join(filepath.Dir(f.databasePath), "blobs"),
		packstore.LayoutOptions{Staging: packstore.StagingStoreDirectory, StagingDir: "tmp"})
	require.NoError(t, err)
	hash, err := packstore.ParseHash(f.request.Selection.Source.SHA256)
	require.NoError(t, err)
	require.NoError(t, os.Remove(layout.LoosePath(hash)))
	result, err := f.service.PrepareNativePDF(t.Context(), f.request, runtime)
	require.ErrorIs(t, err, ErrRedactionPreparationIntegrity)
	require.Empty(t, result.Map.Text)
}
