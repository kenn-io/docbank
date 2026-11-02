package pack

import (
	"bytes"
	"crypto/rand"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReaderRejectsOversized32BitWindow(t *testing.T) {
	if strconv.IntSize != 32 {
		t.Skip("32-bit decoder window limit")
	}
	// Only the header and an empty final block are needed: the window must be
	// rejected before the codec calculates an overflowing history-buffer size.
	header := zstd.Header{SingleSegment: true, HasFCS: true, FrameContentSize: math.MaxInt32}
	frame, err := header.AppendTo(nil)
	require.NoError(t, err)
	frame = append(frame, 1, 0, 0)
	dir := t.TempDir()
	writer, err := NewWriter(dir, WriterOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, writer.Abort()) })
	_, err = writer.AppendEncoded(BlobID{}, frame, math.MaxInt32, true)
	require.NoError(t, err)
	path := filepath.Join(dir, writer.ID()+".pack")
	_, err = writer.Seal(path)
	require.NoError(t, err)
	reader, err := OpenReader(path, nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, reader.Close()) })
	entry := reader.Entries()[0]

	t.Run("ReadBlob", func(t *testing.T) {
		_, err := reader.ReadBlob(entry)
		require.ErrorIs(t, err, zstd.ErrWindowSizeExceeded)
	})
	t.Run("OpenBlob", func(t *testing.T) {
		stream, err := reader.OpenBlob(t.Context(), entry)
		if stream != nil {
			t.Cleanup(func() { _ = stream.Close() })
		}
		var limitErr *StreamLimitError
		require.ErrorAs(t, err, &limitErr)
		assert.Equal(t, StreamLimitWindowBytes, limitErr.Dimension)
		assert.Equal(t, uint64(math.MaxInt32), limitErr.Actual)
		assert.Equal(t, uint64(512<<20), limitErr.Limit)
	})
}

// buildTestPack writes a pack with the given blobs and returns its final path
// and entries. crypter may be nil for a plain pack.
func buildTestPack(t *testing.T, blobs [][]byte,
	crypter *Crypter,
) (string, []Entry) {
	t.Helper()
	dir := t.TempDir()
	w, err := NewWriter(dir, WriterOptions{Crypter: crypter})
	require.NoError(t, err)
	for _, b := range blobs {
		_, err := w.Append(b)
		require.NoError(t, err)
	}
	final := filepath.Join(dir, w.ID()+".mvpack")
	entries, err := w.Seal(final)
	require.NoError(t, err)
	return final, entries
}

func testBlobs(t *testing.T) [][]byte {
	t.Helper()
	random := make([]byte, 32*1024)
	_, err := rand.Read(random)
	require.NoError(t, err)
	return [][]byte{
		bytes.Repeat([]byte("compressible text "), 2000),
		random,
		{},
		[]byte("small"),
	}
}

func TestReaderRoundTripPlain(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	blobs := testBlobs(t)
	path, wrote := buildTestPack(t, blobs, nil)

	r, err := OpenReader(path, nil)
	require.NoError(err)
	defer func() { _ = r.Close() }()

	assert.Equal(filepath.Base(path), r.ID()+".mvpack")
	require.Equal(wrote, r.Entries())
	for i, e := range r.Entries() {
		got, err := r.ReadBlob(e)
		require.NoError(err)
		assert.Equal(blobs[i], got, "blob %d", i)
		require.NoError(r.VerifyStored(e))
	}
}

func TestReaderIDIgnoresExtension(t *testing.T) {
	// OpenReader derives the pack ID from the filename minus its extension, so
	// any extension works: the same sealed pack copied under a ".mvpack" name
	// and a ".kpack" name must both open and report the same ID.
	require := require.New(t)
	assert := assert.New(t)
	path, _ := buildTestPack(t, testBlobs(t), nil)
	id := strings.TrimSuffix(filepath.Base(path), ".mvpack")
	data, err := os.ReadFile(path)
	require.NoError(err)

	for _, ext := range []string{".mvpack", ".kpack"} {
		renamed := filepath.Join(t.TempDir(), id+ext)
		require.NoError(os.WriteFile(renamed, data, 0o600))
		r, err := OpenReader(renamed, nil)
		require.NoError(err, "extension %s", ext)
		assert.Equal(id, r.ID(), "extension %s", ext)
		require.NoError(r.Close())
	}
}

