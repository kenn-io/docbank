package packstore

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func TestStoreOpenStreamLoosePackedParity(t *testing.T) {
	content := bytes.Repeat([]byte("stream parity "), 1<<14)
	for _, representation := range []string{"loose", "compressed", "packed"} {
		t.Run(representation, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			store, hash := streamStoreForTest(t, representation, content)
			stream, size, err := store.OpenStream(t.Context(), hash)
			require.NoError(err)
			assert.Equal(int64(len(content)), size)
			prefix := make([]byte, 17)
			_, err = io.ReadFull(stream, prefix)
			require.NoError(err)
			assert.False(stream.Verified())
			rest, err := io.ReadAll(stream)
			require.NoError(err)
			assert.Equal(content, slices.Concat(prefix, rest))
			assert.True(stream.Verified())
			require.NoError(stream.Verify())
			require.NoError(stream.Close())
			require.NoError(stream.Close())
		})
	}
}

func TestStoreStreamsLooseObjectAboveMaintenanceLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := bytes.Repeat([]byte("oversized loose content "), 16)
	layout := layoutForStoreTest(t)
	hash := hashForTest(content)
	require.NoError(os.MkdirAll(filepath.Dir(layout.LoosePath(hash)), 0o700))
	require.NoError(os.WriteFile(layout.LoosePath(hash), content, 0o600))
	limits := DefaultLimits()
	limits.BlobBytes = int64(len(content) - 1)
	store, err := NewStore(&mapResolver{locations: map[Hash]Location{
		hash: {Member: true},
	}}, layout, StoreOptions{Limits: limits})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(store.Close()) })

	stream, size, err := store.OpenStream(t.Context(), hash)
	require.NoError(err)
	assert.Equal(int64(len(content)), size)
	got, err := io.ReadAll(stream)
	require.NoError(err)
	assert.Equal(content, got)
	require.NoError(stream.Close())

	var copied bytes.Buffer
	written, err := store.CopyVerified(t.Context(), hash, &copied)
	require.NoError(err)
	assert.Equal(int64(len(content)), written)
	assert.Equal(content, copied.Bytes())

	_, _, err = store.ReadBounded(t.Context(), hash, limits.BlobBytes)
	var limitErr *LimitError
	require.ErrorAs(err, &limitErr)
	assert.Equal(LimitBlobRawBytes, limitErr.Dimension)
}

func TestStoreOpenStreamEarlyCloseLoosePackedParity(t *testing.T) {
	content := []byte("early close content")
	for _, representation := range []string{"loose", "compressed", "packed"} {
		t.Run(representation, func(t *testing.T) {
			require := require.New(t)
			store, hash := streamStoreForTest(t, representation, content)
			stream, _, err := store.OpenStream(t.Context(), hash)
			require.NoError(err)
			buf := make([]byte, 2)
			_, err = stream.Read(buf)
			require.NoError(err)
			require.ErrorIs(stream.Close(), pack.ErrVerificationIncomplete)
			require.ErrorIs(stream.Close(), pack.ErrVerificationIncomplete)
			assert.False(t, stream.Verified())
			assertPackedLeases(t, store, 0)
		})
	}
}

func TestStoreRejectsUnknownPackFlags(t *testing.T) {
	tests := []struct {
		name string
		read func(*Store, Hash) error
	}{
		{
			name: "bounded",
			read: func(store *Store, hash Hash) error {
				_, _, err := store.ReadBounded(t.Context(), hash, 1<<20)
				return err
			},
		},
		{
			name: "streaming",
			read: func(store *Store, hash Hash) error {
				stream, _, err := store.OpenStream(t.Context(), hash)
				if err != nil {
					return err
				}
				return errors.Join(stream.Verify(), stream.Close())
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			layout := layoutForStoreTest(t)
			entry := buildStoreTestPack(t, layout, []byte("unknown pack flags"))
			f, err := os.OpenFile(layout.PackPath(entry.PackID), os.O_WRONLY, 0)
			require.NoError(err)
			_, err = f.WriteAt([]byte{0x80}, 5)
			require.NoError(err)
			require.NoError(f.Close())
			store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{
				entry.Hash: {Member: true, Pack: &entry},
			}}, layout)

			err = tt.read(store, entry.Hash)
			require.ErrorIs(err, pack.ErrCorrupt)
			require.ErrorContains(err, "unknown pack flags 0x80")
		})
	}
}

