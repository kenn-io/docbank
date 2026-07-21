package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
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
	return newTestBlobStoreWithOptions(t, Options{})
}

func newTestBlobStoreWithOptions(t *testing.T, opts Options) *Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "tmp"), 0o700))
	store, err := newReaderStoreWithOptions(alwaysMemberResolver{}, dir, opts)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func testLooseCompression() LooseCompressionOptions {
	return LooseCompressionOptions{
		Enabled:           true,
		MinBytes:          1024,
		MinSavingsPercent: 10,
	}
}

func TestCompressedWriteReceiptAndVerifiedRoundTrip(t *testing.T) {
	bs := newTestBlobStoreWithOptions(t, Options{LooseCompression: testLooseCompression()})
	content := []byte(strings.Repeat("{\"message\":\"compress me\"}\n", 512))
	wantHash := sha256.Sum256(content)
	wantHex := hex.EncodeToString(wantHash[:])

	receipt, err := bs.WriteDetailedContext(t.Context(), bytes.NewReader(content))
	require.NoError(t, err)
	assert.Equal(t, wantHex, receipt.Hash)
	assert.Equal(t, int64(len(content)), receipt.Size)
	assert.Equal(t, packstore.LooseEncodingZstd, receipt.Encoding)
	assert.Less(t, receipt.StoredSize, receipt.Size)
	assert.True(t, receipt.Created)
	assert.True(t, receipt.PackEligible)

	_, err = os.Stat(bs.path(receipt.Hash))
	require.ErrorIs(t, err, fs.ErrNotExist)
	info, err := os.Stat(bs.compressedPath(receipt.Hash))
	require.NoError(t, err)
	assert.Equal(t, receipt.StoredSize, info.Size())

	assertVerifiedBlob(t, bs, receipt.Hash, content)

	listed, err := bs.List()
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{receipt.Hash: receipt.StoredSize}, listed)
}

func TestCompressedWriteKeepsSmallObjectRaw(t *testing.T) {
	bs := newTestBlobStoreWithOptions(t, Options{LooseCompression: testLooseCompression()})
	content := []byte("small but repetitive")

	receipt, err := bs.WriteDetailedContext(t.Context(), bytes.NewReader(content))
	require.NoError(t, err)
	wantHash := sha256.Sum256(content)
	assert.Equal(t, hex.EncodeToString(wantHash[:]), receipt.Hash)
	assert.Equal(t, int64(len(content)), receipt.Size)
	assert.Equal(t, packstore.LooseEncodingRaw, receipt.Encoding)
	assert.Equal(t, receipt.Size, receipt.StoredSize)
	require.FileExists(t, bs.path(receipt.Hash))
	require.NoFileExists(t, bs.compressedPath(receipt.Hash))
	assertVerifiedBlob(t, bs, receipt.Hash, content)
}

func TestCompressedWriteKeepsIncompressibleObjectRaw(t *testing.T) {
	bs := newTestBlobStoreWithOptions(t, Options{LooseCompression: testLooseCompression()})
	content := deterministicIncompressibleBytes(8 << 10)

	receipt, err := bs.WriteDetailedContext(t.Context(), bytes.NewReader(content))
	require.NoError(t, err)
	wantHash := sha256.Sum256(content)
	assert.Equal(t, hex.EncodeToString(wantHash[:]), receipt.Hash)
	assert.Equal(t, int64(len(content)), receipt.Size)
	assert.Equal(t, packstore.LooseEncodingRaw, receipt.Encoding)
	assert.Equal(t, receipt.Size, receipt.StoredSize)
	require.FileExists(t, bs.path(receipt.Hash))
	require.NoFileExists(t, bs.compressedPath(receipt.Hash))
	assertVerifiedBlob(t, bs, receipt.Hash, content)
}

func TestLegacyRawLooseObjectRemainsVerifiedReadable(t *testing.T) {
	bs := newTestBlobStoreWithOptions(t, Options{LooseCompression: testLooseCompression()})
	content := []byte(strings.Repeat("legacy raw content\n", 256))
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	rawPath := bs.path(hash)
	require.NoError(t, os.MkdirAll(filepath.Dir(rawPath), 0o700))
	require.NoError(t, os.WriteFile(rawPath, content, 0o600))

	assertVerifiedBlob(t, bs, hash, content)

	listed, err := bs.List()
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{hash: int64(len(content))}, listed)
}

func assertVerifiedBlob(t *testing.T, bs *Store, hash string, want []byte) {
	t.Helper()
	stream, logicalSize, err := bs.OpenStream(hash)
	require.NoError(t, err)
	got, err := io.ReadAll(stream)
	require.NoError(t, err)
	assert.Equal(t, int64(len(want)), logicalSize)
	assert.Equal(t, want, got)
	assert.True(t, stream.Verified())
	require.NoError(t, stream.Close())
}

func deterministicIncompressibleBytes(size int) []byte {
	content := make([]byte, 0, size)
	for counter := uint64(0); len(content) < size; counter++ {
		var input [8]byte
		binary.LittleEndian.PutUint64(input[:], counter)
		digest := sha256.Sum256(input[:])
		content = append(content, digest[:]...)
	}
	return content[:size]
}

func TestStoragePolicyKeepsBlobLimitExplicit(t *testing.T) {
	assert.Equal(t, MaxIngestBytes, int64(1<<32))
	assert.Equal(t, MaxPackedBlobBytes, int64(64<<20))
	assert.Equal(t, MaxPackedBlobBytes, StorageLimits().BlobBytes)
}

func TestWriteAcceptsLooseBlobAbovePackingLimit(t *testing.T) {
	bs := newTestBlobStore(t)
	wantSize := MaxPackedBlobBytes + 1
	receipt, err := bs.WriteDetailedContext(t.Context(), io.LimitReader(zeroReader{}, wantSize))
	require.NoError(t, err)
	assert.Equal(t, wantSize, receipt.Size)
	assert.False(t, receipt.PackEligible)

	stream, gotSize, err := bs.OpenStream(receipt.Hash)
	require.NoError(t, err)
	assert.Equal(t, wantSize, gotSize)
	written, err := io.Copy(io.Discard, stream)
	require.NoError(t, err)
	assert.Equal(t, wantSize, written)
	assert.True(t, stream.Verified())
	require.NoError(t, stream.Close())
}

type alwaysMemberResolver struct{}

func (alwaysMemberResolver) Resolve(_ context.Context, _ packstore.Hash) (packstore.Location, error) {
	return packstore.Location{Member: true}, nil
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
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
