package packstore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func TestMultiStoreSelectsFirstHealthyCandidate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := []byte("multi-location content")
	hash := hashForTest(content)
	primary := &recordingReadBackend{content: content}
	secondary := &recordingReadBackend{content: content}
	store, err := NewMultiStore(
		staticLocationResolver{resolution: Resolution{
			Member: true,
			Candidates: []ReadLocation{
				{StoreID: "primary", Generation: "primary-1", Loose: rawLocation(content)},
				{StoreID: "secondary", Generation: "secondary-1", Loose: rawLocation(content)},
			},
		}},
		staticBackendRegistry{
			"primary":   primary,
			"secondary": secondary,
		},
		MultiStoreOptions{},
	)
	require.NoError(err)

	stream, size, err := store.OpenStream(t.Context(), hash)
	require.NoError(err)
	got, err := io.ReadAll(stream)
	require.NoError(err)
	require.NoError(stream.Close())

	assert.Equal(content, got)
	assert.Equal(int64(len(content)), size)
	assert.Equal(1, primary.opens)
	assert.Zero(secondary.opens)
}

func TestMultiStoreOpenFailureFallsThroughBeforePayload(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := []byte("secondary content")
	hash := hashForTest(content)
	primary := &recordingReadBackend{
		err: fmt.Errorf("open primary: %w", ErrPhysicalMissing),
	}
	secondary := &recordingReadBackend{content: content}
	store, err := NewMultiStore(
		staticLocationResolver{resolution: Resolution{
			Member: true,
			Candidates: []ReadLocation{
				{StoreID: "primary", Generation: "primary-1", Loose: rawLocation(content)},
				{StoreID: "secondary", Generation: "secondary-1", Loose: rawLocation(content)},
			},
		}},
		staticBackendRegistry{
			"primary":   primary,
			"secondary": secondary,
		},
		MultiStoreOptions{},
	)
	require.NoError(err)

	stream, _, err := store.OpenStream(t.Context(), hash)
	require.NoError(err)
	got, err := io.ReadAll(stream)
	require.NoError(err)
	require.NoError(stream.Close())

	assert.Equal(content, got)
	assert.Equal(1, primary.opens)
	assert.Equal(1, secondary.opens)
}

func TestMultiStoreRefreshesChangedResolutionAfterMissingLocation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := []byte("migrated content")
	hash := hashForTest(content)
	oldLocation := ReadLocation{
		StoreID: "old", Generation: "old-1",
		Loose: &LooseLocation{
			Encoding: LooseEncodingRaw, LogicalSize: int64(len(content)),
			StoredSize: int64(len(content)),
		},
	}
	newLocation := ReadLocation{
		StoreID: "new", Generation: "new-1",
		Loose: &LooseLocation{
			Encoding: LooseEncodingRaw, LogicalSize: int64(len(content)),
			StoredSize: int64(len(content)),
		},
	}
	resolver := &changingLocationResolver{resolutions: []Resolution{
		{Member: true, Candidates: []ReadLocation{oldLocation}},
		{Member: true, Candidates: []ReadLocation{newLocation}},
	}}
	oldBackend := &recordingReadBackend{err: ErrPhysicalMissing}
	newBackend := &recordingReadBackend{content: content}
	store, err := NewMultiStore(
		resolver,
		staticBackendRegistry{"old": oldBackend, "new": newBackend},
		MultiStoreOptions{},
	)
	require.NoError(err)

	stream, _, err := store.OpenStream(t.Context(), hash)
	require.NoError(err)
	got, err := io.ReadAll(stream)
	require.NoError(err)
	require.NoError(stream.Close())

	assert.Equal(content, got)
	assert.Equal(2, resolver.calls)
	assert.Equal(1, oldBackend.opens)
	assert.Equal(1, newBackend.opens)
}