func TestStoreRejectsUnknownFlagsOnUnselectedEntry(t *testing.T) {
	tests := []struct {
		name string
		read func(*Store, Hash) error
	}{
		{
			name: "bounded",
			read: func(store *Store, hash Hash) error {
				_, _, err := store.ReadBounded(t.Context(), hash, 1<<20)
				return err
			},
		},
		{
			name: "streaming",
			read: func(store *Store, hash Hash) error {
				stream, _, err := store.OpenStream(t.Context(), hash)
				if err != nil {
					return err
				}
				return errors.Join(stream.Verify(), stream.Close())
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			layout := layoutForStoreTest(t)
			staging := t.TempDir()
			writer, err := pack.NewWriter(staging, pack.WriterOptions{})
			require.NoError(err)
			selected, err := writer.Append([]byte("selected entry"))
			require.NoError(err)
			_, err = writer.Append([]byte("unselected entry"))
			require.NoError(err)
			packID := writer.ID()
			path := layout.PackPath(packID)
			require.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
			_, err = writer.Seal(path)
			require.NoError(err)
			mutateImportFooterEntry(t, path, 1, func(entry []byte) { entry[56] |= 0x80 })

			hash, err := ParseHash(selected.ID.String())
			require.NoError(err)
			indexed := IndexEntry{
				Hash: hash, PackID: packID, Offset: int64(selected.Offset),
				StoredLen: int64(selected.StoredLen), RawLen: int64(selected.RawLen),
				Flags: uint8(selected.Flags), CRC32C: selected.CRC32C,
			}
			store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{
				hash: {Member: true, Pack: &indexed},
			}}, layout)

			err = tt.read(store, hash)
			require.ErrorIs(err, pack.ErrCorrupt)
			require.ErrorContains(err, "entry 1 has unknown flags 0x80")
		})
	}
}

func TestStoreCopyVerifiedLoosePackedParity(t *testing.T) {
	content := bytes.Repeat([]byte("verified copy "), 2048)
	for _, representation := range []string{"loose", "compressed", "packed"} {
		t.Run(representation, func(t *testing.T) {
			store, hash := streamStoreForTest(t, representation, content)
			var dst bytes.Buffer
			written, err := store.CopyVerified(t.Context(), hash, &dst)
			require.NoError(t, err)
			assert.Equal(t, int64(len(content)), written)
			assert.Equal(t, content, dst.Bytes())
			assertPackedLeases(t, store, 0)
		})
	}
}

func TestStoreCopyVerifiedDestinationFailureReleasesSource(t *testing.T) {
	content := bytes.Repeat([]byte("destination failure "), 2048)
	store, hash := streamStoreForTest(t, "packed", content)
	destinationErr := errors.New("destination failed")
	dst := &failAfterWriter{remaining: 32, err: destinationErr}
	written, err := store.CopyVerified(t.Context(), hash, dst)
	require.ErrorIs(t, err, destinationErr)
	assert.Equal(t, int64(32), written)
	assertPackedLeases(t, store, 0)
}

type failAfterWriter struct {
	remaining int
	err       error
}

func (w *failAfterWriter) Write(p []byte) (int, error) {
	if w.remaining == 0 {
		return 0, w.err
	}
	n := min(len(p), w.remaining)
	w.remaining -= n
	if n < len(p) {
		return n, w.err
	}
	return n, nil
}

func TestStoreOpenStreamTerminalIntegrityErrors(t *testing.T) {
	content := []byte("terminal integrity content")
	tests := []struct {
		name           string
		representation string
		want           error
	}{
		{name: "loose", representation: "loose", want: ErrContentMismatch},
		{name: "packed", representation: "packed", want: pack.ErrCorrupt},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			layout := layoutForStoreTest(t)
			hash := hashForTest(content)
			var entry IndexEntry
			if tt.representation == "loose" {
				require.NoError(os.MkdirAll(filepath.Dir(layout.LoosePath(hash)), 0o700))
				corrupt := append([]byte(nil), content...)
				corrupt[0] ^= 0xff
				require.NoError(os.WriteFile(layout.LoosePath(hash), corrupt, 0o600))
			} else {
				entry = buildStoreTestPack(t, layout, content)
				hash = entry.Hash
				f, err := os.OpenFile(layout.PackPath(entry.PackID), os.O_RDWR, 0)
				require.NoError(err)
				_, err = f.WriteAt([]byte{'X'}, entry.Offset)
				require.NoError(err)
				require.NoError(f.Close())
			}
			location := Location{Member: true}
			if tt.representation == "packed" {
				location.Pack = &entry
			}
			store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{hash: location}}, layout)
			stream, _, err := store.OpenStream(t.Context(), hash)
			require.NoError(err)
			got, err := io.ReadAll(stream)
			require.ErrorIs(err, tt.want)
			assert.Len(got, len(content))
			assert.False(stream.Verified())
			require.ErrorIs(stream.Verify(), tt.want)
			require.ErrorIs(stream.Close(), tt.want)
			assertPackedLeases(t, store, 0)
		})
	}
}

