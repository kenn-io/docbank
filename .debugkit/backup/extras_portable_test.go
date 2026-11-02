package backup_test

import (
	"bytes"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
)

func TestPortableBackupChunkedExtras(t *testing.T) {
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	extraPath := filepath.Join(t.TempDir(), "history.db")
	file, err := os.Create(extraPath)
	require.NoError(t, err)
	// Repeated bytes compress quickly, but the last byte distinguishes the end.
	digest := sha256.New()
	writer := io.MultiWriter(file, digest)
	chunk := bytes.Repeat([]byte("data"), 1<<18)
	for range 64 {
		_, err = writer.Write(chunk)
		require.NoError(t, err)
	}
	_, err = writer.Write([]byte("!"))
	require.NoError(t, err)
	require.NoError(t, file.Close())
	source := &portableSource{
		raw:   []byte(`{"notes":["snapshot"],"files":[]}`),
		stats: []byte(`{"notes":1,"files":0}`), info: &backup.ContentInfo{},
	}
	manifest, err := backup.Create(t.Context(), repo, portableApp{}, backup.CreateOptions{
		MetadataSource: source,
		Extras:         backup.ExtrasSpec{Files: []backup.ExtrasFileSpec{{Path: extraPath, RecordAs: "history.db"}}},
	})
	require.NoError(t, err)
	require.Equal(t, 6, manifest.MinReaderVersion)
	verified, err := backup.Verify(t.Context(), repo, portableApp{}, backup.VerifyOptions{SnapshotID: manifest.SnapshotID})
	require.NoError(t, err)
	require.Empty(t, verified.Problems)
	target := filepath.Join(t.TempDir(), "restore")
	_, err = backup.Restore(t.Context(), repo, portableApp{}, backup.RestoreOptions{
		SnapshotID: manifest.SnapshotID, TargetDir: target, MetadataRestorer: portableRestorer{},
	})
	require.NoError(t, err)
	restored, err := os.Open(filepath.Join(target, "history.db"))
	require.NoError(t, err)
	defer func() { _ = restored.Close() }()
	got := sha256.New()
	n, err := io.Copy(got, restored)
	require.NoError(t, err)
	require.Equal(t, int64(64<<20+1), n)
	require.Equal(t, digest.Sum(nil), got.Sum(nil))
}
