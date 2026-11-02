package pack

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAppendStreamRoundTrip(t *testing.T) {
	t.Parallel()
	incompressible := make([]byte, 64<<10)
	var state uint32 = 1
	for i := range incompressible {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		incompressible[i] = byte(state)
	}
	tests := []struct {
		name       string
		content    []byte
		compressed bool
	}{
		{name: "empty", content: []byte{}},
		{name: "raw", content: []byte("plain streamed content")},
		{name: "below-zstd-window", content: make([]byte, 1023)},
		{name: "at-zstd-window", content: make([]byte, 1024), compressed: true},
		{name: "incompressible", content: incompressible},
		{name: "compressed", content: bytes.Repeat([]byte("compressible-stream-"), 1<<16), compressed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			t.Parallel()
			dir := t.TempDir()
			writer, err := NewWriter(dir, WriterOptions{})
			require.NoError(err)
			t.Cleanup(func() { _ = writer.Abort() })

			id := ComputeBlobID(tt.content)
			entry, err := writer.AppendStream(t.Context(), bytes.NewReader(tt.content), uint64(len(tt.content)), AppendStreamOptions{
				ExpectedID:   &id,
				ScratchDir:   dir,
				ScratchBytes: uint64(len(tt.content))*3 + 1024,
			})
			require.NoError(err)
			assert.Equal(tt.compressed, entry.Flags&BlobCompressed != 0)
			assert.Equal(id, entry.ID)

			matches, err := filepath.Glob(filepath.Join(dir, "pack-prepared-*"))
			require.NoError(err)
			assert.Empty(matches)

			final := filepath.Join(dir, writer.ID()+".pack")
			_, err = writer.Seal(final)
			require.NoError(err)
			reader, err := OpenReader(final, nil)
			require.NoError(err)
			if tt.compressed {
				window, windowErr := reader.streamingWindow(reader.Entries()[0])
				require.NoError(windowErr)
				assert.LessOrEqual(window, uint64(streamMaxWindowSize))
			}
			stream, err := reader.OpenBlob(t.Context(), reader.Entries()[0])
			require.NoError(err)
			got, err := io.ReadAll(stream)
			require.NoError(err)
			assert.Equal(tt.content, got)
			assert.True(stream.Verified())
			require.NoError(stream.Verify())
			require.NoError(stream.Close())

			buffered, err := reader.ReadBlob(reader.Entries()[0])
			require.NoError(err)
			assert.Equal(tt.content, buffered)
			require.NoError(reader.Close())
		})
	}
}

func TestAppendStreamAboveLegacyCeiling(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a blob above the former 64 MiB policy ceiling")
	}
	size := uint64(largeStreamTestBytes(t, 64<<20+1))
	tests := []struct {
		name       string
		source     func() io.Reader
		compressed bool
	}{
		{name: "compressed", source: func() io.Reader { return zeroReader{} }, compressed: true},
		{name: "raw", source: func() io.Reader { return &noiseReader{state: 1} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			dir := t.TempDir()
			writer, err := NewWriter(dir, WriterOptions{})
			require.NoError(err)
			t.Cleanup(func() { _ = writer.Abort() })
			source := io.LimitReader(tt.source(), int64(size))
			entry, err := writer.AppendStream(t.Context(), source, size, AppendStreamOptions{
				ScratchDir: dir, ScratchBytes: size*2 + 64<<20,
			})
			require.NoError(err)
			assert.Equal(tt.compressed, entry.Flags&BlobCompressed != 0)
			final := filepath.Join(dir, writer.ID()+".pack")
			_, err = writer.Seal(final)
			require.NoError(err)

			reader, err := OpenReader(final, nil)
			require.NoError(err)
			stream, err := reader.OpenBlob(t.Context(), reader.Entries()[0])
			require.NoError(err)
			require.NoError(stream.Verify())
			assert.True(stream.Verified())
			require.NoError(stream.Close())
			require.NoError(reader.Close())
		})
	}
}