func TestStoreOpenStreamCancellationReleasesPackedLease(t *testing.T) {
	require := require.New(t)
	content := bytes.Repeat([]byte("cancel packed stream "), 4096)
	store, hash := streamStoreForTest(t, "packed", content)
	ctx, cancel := context.WithCancel(t.Context())
	stream, _, err := store.OpenStream(ctx, hash)
	require.NoError(err)
	buf := make([]byte, 32)
	_, err = stream.Read(buf)
	require.NoError(err)
	cancel()
	_, err = stream.Read(buf)
	require.ErrorIs(err, context.Canceled)
	assertPackedLeases(t, store, 0)
	require.ErrorIs(stream.Close(), context.Canceled)
}

func TestStoreOpenStreamCancellationClosesCompressedLoose(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := bytes.Repeat([]byte("cancel compressed stream "), 4096)
	store, hash := streamStoreForTest(t, "compressed", content)
	originalReader := newLooseZstdReader
	closeCalls := 0
	newLooseZstdReader = func(src io.Reader) (looseZstdReader, error) {
		reader, err := originalReader(src)
		if err != nil {
			return nil, err
		}
		return &closeCountingLooseZstdReader{looseZstdReader: reader, closeCalls: &closeCalls}, nil
	}
	t.Cleanup(func() { newLooseZstdReader = originalReader })
	ctx, cancel := context.WithCancel(t.Context())
	stream, _, err := store.OpenStream(ctx, hash)
	require.NoError(err)
	physical := stream.(*looseVerifiedStream).object.file
	buf := make([]byte, 32)
	_, err = stream.Read(buf)
	require.NoError(err)
	cancel()
	_, err = stream.Read(buf)
	require.ErrorIs(err, context.Canceled)
	require.ErrorIs(stream.Close(), context.Canceled)
	assert.False(stream.Verified())
	assert.Equal(1, closeCalls)
	_, err = physical.Read(make([]byte, 1))
	require.ErrorIs(err, os.ErrClosed)
}

func TestStoreOpenStreamChecksCancellationBetweenCompressedPayloadReads(t *testing.T) {
	content := bytes.Repeat([]byte("cancel within compressed payload "), 128)
	store, hash := streamStoreForTest(t, "compressed", content)
	ctx, cancel := context.WithCancel(t.Context())
	originalReader := newLooseZstdReader
	newLooseZstdReader = func(src io.Reader) (looseZstdReader, error) {
		return &cancelBetweenSourceReadsDecoder{
			source: src,
			cancel: cancel,
		}, nil
	}
	t.Cleanup(func() { newLooseZstdReader = originalReader })
	stream, _, err := store.OpenStream(ctx, hash)
	require.NoError(t, err)

	_, err = stream.Read(make([]byte, 1))

	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, stream.Close(), context.Canceled)
}

var errCompressedSourceCancellationMissed = errors.New("compressed source missed cancellation")

type cancelBetweenSourceReadsDecoder struct {
	source io.Reader
	cancel context.CancelFunc
}

func (r *cancelBetweenSourceReadsDecoder) Read([]byte) (int, error) {
	var one [1]byte
	if _, err := r.source.Read(one[:]); err != nil {
		return 0, err
	}
	r.cancel()
	if _, err := r.source.Read(one[:]); err != nil {
		return 0, err
	}
	return 0, errCompressedSourceCancellationMissed
}

func (*cancelBetweenSourceReadsDecoder) Close() {}

type closeCountingLooseZstdReader struct {
	looseZstdReader
	closeCalls *int
}

func (r *closeCountingLooseZstdReader) Close() {
	*r.closeCalls++
	r.looseZstdReader.Close()
}

