package loadfile

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func zipFixture(t *testing.T, names ...string) []byte {
	t.Helper()
	var data bytes.Buffer
	w := zip.NewWriter(&data)
	for _, name := range names {
		file, err := w.Create(name)
		require.NoError(t, err)
		_, err = file.Write([]byte("synthetic"))
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	return data.Bytes()
}

func TestExtractZIPConfinesPathsAndPreservesBytes(t *testing.T) {
	data := zipFixture(t, "VOL001/DATA/package.dat", "VOL001/NATIVES/a.txt")
	root, err := ExtractZIP(t.Context(), bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	got, err := os.ReadFile(filepath.Join(root, "VOL001", "NATIVES", "a.txt"))
	require.NoError(t, err)
	require.Equal(t, "synthetic", string(got))
	for _, bad := range []string{"../escape", "/absolute", "VOL001/../escape", `VOL001\escape`, "VOL001/./escape", "VOL001//escape"} {
		data := zipFixture(t, bad)
		_, err := ExtractZIP(t.Context(), bytes.NewReader(data), int64(len(data)))
		require.Error(t, err, bad)
	}
}

func TestExtractZIPRejectsDuplicateCasefoldNames(t *testing.T) {
	data := zipFixture(t, "VOL001/A.txt", "VOL001/a.TXT")
	_, err := ExtractZIP(t.Context(), bytes.NewReader(data), int64(len(data)))
	require.Error(t, err)
}

func TestRemoveExtractedZIPAcceptsTrailingSlashTempDir(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir()+string(filepath.Separator))
	data := zipFixture(t, "VOL001/DATA/package.dat")
	root, err := ExtractZIP(t.Context(), bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	require.NoError(t, RemoveExtractedZIP(root))
	_, err = os.Stat(root)
	require.ErrorIs(t, err, os.ErrNotExist)
}
