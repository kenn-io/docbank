package blob

import (
	"context"
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
	"go.kenn.io/kit/packstore"
)

func newTestBlobStore(t *testing.T) *Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "tmp"), 0o700))
	store, err := newReaderStore(alwaysMemberResolver{}, dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

type alwaysMemberResolver struct{}

func (alwaysMemberResolver) Resolve(_ context.Context, _ packstore.Hash) (packstore.Location, error) {
	return packstore.Location{Member: true}, nil
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

func TestOpenStreamRequiresTerminalVerification(t *testing.T) {
	bs := newTestBlobStore(t)
	content := "stream me completely"
	hash, _, err := bs.Write(strings.NewReader(content))
	require.NoError(t, err)

	partial, size, err := bs.OpenStream(hash)
	require.NoError(t, err)
	assert.Equal(t, int64(len(content)), size)
	buf := make([]byte, 1)
	_, err = partial.Read(buf)
	require.NoError(t, err)
	assert.False(t, partial.Verified())
	require.ErrorIs(t, partial.Close(), pack.ErrVerificationIncomplete)

	complete, size, err := bs.OpenStream(hash)
	require.NoError(t, err)
	got, err := io.ReadAll(complete)
	require.NoError(t, err)
	assert.Equal(t, int64(len(got)), size)
	assert.Equal(t, content, string(got))
	assert.True(t, complete.Verified())
	require.NoError(t, complete.Close())
}

func TestOpenStreamHonorsCancellation(t *testing.T) {
	bs := newTestBlobStore(t)
	hash, _, err := bs.Write(strings.NewReader("cancel this stream"))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	stream, _, err := bs.OpenStreamContext(ctx, hash)
	require.NoError(t, err)
	cancel()
	_, err = stream.Read(make([]byte, 1))
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, stream.Close(), context.Canceled)
}

func TestOpenStreamDetectsLooseCorruption(t *testing.T) {
	bs := newTestBlobStore(t)
	content := "expected bytes"
	hash, _, err := bs.Write(strings.NewReader(content))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(bs.path(hash), []byte("corrupted data"), 0o600))

	stream, _, err := bs.OpenStream(hash)
	require.NoError(t, err)
	_, err = io.ReadAll(stream)
	require.ErrorIs(t, err, packstore.ErrContentMismatch)
	assert.False(t, stream.Verified())
	require.ErrorIs(t, stream.Close(), packstore.ErrContentMismatch)
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
	require.ErrorContains(t, err, "sync")

	// A durably-successful write reports (hash, size); on SyncDir failure
	// Write must return the zero values so no caller can mistake this write
	// for a durable success, even though the rename onto the final path
	// already happened before the fsync failed.
	assert.Empty(t, hash)
	assert.Zero(t, size)
}

func TestWriteDedupFastPathSurfacesSyncDirFailure(t *testing.T) {
	bs := newTestBlobStore(t)
	content := "dedup me"

	hash1, size1, err := bs.Write(strings.NewReader(content))
	require.NoError(t, err)

	orig := pack.SyncDir
	pack.SyncDir = func(string) error { return errors.New("boom") }
	t.Cleanup(func() { pack.SyncDir = orig })

	// Second write hits the dedup fast path (blob already exists), which
	// must still sync the shard dir and surface a failure there instead of
	// reporting durable success.
	hash2, size2, err := bs.Write(strings.NewReader(content))
	require.Error(t, err)
	require.ErrorContains(t, err, "sync")
	assert.Empty(t, hash2)
	assert.Zero(t, size2)

	// Sanity: the first write did succeed and produced the expected hash.
	sum := sha256.Sum256([]byte(content))
	assert.Equal(t, hex.EncodeToString(sum[:]), hash1)
	assert.NotZero(t, size1)
}

func TestCleanTmpRefusesSymlinkedTmpDir(t *testing.T) {
	bs := newTestBlobStore(t)
	outside := t.TempDir()
	victim := filepath.Join(outside, "keep.txt")
	require.NoError(t, os.WriteFile(victim, []byte("keep"), 0o600))

	tmp := filepath.Join(bs.dir, "tmp")
	require.NoError(t, os.RemoveAll(tmp))
	require.NoError(t, os.Symlink(outside, tmp))

	require.Error(t, bs.CleanTmp())
	_, err := os.Stat(victim)
	assert.NoError(t, err, "file behind the symlink must survive")
}

func TestRemoveSurfacesSyncDirFailure(t *testing.T) {
	bs := newTestBlobStore(t)
	hash, _, err := bs.Write(strings.NewReader("gone"))
	require.NoError(t, err)

	orig := pack.SyncDir
	pack.SyncDir = func(string) error { return errors.New("boom") }
	t.Cleanup(func() { pack.SyncDir = orig })

	// A remove whose unlink is not provably durable must fail, so gc keeps
	// the metadata row for a file that could resurface after a crash.
	err = bs.Remove(hash)
	require.Error(t, err)
	require.ErrorContains(t, err, "sync loose removal")

	// The retry finds the file already gone, but the earlier unlink was
	// never durably synced: returning success without a sync would let gc
	// delete the row above a still-volatile unlink.
	err = bs.Remove(hash)
	require.Error(t, err)
	require.ErrorContains(t, err, "sync loose removal")

	// Once syncing works the retry converges, and a blob whose shard dir
	// never existed has no entry to resurface and needs no sync.
	pack.SyncDir = orig
	require.NoError(t, bs.Remove(hash))
	require.NoError(t, bs.Remove(strings.Repeat("f", 64)))
}

func TestWriteRefusesInvalidExistingObject(t *testing.T) {
	bs := newTestBlobStore(t)
	content := "healthy bytes"
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	final := filepath.Join(bs.dir, hash[:2], hash)
	require.NoError(t, os.MkdirAll(filepath.Dir(final), 0o700))

	// A wrong-size regular file is not the content this hash promises.
	// Kit fails closed rather than replacing a path whose identity raced or
	// was tampered with.
	require.NoError(t, os.WriteFile(final, []byte("truncated"), 0o600))
	h, _, err := bs.Write(strings.NewReader(content))
	require.ErrorIs(t, err, packstore.ErrContentMismatch)
	assert.Empty(t, h)
	got, err := os.ReadFile(final)
	require.NoError(t, err)
	assert.Equal(t, "truncated", string(got))

	// A symlink (even to identical bytes) is likewise refused.
	other := filepath.Join(t.TempDir(), "elsewhere")
	require.NoError(t, os.WriteFile(other, []byte(content), 0o600))
	require.NoError(t, os.Remove(final))
	require.NoError(t, os.Symlink(other, final))
	_, _, err = bs.Write(strings.NewReader(content))
	require.ErrorIs(t, err, packstore.ErrContentMismatch)
	fi, err := os.Lstat(final)
	require.NoError(t, err)
	assert.NotZero(t, fi.Mode()&os.ModeSymlink)

	// A directory cannot be replaced by rename: hard error, never a fake
	// dedup success.
	require.NoError(t, os.Remove(final))
	require.NoError(t, os.Mkdir(final, 0o700))
	_, _, err = bs.Write(strings.NewReader(content))
	require.Error(t, err)
}