func TestStoreOpenStreamRejectsCompressedLooseIntegrityFailures(t *testing.T) {
	content := bytes.Repeat([]byte("compressed integrity "), 1024)
	var emptyFrame bytes.Buffer
	emptyEncoder, err := zstd.NewWriter(&emptyFrame, zstd.WithEncoderConcurrency(1))
	require.NoError(t, err)
	require.NoError(t, emptyEncoder.Close())
	emptySkippableFrame := []byte{0x50, 0x2a, 0x4d, 0x18, 0, 0, 0, 0}
	tests := []struct {
		name        string
		logicalSize int64
		decoded     []byte
		mutate      func([]byte) []byte
	}{
		{
			name:        "truncated zstd",
			logicalSize: int64(len(content)),
			decoded:     content,
			mutate: func(physical []byte) []byte {
				return physical[:len(physical)-3]
			},
		},
		{
			name:        "trailing decoded data",
			logicalSize: int64(len(content)),
			decoded:     append(bytes.Clone(content), []byte("extra")...),
		},
		{
			name:        "header size exceeds decoded size",
			logicalSize: int64(len(content) + 1),
			decoded:     content,
		},
		{
			name:        "wrong digest",
			logicalSize: int64(len(content)),
			decoded:     bytes.Repeat([]byte{'x'}, len(content)),
		},
		{
			name:        "trailing physical data",
			logicalSize: int64(len(content)),
			decoded:     content,
			mutate: func(physical []byte) []byte {
				return append(physical, []byte("not a zstd frame")...)
			},
		},
		{
			name:        "trailing empty zstd frame",
			logicalSize: int64(len(content)),
			decoded:     content,
			mutate: func(physical []byte) []byte {
				return append(physical, emptyFrame.Bytes()...)
			},
		},
		{
			name:        "trailing empty skippable frame",
			logicalSize: int64(len(content)),
			decoded:     content,
			mutate: func(physical []byte) []byte {
				return append(physical, emptySkippableFrame...)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			layout := layoutForStoreTest(t)
			hash := hashForTest(content)
			writeCompressedLooseFixture(t, layout, hash, tt.logicalSize, tt.decoded, tt.mutate)
			store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{
				hash: {Member: true},
			}}, layout)

			stream, size, err := store.OpenStream(t.Context(), hash)
			require.NoError(err)
			assert.Equal(tt.logicalSize, size)
			err = stream.Verify()
			require.ErrorIs(err, ErrContentMismatch)
			require.ErrorIs(stream.Close(), ErrContentMismatch)
			assert.False(stream.Verified())
		})
	}
}

func TestStoreOpenStreamRejectsCompressedLooseGrowthAfterOpen(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := bytes.Repeat([]byte("compressed growth after open "), 1024)
	layout := layoutForStoreTest(t)
	hash := hashForTest(content)
	writeCompressedLooseFixture(t, layout, hash, int64(len(content)), content, nil)
	store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{
		hash: {Member: true},
	}}, layout)
	stream, size, err := store.OpenStream(t.Context(), hash)
	require.NoError(err)
	assert.Equal(int64(len(content)), size)
	appendFile, err := os.OpenFile(layout.CompressedLoosePath(hash), os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(err)
	_, err = appendFile.WriteString("trailing physical mutation")
	require.NoError(err)
	require.NoError(appendFile.Close())

	err = stream.Verify()

	require.ErrorIs(err, ErrContentMismatch)
	require.ErrorIs(stream.Close(), ErrContentMismatch)
	assert.False(stream.Verified())
}

func TestStoreOpenStreamRejectsSkippableFrameAfterSingleSegmentRawBlock(t *testing.T) {
	require := require.New(t)
	content := []byte("12345678")
	frame := []byte{
		0x28, 0xb5, 0x2f, 0xfd, // zstd magic
		0x20, byte(len(content)), // single-segment descriptor and content size
		byte(1 | len(content)<<3), 0, 0, // last raw block
	}
	frame = append(frame, content...)
	frame = append(frame, 0x50, 0x2a, 0x4d, 0x18, 0, 0, 0, 0)

	layout := layoutForStoreTest(t)
	hash := hashForTest(content)
	writeCompressedLooseFixture(t, layout, hash, int64(len(content)), content, func(physical []byte) []byte {
		return append(bytes.Clone(physical[:compressedLooseHeaderSize]), frame...)
	})
	store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{
		hash: {Member: true},
	}}, layout)

	stream, size, err := store.OpenStream(t.Context(), hash)
	require.NoError(err)
	assert.Equal(t, int64(len(content)), size)
	require.ErrorIs(stream.Verify(), ErrContentMismatch)
	require.ErrorIs(stream.Close(), ErrContentMismatch)
}