func largeStreamTestBytes(t *testing.T, fallback int64) int64 {
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

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

type noiseReader struct{ state uint32 }

func (r *noiseReader) Read(p []byte) (int, error) {
	for i := range p {
		r.state ^= r.state << 13
		r.state ^= r.state >> 17
		r.state ^= r.state << 5
		p[i] = byte(r.state)
	}
	return len(p), nil
}

func TestAppendStreamSourceFailuresLeaveWriterUsable(t *testing.T) {
	require := require.New(t)
	t.Parallel()
	dir := t.TempDir()
	writer, err := NewWriter(dir, WriterOptions{})
	require.NoError(err)
	t.Cleanup(func() { _ = writer.Abort() })

	_, err = writer.AppendStream(t.Context(), strings.NewReader("short"), 6, AppendStreamOptions{ScratchDir: dir})
	require.ErrorIs(err, ErrTruncated)
	_, err = writer.AppendStream(t.Context(), strings.NewReader("trailing"), 5, AppendStreamOptions{ScratchDir: dir})
	require.ErrorIs(err, ErrCorrupt)

	content := []byte("valid after source failures")
	entry, err := writer.AppendStream(t.Context(), bytes.NewReader(content), uint64(len(content)), AppendStreamOptions{ScratchDir: dir})
	require.NoError(err)
	assert.Equal(t, ComputeBlobID(content), entry.ID)
}

func TestPrepareBlobCancellationCleansScratch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	source := &cancelingReader{remaining: 1 << 20, cancel: cancel}
	_, err := PrepareBlob(ctx, source, 1<<20, DefaultZstdLevel, AppendStreamOptions{ScratchDir: dir})
	require.ErrorIs(t, err, context.Canceled)
	matches, err := filepath.Glob(filepath.Join(dir, "pack-prepared-*"))
	require.NoError(t, err)
	assert.Empty(t, matches)
}

func TestPrepareBlobPreservesSourceError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sourceErr := errors.New("source failed")
	_, err := PrepareBlob(t.Context(), &failingReader{err: sourceErr}, 1<<20, DefaultZstdLevel, AppendStreamOptions{ScratchDir: dir})
	require.ErrorIs(t, err, sourceErr)
	matches, err := filepath.Glob(filepath.Join(dir, "pack-prepared-*"))
	require.NoError(t, err)
	assert.Empty(t, matches)
}

type failingReader struct {
	wrote bool
	err   error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.wrote {
		return 0, r.err
	}
	r.wrote = true
	n := min(len(p), 128)
	clear(p[:n])
	return n, nil
}

type cancelingReader struct {
	remaining int
	cancel    context.CancelFunc
	didCancel bool
}

func (r *cancelingReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.remaining)
	clear(p[:n])
	r.remaining -= n
	if !r.didCancel {
		r.didCancel = true
		r.cancel()
	}
	return n, nil
}

func TestPrepareBlobLimitsAndIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()
	dir := t.TempDir()
	content := bytes.Repeat([]byte("x"), 1<<16)
	wrong := ComputeBlobID([]byte("wrong"))

	_, err := PrepareBlob(t.Context(), bytes.NewReader(content), uint64(len(content)), DefaultZstdLevel, AppendStreamOptions{
		ScratchDir: dir, ScratchBytes: uint64(len(content)),
	})
	var limitErr *StreamLimitError
	require.ErrorAs(err, &limitErr)
	assert.Equal(StreamLimitScratchBytes, limitErr.Dimension)

	_, err = PrepareBlob(t.Context(), bytes.NewReader(content), uint64(len(content)), DefaultZstdLevel, AppendStreamOptions{
		ExpectedID: &wrong, ScratchDir: dir,
	})
	require.ErrorIs(err, ErrBlobMismatch)
	matches, globErr := filepath.Glob(filepath.Join(dir, "pack-prepared-*"))
	require.NoError(globErr)
	assert.Empty(matches)

	_, err = PrepareBlob(t.Context(), strings.NewReader(""), MaxRawLen+1, DefaultZstdLevel, AppendStreamOptions{ScratchDir: dir})
	require.ErrorAs(err, &limitErr)
	assert.Equal(StreamLimitRawBytes, limitErr.Dimension)

	small := []byte("small")
	prepared, err := PrepareBlob(t.Context(), bytes.NewReader(small), uint64(len(small)), DefaultZstdLevel, AppendStreamOptions{
		ScratchDir: dir, ScratchBytes: uint64(len(small)),
	})
	require.NoError(err)
	require.NoError(prepared.Close())
}