func TestMultiStoreRefreshesAfterMixedCorruptAndMissingLocations(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := []byte("migrated content after mixed failures")
	hash := hashForTest(content)
	corruptLocation := ReadLocation{
		StoreID: "corrupt", Generation: "corrupt-1",
		Loose: rawLocation(content),
	}
	missingLocation := ReadLocation{
		StoreID: "old", Generation: "old-1",
		Loose: rawLocation(content),
	}
	newLocation := ReadLocation{
		StoreID: "new", Generation: "new-1",
		Loose: rawLocation(content),
	}
	resolver := &changingLocationResolver{resolutions: []Resolution{
		{Member: true, Candidates: []ReadLocation{corruptLocation, missingLocation}},
		{Member: true, Candidates: []ReadLocation{newLocation}},
	}}
	corruptBackend := &recordingReadBackend{err: ErrPhysicalCorrupt}
	missingBackend := &recordingReadBackend{err: ErrPhysicalMissing}
	newBackend := &recordingReadBackend{content: content}
	store, err := NewMultiStore(
		resolver,
		staticBackendRegistry{
			"corrupt": corruptBackend,
			"old":     missingBackend,
			"new":     newBackend,
		},
		MultiStoreOptions{},
	)
	require.NoError(err)

	stream, _, err := store.OpenStream(t.Context(), hash)
	require.NoError(err)
	got, err := io.ReadAll(stream)
	require.NoError(err)
	require.NoError(stream.Close())

	assert.Equal(content, got)
	assert.Equal(2, resolver.calls)
	assert.Equal(1, corruptBackend.opens)
	assert.Equal(1, missingBackend.opens)
	assert.Equal(1, newBackend.opens)
}

func TestMultiStoreNextReadDemotesCorruptGeneration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := []byte("redundant content")
	hash := hashForTest(content)
	primary := &recordingReadBackend{
		content:     content,
		terminalErr: fmt.Errorf("verify primary: %w", ErrPhysicalCorrupt),
	}
	secondary := &recordingReadBackend{content: content}
	store, err := NewMultiStore(
		staticLocationResolver{resolution: Resolution{
			Member: true,
			Candidates: []ReadLocation{
				{StoreID: "primary", Generation: "primary-1", Loose: rawLocation(content)},
				{StoreID: "secondary", Generation: "secondary-1", Loose: rawLocation(content)},
			},
		}},
		staticBackendRegistry{
			"primary":   primary,
			"secondary": secondary,
		},
		MultiStoreOptions{},
	)
	require.NoError(err)

	stream, _, err := store.OpenStream(t.Context(), hash)
	require.NoError(err)
	got, err := io.ReadAll(stream)
	require.ErrorIs(err, ErrPhysicalCorrupt)
	assert.Equal(content, got)
	assert.False(stream.Verified())
	require.ErrorIs(stream.Close(), ErrPhysicalCorrupt)

	stream, _, err = store.OpenStream(t.Context(), hash)
	require.NoError(err)
	got, err = io.ReadAll(stream)
	require.NoError(err)
	require.NoError(stream.Close())

	assert.Equal(content, got)
	assert.Equal(1, primary.opens)
	assert.Equal(1, secondary.opens)
}

func TestMultiStoreGenerationChangeClearsDemotion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := []byte("repaired content")
	hash := hashForTest(content)
	primary := &recordingReadBackend{
		content:     content,
		terminalErr: ErrPhysicalCorrupt,
	}
	secondary := &recordingReadBackend{content: content}
	resolver := &mutableLocationResolver{resolution: Resolution{
		Member: true,
		Candidates: []ReadLocation{
			{StoreID: "primary", Generation: "primary-1", Loose: rawLocation(content)},
			{StoreID: "secondary", Generation: "secondary-1", Loose: rawLocation(content)},
		},
	}}
	store, err := NewMultiStore(
		resolver,
		staticBackendRegistry{
			"primary":   primary,
			"secondary": secondary,
		},
		MultiStoreOptions{},
	)
	require.NoError(err)

	stream, _, err := store.OpenStream(t.Context(), hash)
	require.NoError(err)
	_, err = io.ReadAll(stream)
	require.ErrorIs(err, ErrPhysicalCorrupt)
	require.ErrorIs(stream.Close(), ErrPhysicalCorrupt)

	stream, _, err = store.OpenStream(t.Context(), hash)
	require.NoError(err)
	_, err = io.ReadAll(stream)
	require.NoError(err)
	require.NoError(stream.Close())

	primary.terminalErr = nil
	resolver.resolution.Candidates[0].Generation = "primary-2"
	stream, _, err = store.OpenStream(t.Context(), hash)
	require.NoError(err)
	_, err = io.ReadAll(stream)
	require.NoError(err)
	require.NoError(stream.Close())

	assert.Equal(2, primary.opens)
	assert.Equal(1, secondary.opens)
}