func TestSingleZstdFrameReaderLeavesConcatenatedFrameUnread(t *testing.T) {
	content := []byte("12345678")
	rawFrame := []byte{
		0x28, 0xb5, 0x2f, 0xfd,
		0x20, byte(len(content)),
		byte(1 | len(content)<<3), 0, 0,
	}
	rawFrame = append(rawFrame, content...)
	rleFrame := []byte{
		0x28, 0xb5, 0x2f, 0xfd,
		0x20, byte(len(content)),
		byte(1 | 1<<1 | len(content)<<3), 0, 0,
		content[0],
	}
	encode := func(t *testing.T, opts ...zstd.EOption) []byte {
		t.Helper()
		require := require.New(t)
		t.Helper()
		encoder, err := zstd.NewWriter(nil, opts...)
		require.NoError(err)
		frame := encoder.EncodeAll(bytes.Repeat([]byte("compressible block content "), 128), nil)
		require.NoError(encoder.Close())
		var header zstd.Header
		require.NoError(header.Decode(frame))
		require.True(header.FirstBlock.Compressed)
		return frame
	}
	tests := []struct {
		name  string
		frame []byte
	}{
		{name: "single segment raw block", frame: rawFrame},
		{name: "single segment RLE block", frame: rleFrame},
		{
			name:  "single segment compressed block with checksum",
			frame: encode(t, zstd.WithSingleSegment(true), zstd.WithEncoderCRC(true)),
		},
		{
			name:  "windowed compressed block without checksum",
			frame: encode(t, zstd.WithSingleSegment(false), zstd.WithEncoderCRC(false)),
		},
	}
	skippable := []byte{0x50, 0x2a, 0x4d, 0x18, 0, 0, 0, 0}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			physical := append(bytes.Clone(tt.frame), skippable...)
			source := &io.LimitedReader{R: bytes.NewReader(physical), N: int64(len(physical))}

			got, err := io.ReadAll(newSingleZstdFrameReader(source))
			require.NoError(t, err)
			assert.Equal(t, tt.frame, got)
			assert.Equal(t, int64(len(skippable)), source.N)
		})
	}
}

func TestSingleZstdFrameReaderLeavesTrailingFrameAfterMaximalHeader(t *testing.T) {
	content := []byte("maximal header")
	frame := []byte{
		0x28, 0xb5, 0x2f, 0xfd, // zstd magic
		0xc3,       // windowed, 4-byte dictionary ID, 8-byte content size
		0,          // window descriptor
		0, 0, 0, 0, // zero dictionary ID
	}
	frame = binary.LittleEndian.AppendUint64(frame, uint64(len(content)))
	frame = append(frame,
		byte(1|len(content)<<3), 0, 0, // last raw block
	)
	frame = append(frame, content...)
	skippable := []byte{0x50, 0x2a, 0x4d, 0x18, 0, 0, 0, 0}
	physical := append(bytes.Clone(frame), skippable...)
	source := &io.LimitedReader{R: bytes.NewReader(physical), N: int64(len(physical))}

	got, err := io.ReadAll(newSingleZstdFrameReader(source))

	require.NoError(t, err)
	assert.Equal(t, frame, got)
	assert.Equal(t, int64(len(skippable)), source.N)
}

func TestStoreOpenStreamRejectsMalformedCompressedLooseHeader(t *testing.T) {
	require := require.New(t)
	content := []byte("malformed compressed header")
	layout := layoutForStoreTest(t)
	hash := hashForTest(content)
	path := layout.CompressedLoosePath(hash)
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(os.WriteFile(path, []byte("short header"), 0o600))
	require.NoError(os.WriteFile(layout.LoosePath(hash), content, 0o600))
	store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{
		hash: {Member: true},
	}}, layout)

	stream, _, err := store.OpenStream(t.Context(), hash)
	require.ErrorIs(err, ErrContentMismatch)
	assert.Nil(t, stream)
}

func TestStoreOpenStreamPrefersCompressedLooseWithoutCorruptFallback(t *testing.T) {
	require := require.New(t)
	content := []byte("preferred compressed loose content")
	layout := layoutForStoreTest(t)
	hash := hashForTest(content)
	rawPath := layout.LoosePath(hash)
	require.NoError(os.MkdirAll(filepath.Dir(rawPath), 0o700))
	require.NoError(os.WriteFile(rawPath, content, 0o600))
	writeCompressedLooseFixture(t, layout, hash, int64(len(content)), bytes.Repeat([]byte{'x'}, len(content)), nil)
	store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{
		hash: {Member: true},
	}}, layout)

	stream, _, err := store.OpenStream(t.Context(), hash)
	require.NoError(err)
	require.ErrorIs(stream.Verify(), ErrContentMismatch)
	require.ErrorIs(stream.Close(), ErrContentMismatch)
}