func TestPreparedBlobCloseIsIdempotent(t *testing.T) {
	require := require.New(t)
	t.Parallel()
	dir := t.TempDir()
	content := []byte("discard prepared content")
	prepared, err := PrepareBlob(t.Context(), bytes.NewReader(content), uint64(len(content)), DefaultZstdLevel, AppendStreamOptions{ScratchDir: dir})
	require.NoError(err)
	require.NoError(prepared.Close())
	require.NoError(prepared.Close())
	matches, err := filepath.Glob(filepath.Join(dir, "pack-prepared-*"))
	require.NoError(err)
	assert.Empty(t, matches)
}

func TestAppendPreparedZeroByteWriteFailureDoesNotPoisonWriter(t *testing.T) {
	require := require.New(t)
	t.Parallel()
	dir := t.TempDir()
	content := bytes.Repeat([]byte("prepared"), 1<<14)
	prepared, err := PrepareBlob(t.Context(), bytes.NewReader(content), uint64(len(content)), DefaultZstdLevel, AppendStreamOptions{ScratchDir: dir})
	require.NoError(err)

	writer, err := NewWriter(dir, WriterOptions{})
	require.NoError(err)
	t.Cleanup(func() { _ = writer.Abort() })
	require.NoError(writer.f.Close())
	_, err = writer.AppendPrepared(t.Context(), prepared)
	require.Error(err)
	assert.NoError(t, writer.err)
}

func TestAppendPreparedCancellationBeforeCopyLeavesWriterUsable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()
	dir := t.TempDir()
	content := bytes.Repeat([]byte("prepared cancellation"), 1<<12)
	prepared, err := PrepareBlob(t.Context(), bytes.NewReader(content), uint64(len(content)), DefaultZstdLevel, AppendStreamOptions{ScratchDir: dir})
	require.NoError(err)

	writer, err := NewWriter(dir, WriterOptions{})
	require.NoError(err)
	t.Cleanup(func() { _ = writer.Abort() })
	ctx, cancel := context.WithCancel(t.Context())
	cancelBetweenChecks := &cancelAfterFirstErrContext{Context: ctx, cancel: cancel}
	_, err = writer.AppendPrepared(cancelBetweenChecks, prepared)
	require.ErrorIs(err, context.Canceled)
	assert.Equal(int64(headerSize), writer.StoredSize())

	later := []byte("later append")
	entry, err := writer.Append(later)
	require.NoError(err)
	assert.Equal(ComputeBlobID(later), entry.ID)
}

type cancelAfterFirstErrContext struct {
	context.Context
	cancel context.CancelFunc
	once   sync.Once
}

func (c *cancelAfterFirstErrContext) Err() error {
	err := c.Context.Err()
	c.once.Do(c.cancel)
	return err
}

func TestAppendPreparedScratchCorruptionPoisonsWriter(t *testing.T) {
	require := require.New(t)
	t.Parallel()
	dir := t.TempDir()
	content := bytes.Repeat([]byte("prepared corruption"), 1<<12)
	prepared, err := PrepareBlob(t.Context(), bytes.NewReader(content), uint64(len(content)), DefaultZstdLevel, AppendStreamOptions{ScratchDir: dir})
	require.NoError(err)
	_, err = prepared.f.WriteAt([]byte{0xff}, 0)
	require.NoError(err)

	writer, err := NewWriter(dir, WriterOptions{})
	require.NoError(err)
	t.Cleanup(func() { _ = writer.Abort() })
	_, firstErr := writer.AppendPrepared(t.Context(), prepared)
	require.ErrorIs(firstErr, ErrCorrupt)
	_, nextErr := writer.Append([]byte("later"))
	assert.EqualError(t, nextErr, firstErr.Error())
}

