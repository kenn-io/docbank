package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// readAllContentFiles walks a seeded content directory into a map of file
// content keyed by its SHA-256 hex digest, matching how ContentRef.Hash keys
// a mapSource. Used to build a ContentSource from a fixture's loose files
// before deleting the directory they came from.
func readAllContentFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	blobs := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		blobs[hex.EncodeToString(sum[:])] = content
		return nil
	})
	require.NoError(t, err)
	return blobs
}

// TestCreateWithContentSource pins CreateOptions.ContentSource end to end:
// when set, Create captures attachment content through the source instead of
// reading ContentDir, so a backup can run even after the live attachments
// directory is gone. Verify then proves the packs hold real, hash-valid
// content, not just that Create returned no error.
func TestCreateWithContentSource(t *testing.T) {
	require := require.New(t)

	dbPath, contentDir, dataDir, db := seedBackupFixture(t)
	src := &mapSource{blobs: readAllContentFiles(t, contentDir)}
	require.NoError(os.RemoveAll(contentDir))

	r := initTestRepo(t)
	opts := createOpts(dbPath, contentDir, dataDir, t.TempDir())
	opts.ContentSource = src

	m, err := Create(t.Context(), r, newTestApp(), opts)
	require.NoError(err)
	require.NotNil(m)
	require.Equal(int64(2), m.Attachments.Blobs)

	res, err := Verify(t.Context(), r, newTestApp(), VerifyOptions{})
	require.NoError(err)
	require.Empty(res.Problems)
	_ = db
}