func TestStoreOpenStreamRetriesAuthorityMoves(t *testing.T) {
	content := []byte("stream migration race")
	hash := hashForTest(content)

	t.Run("loose to pack", func(t *testing.T) {
		require := require.New(t)
		layout := layoutForStoreTest(t)
		entry := buildStoreTestPack(t, layout, content)
		loosePath := layout.LoosePath(hash)
		require.NoError(os.MkdirAll(filepath.Dir(loosePath), 0o700))
		require.NoError(os.WriteFile(loosePath, content, 0o600))
		resolver := &sequenceResolver{locations: []Location{{Member: true}, {Member: true, Pack: &entry}}}
		resolver.beforeFirstReturn = func() { require.NoError(os.Remove(loosePath)) }
		store := newStoreForTest(t, resolver, layout)
		assertStreamContent(t, store, hash, content)
		assert.Equal(t, 2, resolver.calls)
	})

	t.Run("pack to loose", func(t *testing.T) {
		require := require.New(t)
		layout := layoutForStoreTest(t)
		entry := buildStoreTestPack(t, layout, content)
		loosePath := layout.LoosePath(hash)
		require.NoError(os.MkdirAll(filepath.Dir(loosePath), 0o700))
		require.NoError(os.WriteFile(loosePath, content, 0o600))
		require.NoError(os.Remove(layout.PackPath(entry.PackID)))
		resolver := &sequenceResolver{locations: []Location{{Member: true, Pack: &entry}, {Member: true}}}
		store := newStoreForTest(t, resolver, layout)
		assertStreamContent(t, store, hash, content)
		assert.Equal(t, 2, resolver.calls)
	})

	t.Run("pack to pack", func(t *testing.T) {
		layout := layoutForStoreTest(t)
		first := buildStoreTestPack(t, layout, content)
		second := buildStoreTestPack(t, layout, content)
		require.NotEqual(t, first.PackID, second.PackID)
		resolver := &sequenceResolver{locations: []Location{{Member: true, Pack: &first}, {Member: true, Pack: &second}}}
		resolver.beforeFirstReturn = func() { require.NoError(t, os.Remove(layout.PackPath(first.PackID))) }
		store := newStoreForTest(t, resolver, layout)
		assertStreamContent(t, store, hash, content)
		assert.Equal(t, 2, resolver.calls)
	})
}

func TestStoreConcurrentPackedStreamsShareLeasedReader(t *testing.T) {
	require := require.New(t)
	content := bytes.Repeat([]byte("shared packed stream "), 1<<14)
	store, hash := streamStoreForTest(t, "packed", content)
	const streams = 16
	readers := make([]VerifiedReadCloser, streams)
	for i := range readers {
		reader, _, err := store.OpenStream(t.Context(), hash)
		require.NoError(err)
		readers[i] = reader
	}
	store.mu.Lock()
	require.Len(store.packReaders, 1)
	for _, slot := range store.packReaders {
		assert.Equal(t, streams, slot.leases)
	}
	store.mu.Unlock()

	var wg sync.WaitGroup
	errs := make(chan error, streams)
	for _, reader := range readers {
		wg.Go(func() { errs <- reader.Verify() })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(err)
	}
	assertPackedLeases(t, store, 0)
}

func TestStoreEvictionAndClosePreserveActiveStreams(t *testing.T) {
	require := require.New(t)
	layout := layoutForStoreTest(t)
	firstContent := bytes.Repeat([]byte("first stream "), 4096)
	secondContent := bytes.Repeat([]byte("second stream "), 4096)
	first := buildStoreTestPack(t, layout, firstContent)
	second := buildStoreTestPack(t, layout, secondContent)
	resolver := &mapResolver{locations: map[Hash]Location{
		first.Hash: {Member: true, Pack: &first}, second.Hash: {Member: true, Pack: &second},
	}}
	store, err := NewStore(resolver, layout, StoreOptions{Limits: DefaultLimits(), ReaderSlots: 1})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(store.Close()) })

	firstStream, _, err := store.OpenStream(t.Context(), first.Hash)
	require.NoError(err)
	secondStream, _, err := store.OpenStream(t.Context(), second.Hash)
	require.NoError(err)
	require.Len(store.packReaders, 1)
	require.NoError(secondStream.Verify())
	require.NoError(store.Close())
	assert.Empty(t, store.packReaders)
	require.NoError(firstStream.Verify())
	require.NoError(firstStream.Close())
}

func TestStoreRetirePackKeepsActiveStreamReadable(t *testing.T) {
	require := require.New(t)
	content := bytes.Repeat([]byte("retired active stream "), 4096)
	store, hash := streamStoreForTest(t, "packed", content)
	location := store.resolver.(*mapResolver).locations[hash]
	require.NotNil(location.Pack)
	stream, _, err := store.OpenStream(t.Context(), hash)
	require.NoError(err)
	require.NoError(store.RetirePack(location.Pack.PackID))
	_, err = os.Stat(store.layout.PackPath(location.Pack.PackID))
	require.ErrorIs(err, fs.ErrNotExist)
	require.NoError(stream.Verify())
	require.NoError(stream.Close())
}