func TestReaderHeaderValidation(t *testing.T) {
	require := require.New(t)
	path, _ := buildTestPack(t, testBlobs(t), nil)
	data, err := os.ReadFile(path)
	require.NoError(err)

	writeVariant := func(mutate func([]byte)) string {
		v := append([]byte(nil), data...)
		mutate(v)
		p := filepath.Join(t.TempDir(), NewPackID()+".mvpack")
		require.NoError(os.WriteFile(p, v, 0o600))
		return p
	}

	_, err = OpenReader(writeVariant(func(b []byte) { b[0] = 'X' }), nil)
	require.ErrorIs(err, ErrBadMagic)

	_, err = OpenReader(writeVariant(func(b []byte) { b[4] = 99 }), nil)
	require.ErrorIs(err, ErrUnsupportedVersion)

	_, err = OpenReader(writeVariant(func(b []byte) { b[5] = byte(packEncrypted | 1<<7) }), nil)
	require.ErrorIs(err, ErrCorrupt)
	require.ErrorContains(err, "unknown pack flags 0x81")
}

func TestReaderBlobCorruption(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	path, entries := buildTestPack(t, testBlobs(t), nil)
	data, err := os.ReadFile(path)
	require.NoError(err)

	// Flip one byte inside the first blob's stored bytes. The footer is
	// untouched, so Entries parses; the CRC catches the damage.
	data[entries[0].Offset] ^= 0x01
	corrupted := filepath.Join(t.TempDir(), filepath.Base(path))
	require.NoError(os.WriteFile(corrupted, data, 0o600))

	r, err := OpenReader(corrupted, nil)
	require.NoError(err)
	defer func() { _ = r.Close() }()
	_, err = r.ReadBlob(r.Entries()[0])
	require.ErrorIs(err, ErrCorrupt)
	require.ErrorIs(r.VerifyStored(r.Entries()[0]), ErrCorrupt)

	got, err := r.ReadBlob(r.Entries()[3])
	require.NoError(err, "other blobs remain readable")
	assert.Equal([]byte("small"), got)
}

func TestReaderRoundTripLargePack(t *testing.T) {
	// Build a several-MB pack (much larger than the footer itself) and
	// confirm opening and reading it back is still correct.
	require := require.New(t)
	assert := assert.New(t)

	var blobs [][]byte
	for range 8 {
		b := make([]byte, 512*1024)
		_, err := rand.Read(b)
		require.NoError(err)
		blobs = append(blobs, b)
	}
	path, wrote := buildTestPack(t, blobs, nil)

	info, err := os.Stat(path)
	require.NoError(err)
	require.Greater(info.Size(), int64(4<<20), "fixture must be several MB")

	r, err := OpenReader(path, nil)
	require.NoError(err)
	defer func() { _ = r.Close() }()

	require.Equal(wrote, r.Entries())
	for i, e := range r.Entries() {
		got, err := r.ReadBlob(e)
		require.NoError(err)
		assert.Equal(blobs[i], got, "blob %d", i)
	}
}

