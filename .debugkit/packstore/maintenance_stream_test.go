package packstore

import (
	"io"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func TestPackAndUnpackStreamAboveFormerCeiling(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	if testing.Short() {
		t.Skip("streams an object above the former 64 MiB maintenance ceiling")
	}
	size := largeStoreStreamTestBytes(t, 64<<20+1)
	layout := layoutForStoreTest(t)
	loose, err := NewLooseStore(layout)
	require.NoError(err)
	written, err := loose.Write(t.Context(), io.LimitReader(streamZeroReader{}, size), WriteOptions{
		Durability: AtomicPublication, Dedup: VerifyFullHash, MaxBytes: size,
	})
	require.NoError(err)
	require.Equal(size, written.Size)

	catalog := newMaintenanceCatalog()
	catalog.addLoose(written.Hash, written.Path)
	limits := DefaultLimits()
	limits.BlobBytes = size
	maintainer := newMaintainerForTest(t, catalog, layout, limits)

	packed, err := maintainer.Pack(t.Context(), PackOptions{})
	require.NoError(err)
	assert.Equal(1, packed.BlobsPacked)
	assert.Equal(size, packed.BytesPacked)
	assert.NoFileExists(written.Path)

	unpacked, err := maintainer.Unpack(t.Context())
	require.NoError(err)
	assert.Equal(1, unpacked.BlobsRestored)
	assert.Equal(size, unpacked.BytesRestored)
	info, err := os.Stat(written.Path)
	require.NoError(err)
	assert.Equal(size, info.Size())
	err = verifyLoosePath(t.Context(), written.Path, written.Hash, size)
	require.NoError(err)
}

func TestPackCompressedLooseStreamAboveFormerCeiling(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	if testing.Short() {
		t.Skip("streams a compressed object above the former 64 MiB maintenance ceiling")
	}
	size := largeStoreStreamTestBytes(t, 64<<20+1)
	layout := layoutForStoreTest(t)
	loose, err := NewLooseStore(layout)
	require.NoError(err)
	written, err := loose.Write(t.Context(), io.LimitReader(streamZeroReader{}, size), WriteOptions{
		Durability:  AtomicPublication,
		Dedup:       VerifyFullHash,
		MaxBytes:    size,
		Compression: LooseCompressionOptions{Enabled: true},
	})
	require.NoError(err)
	require.Equal(LooseEncodingZstd, written.Encoding)
	require.Less(written.StoredSize, written.Size)

	catalog := newMaintenanceCatalog()
	addMaintenanceCandidate(catalog, Candidate{
		Hash: written.Hash, Paths: []string{written.Path}, Size: written.Size,
	})
	limits := DefaultLimits()
	limits.BlobBytes = size
	maintainer := newMaintainerForTest(t, catalog, layout, limits)

	stats, err := maintainer.Pack(t.Context(), PackOptions{})

	require.NoError(err)
	assert.Equal(1, stats.BlobsPacked)
	assert.Equal(size, stats.BytesPacked)
	assert.NoFileExists(written.Path)
	stream, gotSize, err := maintainer.store.OpenStream(t.Context(), written.Hash)
	require.NoError(err)
	assert.Equal(size, gotSize)
	require.NoError(stream.Verify())
	require.NoError(stream.Close())
}

func TestRepackStreamsAboveFormerCeiling(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	if testing.Short() {
		t.Skip("streams an object above the former 64 MiB maintenance ceiling")
	}
	size := largeStoreStreamTestBytes(t, 64<<20+1)
	layout := layoutForStoreTest(t)
	require.NoError(os.MkdirAll(layout.PacksDir(), 0o700))
	writer, err := pack.NewWriter(layout.PacksDir(), pack.WriterOptions{})
	require.NoError(err)
	live, err := writer.AppendStream(t.Context(), io.LimitReader(streamZeroReader{}, size), uint64(size), pack.AppendStreamOptions{
		ScratchDir: layout.PacksDir(),
	})
	require.NoError(err)
	_, err = writer.Append([]byte("dead-one"))
	require.NoError(err)
	_, err = writer.Append([]byte("dead-two"))
	require.NoError(err)
	packID := writer.ID()
	entries, err := writer.Seal(layout.PackPath(packID))
	require.NoError(err)

	indexed := indexFromPack(packID, live)
	catalog := newMaintenanceCatalog()
	catalog.members[indexed.Hash] = Reference{Hash: indexed.Hash}
	catalog.entries[indexed.Hash] = indexed
	record := PackRecord{PackID: packID, EntryCount: int64(len(entries)), CreatedAt: time.Now().Add(-time.Hour)}
	for _, entry := range entries {
		record.StoredBytes += int64(entry.StoredLen)
	}
	catalog.packs[packID] = record
	limits := DefaultLimits()
	limits.BlobBytes = size
	maintainer := newMaintainerForTest(t, catalog, layout, limits)

	stats, err := maintainer.Repack(t.Context(), RepackOptions{
		Now: time.Now(), Selection: RepackSelection{MinAge: time.Nanosecond, MinDeadStored: 1},
	})
	require.NoError(err)
	assert.Equal(1, stats.BlobsRepacked)
	assert.Equal(size, stats.BytesRepacked)
	assert.NoFileExists(layout.PackPath(packID))
	stream, gotSize, err := maintainer.store.OpenStream(t.Context(), indexed.Hash)
	require.NoError(err)
	assert.Equal(size, gotSize)
	require.NoError(stream.Verify())
	require.NoError(stream.Close())
}