func TestStoreRetirePackErrorsAreTyped(t *testing.T) {
	require := require.New(t)
	content := []byte("typed retirement")
	store, hash := streamStoreForTest(t, "packed", content)
	location := store.resolver.(*mapResolver).locations[hash]
	path := store.layout.PackPath(location.Pack.PackID)
	stream, _, err := store.OpenStream(t.Context(), hash)
	require.NoError(err)
	orphan := path + ".open"
	require.NoError(os.Rename(path, orphan))
	require.NoError(os.Mkdir(path, 0o700))
	require.NoError(os.WriteFile(filepath.Join(path, "child"), []byte("x"), 0o600))
	err = store.RetirePack(location.Pack.PackID)
	require.ErrorIs(err, ErrPackRetirementDeferred)
	var retireErr *PackRetirementError
	require.ErrorAs(err, &retireErr)
	assert.Equal(t, location.Pack.PackID, retireErr.PackID)
	require.NoError(stream.Verify())
	require.NoError(stream.Close())
}

func TestStoreOpenStreamPreservesBufferedContractAndAppliesPolicy(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := []byte("container policy")
	layout := layoutForStoreTest(t)
	entry := buildStoreTestPack(t, layout, content)
	limits := DefaultLimits()
	limits.PackBytes = 1
	store, err := NewStore(&mapResolver{locations: map[Hash]Location{
		entry.Hash: {Member: true, Pack: &entry},
	}}, layout, StoreOptions{Limits: limits})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(store.Close()) })
	buffered, _, err := store.Open(t.Context(), entry.Hash)
	require.NoError(err)
	require.NoError(buffered.Close())
	_, _, err = store.OpenStream(t.Context(), entry.Hash)
	var limitErr *LimitError
	require.ErrorAs(err, &limitErr)
	assert.Equal(LimitPackContainerBytes, limitErr.Dimension)

	strictStore, err := NewStore(&mapResolver{locations: map[Hash]Location{
		entry.Hash: {Member: true, Pack: &entry},
	}}, layout, StoreOptions{Limits: limits})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(strictStore.Close()) })
	_, _, err = strictStore.OpenStream(t.Context(), entry.Hash)
	require.ErrorAs(err, &limitErr)
	assert.Equal(LimitPackContainerBytes, limitErr.Dimension)
}

func TestStoreOpenStreamRejectsNonMemberBeforePhysicalRead(t *testing.T) {
	content := []byte("physical but unauthorized")
	layout := layoutForStoreTest(t)
	hash := hashForTest(content)
	require.NoError(t, os.MkdirAll(filepath.Dir(layout.LoosePath(hash)), 0o700))
	require.NoError(t, os.WriteFile(layout.LoosePath(hash), content, 0o600))
	store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{hash: {}}}, layout)
	_, _, err := store.OpenStream(t.Context(), hash)
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func TestStoreOpenWindowPolicyAppliesOnlyToStreaming(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := bytes.Repeat([]byte("window policy "), 1<<18)
	var frame bytes.Buffer
	encoder, err := zstd.NewWriter(&frame, zstd.WithWindowSize(8<<20), zstd.WithEncoderConcurrency(1))
	require.NoError(err)
	_, err = encoder.Write(content)
	require.NoError(err)
	require.NoError(encoder.Close())

	layout := layoutForStoreTest(t)
	staging := t.TempDir()
	w, err := pack.NewWriter(staging, pack.WriterOptions{})
	require.NoError(err)
	id := pack.ComputeBlobID(content)
	entry, err := w.AppendEncoded(id, frame.Bytes(), uint64(len(content)), true)
	require.NoError(err)
	packID := w.ID()
	require.NoError(os.MkdirAll(filepath.Dir(layout.PackPath(packID)), 0o700))
	_, err = w.Seal(layout.PackPath(packID))
	require.NoError(err)
	hash, err := ParseHash(id.String())
	require.NoError(err)
	indexed := IndexEntry{Hash: hash, PackID: packID, Offset: int64(entry.Offset), StoredLen: int64(entry.StoredLen), RawLen: int64(entry.RawLen), Flags: uint8(entry.Flags), CRC32C: entry.CRC32C}
	limits := DefaultLimits()
	limits.BlobBytes = 4 << 20
	store, err := NewStore(&mapResolver{locations: map[Hash]Location{
		hash: {Member: true, Pack: &indexed},
	}}, layout, StoreOptions{Limits: limits})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(store.Close()) })
	buffered, size, err := store.Open(t.Context(), hash)
	require.NoError(err)
	assert.Equal(int64(len(content)), size)
	actual, err := io.ReadAll(buffered)
	require.NoError(err)
	require.NoError(buffered.Close())
	assert.Equal(content, actual)

	_, _, err = store.OpenStream(t.Context(), hash)
	var limitErr *LimitError
	require.ErrorAs(err, &limitErr)
	assert.Equal(LimitBlobWindowBytes, limitErr.Dimension)
}