func TestBlobReaderTerminalVerificationAndParentLifetime(t *testing.T) {
	require := require.New(t)
	t.Parallel()
	dir := t.TempDir()
	content := []byte("terminal verification content")
	writer, err := NewWriter(dir, WriterOptions{})
	require.NoError(err)
	entry, err := writer.Append(content)
	require.NoError(err)
	final := filepath.Join(dir, writer.ID()+".pack")
	_, err = writer.Seal(final)
	require.NoError(err)

	reader, err := OpenReader(final, nil)
	require.NoError(err)
	stream, err := reader.OpenBlob(t.Context(), entry)
	require.NoError(err)
	require.ErrorIs(reader.Close(), ErrStreamsActive)
	buf := make([]byte, 4)
	_, err = stream.Read(buf)
	require.NoError(err)
	require.ErrorIs(stream.Close(), ErrVerificationIncomplete)
	require.NoError(reader.Close())
}

func TestBlobReaderRejectsEntryOutsideVerifiedFooter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()
	dir := t.TempDir()
	secret := []byte("secret entry bytes")
	public := []byte("public entry bytes")
	writer, err := NewWriter(dir, WriterOptions{})
	require.NoError(err)
	secretEntry, err := writer.Append(secret)
	require.NoError(err)
	publicEntry, err := writer.Append(public)
	require.NoError(err)
	final := filepath.Join(dir, writer.ID()+".pack")
	_, err = writer.Seal(final)
	require.NoError(err)

	reader, err := OpenReader(final, nil)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(reader.Close()) })
	listed := reader.Entries()
	listed[0] = publicEntry
	assert.Equal(secretEntry, reader.Entries()[0], "returned footer entries must not mutate reader authority")

	forged := publicEntry
	forged.Offset = secretEntry.Offset
	forged.StoredLen = secretEntry.StoredLen
	forged.RawLen = secretEntry.RawLen
	stream, err := reader.OpenBlob(t.Context(), forged)
	require.ErrorIs(err, ErrCorrupt)
	require.ErrorContains(err, "does not match verified footer")
	assert.Nil(stream)
}

func TestBlobReaderCancellationIsTerminal(t *testing.T) {
	require := require.New(t)
	t.Parallel()
	dir := t.TempDir()
	content := bytes.Repeat([]byte("cancel stream"), 1<<14)
	writer, err := NewWriter(dir, WriterOptions{})
	require.NoError(err)
	entry, err := writer.Append(content)
	require.NoError(err)
	final := filepath.Join(dir, writer.ID()+".pack")
	_, err = writer.Seal(final)
	require.NoError(err)
	reader, err := OpenReader(final, nil)
	require.NoError(err)

	ctx, cancel := context.WithCancel(t.Context())
	stream, err := reader.OpenBlob(ctx, entry)
	require.NoError(err)
	buf := make([]byte, 32)
	_, err = stream.Read(buf)
	require.NoError(err)
	cancel()
	_, err = stream.Read(buf)
	require.ErrorIs(err, context.Canceled)
	require.ErrorIs(stream.Verify(), context.Canceled)
	require.ErrorIs(stream.Close(), context.Canceled)
	require.NoError(reader.Close())
}

func TestBlobReaderReportsTerminalIntegrityErrors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()
	dir := t.TempDir()
	content := []byte("content delivered before terminal verification")
	writer, err := NewWriter(dir, WriterOptions{})
	require.NoError(err)
	entry, err := writer.Append(content)
	require.NoError(err)
	final := filepath.Join(dir, writer.ID()+".pack")
	_, err = writer.Seal(final)
	require.NoError(err)

	f, err := os.OpenFile(final, os.O_RDWR, 0)
	require.NoError(err)
	_, err = f.WriteAt([]byte{'X'}, int64(entry.Offset))
	require.NoError(err)
	require.NoError(f.Close())

	reader, err := OpenReader(final, nil)
	require.NoError(err)
	stream, err := reader.OpenBlob(t.Context(), reader.Entries()[0])
	require.NoError(err)
	got, err := io.ReadAll(stream)
	require.ErrorIs(err, ErrCorrupt)
	assert.Len(got, len(content))
	assert.False(stream.Verified())
	require.ErrorIs(stream.Close(), ErrVerificationIncomplete)
	require.NoError(reader.Close())
}

