//go:build unix

package ingest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestImportRefusesSymlinkAtOpen(t *testing.T) {
	ing := newTestIngester(t)
	ctx := t.Context()
	src := writeTree(t, map[string]string{"real.txt": "hello"})
	link := filepath.Join(src, "link.txt")
	require.NoError(t, os.Symlink(filepath.Join(src, "real.txt"), link))

	// Classification via Lstat/WalkDir can be raced by a swap, so "symlinks
	// are skipped" has to hold at the point the file is opened for reading.
	ingestID, err := ing.Store.BeginIngest(ctx, "cli", "test")
	require.NoError(t, err)
	_, err = ing.importFile(ctx, ingestID, ing.Store.RootID(), link)
	require.Error(t, err)
}