func TestStoreStreamsPackedObjectAboveDefaultCeiling(t *testing.T) {
	require := require.New(t)
	if testing.Short() {
		t.Skip("writes a blob above the default 64 MiB policy ceiling")
	}
	size := largeStoreStreamTestBytes(t, 64<<20+1)
	layout := layoutForStoreTest(t)
	staging := t.TempDir()
	w, err := pack.NewWriter(staging, pack.WriterOptions{})
	require.NoError(err)
	entry, err := w.AppendStream(t.Context(), io.LimitReader(streamZeroReader{}, size), uint64(size), pack.AppendStreamOptions{
		ScratchDir: staging, ScratchBytes: uint64(size)*2 + 64<<20,
	})
	require.NoError(err)
	packID := w.ID()
	require.NoError(os.MkdirAll(filepath.Dir(layout.PackPath(packID)), 0o700))
	_, err = w.Seal(layout.PackPath(packID))
	require.NoError(err)
	hash, err := ParseHash(entry.ID.String())
	require.NoError(err)
	indexed := IndexEntry{Hash: hash, PackID: packID, Offset: int64(entry.Offset), StoredLen: int64(entry.StoredLen), RawLen: int64(entry.RawLen), Flags: uint8(entry.Flags), CRC32C: entry.CRC32C}
	limits := DefaultLimits()
	limits.BlobBytes = size
	store, err := NewStore(&mapResolver{locations: map[Hash]Location{
		hash: {Member: true, Pack: &indexed},
	}}, layout, StoreOptions{Limits: limits})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(store.Close()) })
	stream, gotSize, err := store.OpenStream(t.Context(), hash)
	require.NoError(err)
	assert.Equal(t, size, gotSize)
	require.NoError(stream.Verify())
	require.NoError(stream.Close())
}

func largeStoreStreamTestBytes(t *testing.T, fallback int64) int64 {
	t.Helper()
	value := os.Getenv("KIT_STREAM_TEST_BYTES")
	if value == "" {
		return fallback
	}
	size, err := strconv.ParseInt(value, 10, 64)
	require.NoError(t, err)
	require.Positive(t, size)
	return size
}

type streamZeroReader struct{}

func (streamZeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func streamStoreForTest(t *testing.T, representation string, content []byte) (*Store, Hash) {
	t.Helper()
	layout := layoutForStoreTest(t)
	hash := hashForTest(content)
	location := Location{Member: true}
	switch representation {
	case "loose":
		require.NoError(t, os.MkdirAll(filepath.Dir(layout.LoosePath(hash)), 0o700))
		require.NoError(t, os.WriteFile(layout.LoosePath(hash), content, 0o600))
	case "compressed":
		writeCompressedLooseFixture(t, layout, hash, int64(len(content)), content, nil)
	case "packed":
		entry := buildStoreTestPack(t, layout, content)
		hash = entry.Hash
		location.Pack = &entry
	default:
		require.FailNow(t, "unknown representation", representation)
	}
	return newStoreForTest(t, &mapResolver{locations: map[Hash]Location{hash: location}}, layout), hash
}

func writeCompressedLooseFixture(
	t *testing.T,
	layout Layout,
	hash Hash,
	logicalSize int64,
	decoded []byte,
	mutate func([]byte) []byte,
) {
	t.Helper()
	require := require.New(t)
	t.Helper()
	require.GreaterOrEqual(logicalSize, int64(0))
	header := encodeCompressedLooseHeader(uint64(logicalSize))
	var physical bytes.Buffer
	_, err := physical.Write(header[:])
	require.NoError(err)
	encoder, err := zstd.NewWriter(&physical, zstd.WithEncoderConcurrency(1))
	require.NoError(err)
	_, err = encoder.Write(decoded)
	require.NoError(err)
	require.NoError(encoder.Close())
	data := physical.Bytes()
	if mutate != nil {
		data = mutate(bytes.Clone(data))
	}
	path := layout.CompressedLoosePath(hash)
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(os.WriteFile(path, data, 0o600))
}

func assertStreamContent(t *testing.T, store *Store, hash Hash, want []byte) {
	t.Helper()
	require := require.New(t)
	t.Helper()
	stream, size, err := store.OpenStream(t.Context(), hash)
	require.NoError(err)
	got, err := io.ReadAll(stream)
	require.NoError(err)
	assert.Equal(t, int64(len(want)), size)
	assert.Equal(t, want, got)
	require.NoError(stream.Close())
}

func assertPackedLeases(t *testing.T, store *Store, want int) {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, slot := range store.packReaders {
		assert.Equal(t, want, slot.leases)
	}
}