func TestMultiStoreExhaustedPrecedence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	hash := hashForTest([]byte("exhausted content"))
	resolution := Resolution{Member: true}
	backends := staticBackendRegistry{}
	failures := []struct {
		store StoreID
		err   error
	}{
		{store: "offline", err: ErrStoreUnavailable},
		{store: "fenced", err: ErrStoreFenced},
		{store: "missing", err: ErrPhysicalMissing},
		{store: "corrupt", err: ErrPhysicalCorrupt},
	}
	for _, failure := range failures {
		resolution.Candidates = append(resolution.Candidates, ReadLocation{
			StoreID: failure.store, Generation: "generation-1", Loose: rawLocation(nil),
		})
		backends[failure.store] = &recordingReadBackend{err: failure.err}
	}
	store, err := NewMultiStore(
		staticLocationResolver{resolution: resolution},
		backends,
		MultiStoreOptions{},
	)
	require.NoError(err)

	stream, size, err := store.OpenStream(t.Context(), hash)

	assert.Nil(stream)
	assert.Zero(size)
	var exhausted *ExhaustedError
	require.ErrorAs(err, &exhausted)
	require.NotNil(exhausted)
	require.ErrorIs(err, ErrPhysicalCorrupt)
	require.ErrorIs(err, ErrPhysicalMissing)
	require.ErrorIs(err, ErrStoreFenced)
	require.ErrorIs(err, ErrStoreUnavailable)
	assert.Equal(ErrPhysicalCorrupt, exhausted.Headline)
	require.Len(exhausted.Attempts, len(failures))
	for i, failure := range failures {
		assert.Equal(failure.store, exhausted.Attempts[i].Location.StoreID)
		assert.ErrorIs(exhausted.Attempts[i].Err, failure.err)
	}
}

func TestMultiStoreOpenReturnsVerifiedSeekableContent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := []byte("seekable multi-location content")
	hash := hashForTest(content)
	store, err := NewMultiStore(
		staticLocationResolver{resolution: Resolution{
			Member: true,
			Candidates: []ReadLocation{{
				StoreID: "archive", Generation: "archive-1", Loose: rawLocation(content),
			}},
		}},
		staticBackendRegistry{
			"archive": &recordingReadBackend{content: content},
		},
		MultiStoreOptions{},
	)
	require.NoError(err)

	reader, size, err := store.Open(t.Context(), hash)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(reader.Close()) })
	offset, err := reader.Seek(9, io.SeekStart)
	require.NoError(err)
	got, err := io.ReadAll(reader)
	require.NoError(err)

	assert.Equal(int64(len(content)), size)
	assert.Equal(int64(9), offset)
	assert.Equal(content[9:], got)
}

func TestMultiStoreReadBoundedVerifiesWithinLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := []byte("bounded multi-location content")
	hash := hashForTest(content)
	store, err := NewMultiStore(
		staticLocationResolver{resolution: Resolution{
			Member: true,
			Candidates: []ReadLocation{{
				StoreID: "archive", Generation: "archive-1", Loose: rawLocation(content),
			}},
		}},
		staticBackendRegistry{
			"archive": &recordingReadBackend{content: content},
		},
		MultiStoreOptions{},
	)
	require.NoError(err)

	got, size, err := store.ReadBounded(t.Context(), hash, int64(len(content)))
	require.NoError(err)
	assert.Equal(content, got)
	assert.Equal(int64(len(content)), size)

	got, size, err = store.ReadBounded(t.Context(), hash, int64(len(content)-1))
	assert.Nil(got)
	assert.Zero(size)
	var limitErr *LimitError
	require.ErrorAs(err, &limitErr)
	require.NotNil(limitErr)
	assert.Equal(LimitBlobRawBytes, limitErr.Dimension)
	assert.Equal(uint64(len(content)), limitErr.Actual)
}

func TestMultiStoreReadBoundedPreflightsCatalogStoredSize(t *testing.T) {
	content := []byte("data")
	hash := hashForTest(content)
	maxBytes := int64(len(content))
	storedBytes := maxBytes + 1
	packed := IndexEntry{
		Hash: hash, PackID: pack.NewPackID(), Offset: int64(pack.MinEntryOffset),
		StoredLen: storedBytes, RawLen: maxBytes,
	}
	locations := map[string]ReadLocation{
		"loose": {
			StoreID: "archive", Generation: "archive-1",
			Loose: &LooseLocation{
				Encoding: LooseEncodingZstd, LogicalSize: maxBytes, StoredSize: storedBytes,
			},
		},
		"packed": {
			StoreID: "archive", Generation: "archive-1", Pack: &packed,
		},
	}
	for name, location := range locations {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			backend := &recordingReadBackend{content: content}
			store, err := NewMultiStore(
				staticLocationResolver{resolution: Resolution{
					Member: true, Candidates: []ReadLocation{location},
				}},
				staticBackendRegistry{"archive": backend},
				MultiStoreOptions{},
			)
			require.NoError(err)

			got, size, err := store.ReadBounded(t.Context(), hash, maxBytes)

			assert.Nil(got)
			assert.Zero(size)
			var limitErr *LimitError
			require.ErrorAs(err, &limitErr)
			assert.Equal(LimitBlobStoredBytes, limitErr.Dimension)
			assert.Equal(uint64(storedBytes), limitErr.Actual)
			assert.Equal(uint64(maxBytes), limitErr.Limit)
			assert.Zero(backend.opens)
		})
	}
}