func TestReaderRejectsForgedHugeRawLen(t *testing.T) {
	// A forger who can rewrite pack bytes can also recompute the plain
	// trailer's SHA-256 over the rewritten footer region, so the footer
	// checksum passes and only the entry's RawLen is a lie. The bound is
	// enforced at footer parse time — before any blob byte is read or any
	// buffer sized from the untrusted value — for compressed and uncompressed
	// entries alike: maxStoredLen exceeds MaxRawLen by the compression/seal
	// allowances, so an uncompressed entry could otherwise claim a raw length
	// just past the documented blob limit.
	require := require.New(t)
	compressible := bytes.Repeat([]byte("forge me some zstd bytes "), 4096)
	path, entries := buildTestPack(t, [][]byte{compressible}, nil)

	for name, forge := range map[string]func(*Entry){
		"absurd":        func(e *Entry) { e.RawLen = 1 << 50 },
		"just-over-max": func(e *Entry) { e.RawLen = MaxRawLen + 1; e.Flags &^= BlobCompressed },
	} {
		forged := entries[0]
		forge(&forged)

		data, err := os.ReadFile(path)
		require.NoError(err)
		footerStart := int(entries[0].Offset + entries[0].StoredLen)
		rebuilt := append([]byte{}, data[:footerStart]...)
		rebuilt = append(rebuilt, appendPlainTrailer(encodeFooterRegion([]Entry{forged}))...)
		forgedPath := filepath.Join(t.TempDir(), filepath.Base(path))
		require.NoError(os.WriteFile(forgedPath, rebuilt, 0o600))

		_, err = OpenReader(forgedPath, nil)
		require.ErrorIs(err, ErrCorrupt, "case %s", name)
		require.ErrorContains(err, "raw length", "case %s", name)
	}
}

// TestDecodeFrameRejectsOversizedRawLen pins decodeFrame's own backstop for
// entries that never passed footer parsing: the MaxRawLen bound applies to
// uncompressed frames too, not only the zstd preallocation path.
func TestDecodeFrameRejectsOversizedRawLen(t *testing.T) {
	require := require.New(t)
	for _, compressed := range []bool{true, false} {
		_, err := decodeFrame([]byte("stored"), compressed, MaxRawLen+1)
		require.ErrorIs(err, ErrCorrupt, "compressed=%v", compressed)
		require.ErrorContains(err, "raw length", "compressed=%v", compressed)
	}
}

func TestReaderRejectsEncryptedFlagInPlainPack(t *testing.T) {
	// An entry flagged BlobEncrypted inside a pack whose trailer is plain is
	// structurally corrupt: the pack-level flag and the entry-level flag
	// disagree about whether the blob was sealed.
	require := require.New(t)
	blobs := [][]byte{[]byte("first"), []byte("second")}
	path, entries := buildTestPack(t, blobs, nil)

	flagged := append([]Entry{}, entries...)
	flagged[0].Flags |= BlobEncrypted

	data, err := os.ReadFile(path)
	require.NoError(err)
	footerStart := int(entries[len(entries)-1].Offset + entries[len(entries)-1].StoredLen)
	forged := append([]byte{}, data[:footerStart]...)
	forged = append(forged, appendPlainTrailer(encodeFooterRegion(flagged))...)
	forgedPath := filepath.Join(t.TempDir(), filepath.Base(path))
	require.NoError(os.WriteFile(forgedPath, forged, 0o600))

	r, err := OpenReader(forgedPath, nil)
	require.NoError(err)
	defer func() { _ = r.Close() }()
	_, err = r.ReadBlob(r.Entries()[0])
	require.ErrorIs(err, ErrCorrupt)
}

func TestReaderBlobHashMismatch(t *testing.T) {
	require := require.New(t)
	// A stored frame whose bytes are internally consistent (CRC recomputed to
	// match) but whose content does not hash to the entry's BlobID must fail
	// with ErrBlobMismatch. Build it by lying to the footer: swap two entries'
	// IDs after writing.
	blobs := [][]byte{[]byte("first"), []byte("second")}
	path, entries := buildTestPack(t, blobs, nil)

	swapped := []Entry{entries[0], entries[1]}
	swapped[0].ID, swapped[1].ID = swapped[1].ID, swapped[0].ID
	data, err := os.ReadFile(path)
	require.NoError(err)
	footerStart := int(entries[1].Offset + entries[1].StoredLen)
	forged := append([]byte{}, data[:footerStart]...)
	forged = append(forged,
		appendPlainTrailer(encodeFooterRegion(swapped))...)
	forgedPath := filepath.Join(t.TempDir(), filepath.Base(path))
	require.NoError(os.WriteFile(forgedPath, forged, 0o600))

	r, err := OpenReader(forgedPath, nil)
	require.NoError(err)
	defer func() { _ = r.Close() }()
	_, err = r.ReadBlob(r.Entries()[0])
	require.ErrorIs(err, ErrBlobMismatch)
}
