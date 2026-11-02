package embedserver

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMissingRuntimePreventsModelDownload(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "cache")
	_, err := loadModel(t.Context(), dir, filepath.Join(root, "missing-library"))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(dir)
	require.ErrorIs(t, err, os.ErrNotExist)
}