func TestMultiStoreReadBoundedFallsBackAfterStoredSizeLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := []byte("stored representation fallback")
	hash := hashForTest(content)
	maxBytes := int64(len(content))
	oversized := &recordingReadBackend{content: content}
	healthy := &recordingReadBackend{content: content}
	store, err := NewMultiStore(
		staticLocationResolver{resolution: Resolution{
			Member: true,
			Candidates: []ReadLocation{
				{
					StoreID: "oversized", Generation: "oversized-1",
					Loose: &LooseLocation{
						Encoding: LooseEncodingZstd, LogicalSize: maxBytes,
						StoredSize: maxBytes + 1,
					},
				},
				{
					StoreID: "healthy", Generation: "healthy-1",
					Loose: &LooseLocation{
						Encoding: LooseEncodingRaw, LogicalSize: maxBytes,
						StoredSize: maxBytes,
					},
				},
			},
		}},
		staticBackendRegistry{"oversized": oversized, "healthy": healthy},
		MultiStoreOptions{},
	)
	require.NoError(err)

	got, size, err := store.ReadBounded(t.Context(), hash, maxBytes)

	require.NoError(err)
	assert.Equal(content, got)
	assert.Equal(maxBytes, size)
	assert.Zero(oversized.opens)
	assert.Equal(1, healthy.opens)
}

func TestMultiStoreRejectsMismatchedPackedCandidate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := []byte("requested content")
	hash := hashForTest(content)
	otherHash := hashForTest([]byte("different content"))
	entry := IndexEntry{
		Hash: otherHash, PackID: pack.NewPackID(), Offset: int64(pack.MinEntryOffset),
		StoredLen: 1, RawLen: 1,
	}
	backend := &recordingReadBackend{content: content}
	store, err := NewMultiStore(
		staticLocationResolver{resolution: Resolution{
			Member: true,
			Candidates: []ReadLocation{
				{StoreID: "archive", Generation: "archive-1", Loose: rawLocation(content)},
				{StoreID: "archive", Generation: "archive-2", Pack: &entry},
			},
		}},
		staticBackendRegistry{"archive": backend},
		MultiStoreOptions{},
	)
	require.NoError(err)

	stream, size, err := store.OpenStream(t.Context(), hash)

	assert.Nil(stream)
	assert.Zero(size)
	require.ErrorContains(err, "candidate hash")
	assert.Zero(backend.opens)
}

func TestMultiStoreOpenRejectsNilBackendStream(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	hash := hashForTest([]byte("nil backend stream"))
	store, err := NewMultiStore(
		staticLocationResolver{resolution: Resolution{
			Member: true,
			Candidates: []ReadLocation{{
				StoreID: "archive", Generation: "archive-1", Loose: rawLocation(nil),
			}},
		}},
		staticBackendRegistry{
			"archive": &recordingReadBackend{nilStream: true},
		},
		MultiStoreOptions{},
	)
	require.NoError(err)

	reader, size, err := store.Open(t.Context(), hash)

	assert.Nil(reader)
	assert.Zero(size)
	assert.ErrorContains(err, "nil verified stream")
}

func TestMultiStoreRejectsUnknownLooseEncoding(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	hash := hashForTest([]byte("unknown encoding"))
	backend := &recordingReadBackend{content: []byte("unknown encoding")}
	store, err := NewMultiStore(
		staticLocationResolver{resolution: Resolution{
			Member: true,
			Candidates: []ReadLocation{{
				StoreID: "archive", Generation: "archive-1",
				Loose: &LooseLocation{Encoding: LooseEncoding(255)},
			}},
		}},
		staticBackendRegistry{"archive": backend},
		MultiStoreOptions{},
	)
	require.NoError(err)

	stream, size, err := store.OpenStream(t.Context(), hash)

	assert.Nil(stream)
	assert.Zero(size)
	require.ErrorContains(err, "loose encoding")
	assert.Zero(backend.opens)
}

