package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func newTestBlobStore(t *testing.T) *Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "tmp"), 0o700))
	return New(dir)
}

func TestWriteAndReadBack(t *testing.T) {
	bs := newTestBlobStore(t)
	content := "hello docbank"
	wantHash := sha256.Sum256([]byte(content))
	wantHex := hex.EncodeToString(wantHash[:])

	hash, size, err := bs.Write(strings.NewReader(content))
	require.NoError(t, err)
	assert.Equal(t, wantHex, hash)
	assert.Equal(t, int64(len(content)), size)

	// Sharded path: blobs/<aa>/<hash>.
	assert.Equal(t, filepath.Join(bs.dir, wantHex[:2], wantHex), bs.path(hash))

	f, err := bs.Open(hash)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	got, err := io.ReadAll(f)
	require.NoError(t, err)
	assert.Equal(t, content, string(got))

	ok, err := bs.Exists(hash)
	require.NoError(t, err)
	assert.True(t, ok)

	// tmp dir is empty after a successful write.
	entries, err := os.ReadDir(filepath.Join(bs.dir, "tmp"))
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestWriteIsIdempotent(t *testing.T) {
	bs := newTestBlobStore(t)
	h1, _, err := bs.Write(strings.NewReader("same bytes"))
	require.NoError(t, err)
	h2, _, err := bs.Write(strings.NewReader("same bytes"))
	require.NoError(t, err)
	assert.Equal(t, h1, h2)
}

func TestRemoveAndCleanTmp(t *testing.T) {
	bs := newTestBlobStore(t)
	hash, _, err := bs.Write(strings.NewReader("bye"))
	require.NoError(t, err)

	require.NoError(t, bs.Remove(hash))
	ok, err := bs.Exists(hash)
	require.NoError(t, err)
	assert.False(t, ok)
	// Removing a missing blob is not an error (GC re-run reconciles).
	require.NoError(t, bs.Remove(hash))

	// CleanTmp clears stragglers from crashed writes.
	stray := filepath.Join(bs.dir, "tmp", "blob-123456")
	require.NoError(t, os.WriteFile(stray, []byte("partial"), 0o600))
	require.NoError(t, bs.CleanTmp())
	_, err = os.Stat(stray)
	assert.True(t, os.IsNotExist(err))
}

func TestInvalidHashRejected(t *testing.T) {
	bs := newTestBlobStore(t)

	_, err := bs.Open("short")
	require.ErrorIs(t, err, ErrInvalidHash)

	ok, err := bs.Exists("short")
	require.ErrorIs(t, err, ErrInvalidHash)
	assert.False(t, ok)

	err = bs.Remove("short")
	require.ErrorIs(t, err, ErrInvalidHash)
}

func TestWriteSurfacesSyncDirFailure(t *testing.T) {
	bs := newTestBlobStore(t)

	// Pre-create the destination shard dir so MkdirAllSynced's fast path
	// (dir already exists) short-circuits without calling SyncDir itself;
	// otherwise the swapped SyncDir below would fail inside MkdirAllSynced
	// instead of at Write's own post-rename sync.
	content := "x"
	sum := sha256.Sum256([]byte(content))
	wantHash := hex.EncodeToString(sum[:])
	require.NoError(t, os.MkdirAll(filepath.Join(bs.dir, wantHash[:2]), 0o700))

	orig := pack.SyncDir
	pack.SyncDir = func(string) error { return errors.New("boom") }
	t.Cleanup(func() { pack.SyncDir = orig })

	hash, size, err := bs.Write(strings.NewReader(content))
	require.Error(t, err)
	require.ErrorContains(t, err, "syncing blob shard dir")

	// A durably-successful write reports (hash, size); on SyncDir failure
	// Write must return the zero values so no caller can mistake this write
	// for a durable success, even though the rename onto the final path
	// already happened before the fsync failed.
	assert.Empty(t, hash)
	assert.Zero(t, size)
}