func TestBlobReaderReportsCompressedDecodeFailure(t *testing.T) {
	require := require.New(t)
	t.Parallel()
	dir := t.TempDir()
	content := bytes.Repeat([]byte("compressed corruption "), 1<<15)
	writer, err := NewWriter(dir, WriterOptions{})
	require.NoError(err)
	entry, err := writer.AppendStream(t.Context(), bytes.NewReader(content), uint64(len(content)), AppendStreamOptions{ScratchDir: dir})
	require.NoError(err)
	require.NotZero(entry.Flags & BlobCompressed)
	final := filepath.Join(dir, writer.ID()+".pack")
	_, err = writer.Seal(final)
	require.NoError(err)

	f, err := os.OpenFile(final, os.O_RDWR, 0)
	require.NoError(err)
	corruptAt := int64(entry.Offset + entry.StoredLen/2)
	var original [1]byte
	_, err = f.ReadAt(original[:], corruptAt)
	require.NoError(err)
	original[0] ^= 0xff
	_, err = f.WriteAt(original[:], corruptAt)
	require.NoError(err)
	require.NoError(f.Close())

	reader, err := OpenReader(final, nil)
	require.NoError(err)
	stream, err := reader.OpenBlob(t.Context(), reader.Entries()[0])
	require.NoError(err)
	_, err = io.Copy(io.Discard, stream)
	require.ErrorIs(err, ErrCorrupt)
	require.ErrorIs(stream.Close(), ErrVerificationIncomplete)
	require.NoError(reader.Close())
}

func TestBlobReaderDetectsHashMismatch(t *testing.T) {
	require := require.New(t)
	t.Parallel()
	dir := t.TempDir()
	content := []byte("hash checked at eof")
	writer, err := NewWriter(dir, WriterOptions{})
	require.NoError(err)
	entry, err := writer.Append(content)
	require.NoError(err)
	final := filepath.Join(dir, writer.ID()+".pack")
	_, err = writer.Seal(final)
	require.NoError(err)
	forged := entry
	forged.ID = ComputeBlobID([]byte("different"))
	data, err := os.ReadFile(final)
	require.NoError(err)
	footerStart := int(entry.Offset + entry.StoredLen)
	rebuilt := append([]byte{}, data[:footerStart]...)
	rebuilt = append(rebuilt, appendPlainTrailer(encodeFooterRegion([]Entry{forged}))...)
	require.NoError(os.WriteFile(final, rebuilt, 0o600))

	reader, err := OpenReader(final, nil)
	require.NoError(err)
	stream, err := reader.OpenBlob(t.Context(), reader.Entries()[0])
	require.NoError(err)
	got, err := io.ReadAll(stream)
	assert.Equal(t, content, got)
	require.ErrorIs(err, ErrBlobMismatch)
	require.ErrorIs(stream.Verify(), ErrBlobMismatch)
	require.ErrorIs(stream.Close(), ErrBlobMismatch)
	_, repeatedErr := stream.Read(make([]byte, 1))
	require.EqualError(repeatedErr, err.Error())
	require.EqualError(stream.Verify(), err.Error())
	require.NoError(reader.Close())
}

func TestStreamingEncryptedV1IsUnsupported(t *testing.T) {
	require := require.New(t)
	t.Parallel()
	key := [32]byte{1}
	crypter, err := NewCrypter(key)
	require.NoError(err)
	dir := t.TempDir()
	writer, err := NewWriter(dir, WriterOptions{Crypter: crypter})
	require.NoError(err)
	t.Cleanup(func() { _ = writer.Abort() })
	_, err = writer.AppendStream(t.Context(), strings.NewReader("secret"), 6, AppendStreamOptions{})
	require.ErrorIs(err, ErrStreamUnsupported)

	entry, err := writer.Append([]byte("buffered secret"))
	require.NoError(err)
	final := filepath.Join(dir, writer.ID()+".pack")
	_, err = writer.Seal(final)
	require.NoError(err)
	reader, err := OpenReader(final, crypter)
	require.NoError(err)
	_, err = reader.OpenBlob(t.Context(), entry)
	require.ErrorIs(err, ErrStreamUnsupported)
	require.NoError(reader.Close())
}