func TestMultiStoreRejectsAmbiguousLooseLocation(t *testing.T) {
	content := []byte("ambiguous loose authority")
	hash := hashForTest(content)
	backend := &recordingReadBackend{content: content}
	store, err := NewMultiStore(
		staticLocationResolver{resolution: Resolution{
			Member: true,
			Candidates: []ReadLocation{{
				StoreID: "archive", Generation: "archive-1",
				Loose: &LooseLocation{},
			}},
		}},
		staticBackendRegistry{"archive": backend},
		MultiStoreOptions{},
	)
	require.NoError(t, err)

	stream, _, err := store.OpenStream(t.Context(), hash)
	if stream != nil {
		t.Cleanup(func() { _ = stream.Close() })
	}

	require.ErrorIs(t, err, ErrInvalidPolicy)
	assert.Zero(t, backend.opens)
}

func TestMultiStoreRejectsBackendSizeMismatch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := []byte("size mismatch")
	hash := hashForTest(content)
	store, err := NewMultiStore(
		staticLocationResolver{resolution: Resolution{
			Member: true,
			Candidates: []ReadLocation{{
				StoreID: "archive", Generation: "archive-1",
				Loose: &LooseLocation{
					Encoding:    LooseEncodingRaw,
					LogicalSize: int64(len(content) + 1),
					StoredSize:  int64(len(content) + 1),
				},
			}},
		}},
		staticBackendRegistry{
			"archive": &recordingReadBackend{content: content},
		},
		MultiStoreOptions{},
	)
	require.NoError(err)

	stream, size, err := store.OpenStream(t.Context(), hash)

	assert.Nil(stream)
	assert.Zero(size)
	require.ErrorIs(err, ErrPhysicalCorrupt)
	assert.ErrorContains(err, "logical size")
}

func rawLocation(content []byte) *LooseLocation {
	size := int64(len(content))
	return &LooseLocation{
		Encoding: LooseEncodingRaw, LogicalSize: size, StoredSize: size,
	}
}

type staticLocationResolver struct {
	resolution Resolution
	err        error
}

type mutableLocationResolver struct {
	resolution Resolution
}

type changingLocationResolver struct {
	resolutions []Resolution
	calls       int
}

func (r *changingLocationResolver) ResolveLocations(
	context.Context,
	Hash,
) (Resolution, error) {
	index := min(r.calls, len(r.resolutions)-1)
	r.calls++
	return r.resolutions[index], nil
}

func (r *mutableLocationResolver) ResolveLocations(context.Context, Hash) (Resolution, error) {
	return r.resolution, nil
}

func (r staticLocationResolver) ResolveLocations(context.Context, Hash) (Resolution, error) {
	return r.resolution, r.err
}

type staticBackendRegistry map[StoreID]ReadBackend

func (r staticBackendRegistry) Backend(id StoreID) (ReadBackend, bool) {
	backend, ok := r[id]
	return backend, ok
}

type recordingReadBackend struct {
	content     []byte
	err         error
	terminalErr error
	nilStream   bool
	opens       int
}

func (b *recordingReadBackend) OpenLoose(
	context.Context,
	Hash,
	LooseLocation,
) (VerifiedReadCloser, int64, error) {
	b.opens++
	if b.err != nil {
		return nil, 0, b.err
	}
	if b.nilStream {
		return nil, 0, nil
	}
	return &testVerifiedReader{
		Reader:      bytes.NewReader(b.content),
		terminalErr: b.terminalErr,
	}, int64(len(b.content)), nil
}

func (b *recordingReadBackend) OpenPack(
	context.Context,
	Hash,
	IndexEntry,
) (VerifiedReadCloser, int64, error) {
	b.opens++
	if b.err != nil {
		return nil, 0, b.err
	}
	if b.nilStream {
		return nil, 0, nil
	}
	return &testVerifiedReader{
		Reader:      bytes.NewReader(b.content),
		terminalErr: b.terminalErr,
	}, int64(len(b.content)), nil
}

type testVerifiedReader struct {
	*bytes.Reader
	terminalErr error
	terminal    error
	verified    bool
}

func (r *testVerifiedReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.EOF {
		if r.terminalErr != nil {
			r.terminal = r.terminalErr
			return n, r.terminal
		}
		r.verified = true
	}
	return n, err
}

func (r *testVerifiedReader) Verify() error {
	if r.terminal != nil {
		return r.terminal
	}
	_, err := io.Copy(io.Discard, r)
	return err
}

func (r *testVerifiedReader) Verified() bool { return r.verified }

func (r *testVerifiedReader) Close() error { return r.terminal }