func TestOpenReaderWithOptionsEnforcesLimits(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()
	dir := t.TempDir()
	writer, err := NewWriter(dir, WriterOptions{})
	require.NoError(err)
	_, err = writer.Append([]byte("bounded content"))
	require.NoError(err)
	_, err = writer.Append([]byte("second"))
	require.NoError(err)
	final := filepath.Join(dir, writer.ID()+".pack")
	_, err = writer.Seal(final)
	require.NoError(err)

	_, err = OpenReaderWithOptions(final, nil, ReaderOptions{Limits: ReaderLimits{RawBytes: 1}})
	var limitErr *StreamLimitError
	require.ErrorAs(err, &limitErr)
	assert.Equal(StreamLimitRawBytes, limitErr.Dimension)

	info, err := os.Stat(final)
	require.NoError(err)
	_, err = OpenReaderWithOptions(final, nil, ReaderOptions{Limits: ReaderLimits{ContainerBytes: uint64(info.Size() - 1)}})
	require.ErrorAs(err, &limitErr)
	assert.Equal(StreamLimitContainerBytes, limitErr.Dimension)

	_, err = OpenReaderWithOptions(final, nil, ReaderOptions{Limits: ReaderLimits{Entries: 1}})
	require.ErrorAs(err, &limitErr)
	assert.Equal(StreamLimitEntryCount, limitErr.Dimension)

	_, err = OpenReaderWithOptions(final, nil, ReaderOptions{Limits: ReaderLimits{FooterBytes: 1}})
	require.ErrorAs(err, &limitErr)
	assert.Equal(StreamLimitFooterBytes, limitErr.Dimension)

	_, err = OpenReaderWithOptions(final, nil, ReaderOptions{Limits: ReaderLimits{StoredBytes: 1}})
	require.ErrorAs(err, &limitErr)
	assert.Equal(StreamLimitStoredBytes, limitErr.Dimension)
}

func TestBlobReaderEnforcesZstdWindowLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()
	dir := t.TempDir()
	content := bytes.Repeat([]byte("legacy-single-segment"), 1<<16)
	writer, err := NewWriter(dir, WriterOptions{})
	require.NoError(err)
	entry, err := writer.Append(content)
	require.NoError(err)
	require.NotZero(entry.Flags & BlobCompressed)
	final := filepath.Join(dir, writer.ID()+".pack")
	_, err = writer.Seal(final)
	require.NoError(err)

	reader, err := OpenReaderWithOptions(final, nil, ReaderOptions{Limits: ReaderLimits{WindowBytes: 64 << 10}})
	require.NoError(err)
	_, err = reader.OpenBlob(t.Context(), reader.Entries()[0])
	var limitErr *StreamLimitError
	require.ErrorAs(err, &limitErr)
	assert.Equal(StreamLimitWindowBytes, limitErr.Dimension)
	assert.Greater(limitErr.Actual, limitErr.Limit)
	require.NoError(reader.Close())
}

func TestBlobReaderReadsFrozenV1Fixture(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()
	path := filepath.Join("..", "packstore", "testdata", "msgvault-v1", "01kx758hcw5gnkdz233217fd9a.mvpack")
	reader, err := OpenReader(path, nil)
	require.NoError(err)
	compressed := false
	for _, entry := range reader.Entries() {
		compressed = compressed || entry.Flags&BlobCompressed != 0
		stream, openErr := reader.OpenBlob(t.Context(), entry)
		require.NoError(openErr)
		require.NoError(stream.Verify())
		assert.True(stream.Verified())
		require.NoError(stream.Close())
	}
	assert.True(compressed)
	require.NoError(reader.Close())
}

func TestOpenBlobHonorsCancellation(t *testing.T) {
	require := require.New(t)
	t.Parallel()
	dir := t.TempDir()
	writer, err := NewWriter(dir, WriterOptions{})
	require.NoError(err)
	entry, err := writer.Append([]byte("cancelled"))
	require.NoError(err)
	final := filepath.Join(dir, writer.ID()+".pack")
	_, err = writer.Seal(final)
	require.NoError(err)
	reader, err := OpenReader(final, nil)
	require.NoError(err)
	defer func() { _ = reader.Close() }()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = reader.OpenBlob(ctx, entry)
	require.ErrorIs(err, context.Canceled)
}
