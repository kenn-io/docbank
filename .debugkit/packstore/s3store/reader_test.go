package s3store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

func TestDownloadPackRangesRejectsOversizedObjectBeforeGET(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	limits := packstore.DefaultLimits()
	limits.PackBytes = 10
	var getRequests int
	backend := newHTTPBackend(limits, func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodHead {
			header := make(http.Header)
			header.Set("Content-Length", strconv.FormatInt(limits.PackBytes+1, 10))
			return &http.Response{
				StatusCode:    http.StatusOK,
				Header:        header,
				Body:          io.NopCloser(bytes.NewReader(nil)),
				ContentLength: limits.PackBytes + 1,
				Request:       request,
			}, nil
		}
		getRequests++
		return nil, errors.New("unexpected range GET")
	})

	_, _, err := backend.downloadPackRanges(
		t.Context(),
		"0123456789abcdef0123456789abcdef",
	)

	require.ErrorIs(err, packstore.ErrBlobTooLarge)
	require.ErrorIs(err, packstore.ErrPhysicalCorrupt)
	var limit *packstore.LimitError
	require.ErrorAs(err, &limit)
	assert.Equal(packstore.LimitPackContainerBytes, limit.Dimension)
	assert.Zero(getRequests)
}

func TestDownloadPackRangesPreservesCancellation(t *testing.T) {
	for _, terminal := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(terminal.Error(), func(t *testing.T) {
			backend := newHTTPBackend(packstore.DefaultLimits(), func(request *http.Request) (*http.Response, error) {
				header := make(http.Header)
				header.Set("Content-Length", "1")
				if request.Method == http.MethodHead {
					return &http.Response{
						StatusCode: http.StatusOK, Header: header,
						Body: io.NopCloser(bytes.NewReader(nil)), ContentLength: 1,
						Request: request,
					}, nil
				}
				header.Set("Content-Range", "bytes 0-0/1")
				return &http.Response{
					StatusCode: http.StatusPartialContent, Header: header,
					Body: &failingRangeBody{err: terminal}, ContentLength: 1,
					Request: request,
				}, nil
			})

			_, _, err := backend.downloadPackRanges(
				t.Context(),
				"0123456789abcdef0123456789abcdef",
			)

			require.ErrorIs(t, err, terminal)
			require.NotErrorIs(t, err, packstore.ErrPhysicalCorrupt)
		})
	}
}

func TestPackReaderOptionsUseConfiguredLimits(t *testing.T) {
	backend := &Backend{limits: packstore.Limits{
		BlobBytes: 4096, PackBytes: 8192, FooterBytes: 2048, PackEntries: 32,
	}}

	assert.Equal(t, pack.ReaderOptions{Limits: pack.ReaderLimits{
		ContainerBytes: 8192,
		FooterBytes:    2048,
		Entries:        32,
		WindowBytes:    4096,
	}}, backend.packReaderOptions())
}

func TestOpenRequiresAttachedOwnership(t *testing.T) {
	content := []byte("unattached reads are fenced")
	_, _, entry := makePack(t, content)
	var requests int
	backend := newHTTPBackend(packstore.DefaultLimits(), func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("unexpected request from unattached backend")
	})

	_, _, err := backend.OpenLoose(
		t.Context(),
		entry.Hash,
		packstore.LooseLocation{
			Encoding:    packstore.LooseEncodingRaw,
			LogicalSize: int64(len(content)),
			StoredSize:  int64(len(content)),
		},
	)
	require.ErrorIs(t, err, packstore.ErrStoreFenced)

	_, _, err = backend.OpenPack(t.Context(), entry.Hash, entry)
	require.ErrorIs(t, err, packstore.ErrStoreFenced)
	assert.Zero(t, requests)
}

func TestOpenPackEnforcesConfiguredBlobLimit(t *testing.T) {
	require := require.New(t)
	content := []byte("blob exceeds configured S3 reader limit")
	_, packBytes, indexed := makePack(t, content)
	limits := packstore.DefaultLimits()
	limits.BlobBytes = int64(len(content) - 1)
	backend := newHTTPBackend(limits, func(request *http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("Content-Length", strconv.Itoa(len(packBytes)))
		if request.Method == http.MethodHead {
			return &http.Response{
				StatusCode: http.StatusOK, Header: header,
				Body:          io.NopCloser(bytes.NewReader(nil)),
				ContentLength: int64(len(packBytes)), Request: request,
			}, nil
		}
		header.Set(
			"Content-Range",
			"bytes 0-"+strconv.Itoa(len(packBytes)-1)+"/"+strconv.Itoa(len(packBytes)),
		)
		return &http.Response{
			StatusCode: http.StatusPartialContent, Header: header,
			Body:          io.NopCloser(bytes.NewReader(packBytes)),
			ContentLength: int64(len(packBytes)), Request: request,
		}, nil
	})
	attachTestBackend(backend)

	_, _, err := backend.OpenPack(
		t.Context(),
		indexed.Hash,
		indexed,
	)

	require.ErrorIs(err, packstore.ErrBlobTooLarge)
	require.NotErrorIs(err, packstore.ErrPhysicalCorrupt)
	var limit *packstore.LimitError
	require.ErrorAs(err, &limit)
	assert.Equal(t, packstore.LimitBlobRawBytes, limit.Dimension)
}

func TestOpenPackIgnoresUnselectedEntryBlobLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	selected := []byte("selected blob")
	unselected := bytes.Repeat([]byte("unrelated oversized footer entry"), 32)
	_, packBytes, entries := makePackEntries(t, selected, unselected)
	limits := packstore.DefaultLimits()
	limits.BlobBytes = int64(len(selected))
	backend := newHTTPBackend(limits, func(request *http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("Content-Length", strconv.Itoa(len(packBytes)))
		if request.Method == http.MethodHead {
			return &http.Response{
				StatusCode: http.StatusOK, Header: header,
				Body: io.NopCloser(bytes.NewReader(nil)), ContentLength: int64(len(packBytes)),
				Request: request,
			}, nil
		}
		header.Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(packBytes)-1, len(packBytes)))
		return &http.Response{
			StatusCode: http.StatusPartialContent, Header: header,
			Body: io.NopCloser(bytes.NewReader(packBytes)), ContentLength: int64(len(packBytes)),
			Request: request,
		}, nil
	})
	attachTestBackend(backend)

	stream, size, err := backend.OpenPack(t.Context(), entries[0].Hash, entries[0])
	require.NoError(err)
	got, err := io.ReadAll(stream)
	require.NoError(errors.Join(err, stream.Close()))
	assert.Equal(selected, got)
	assert.Equal(int64(len(selected)), size)
}

func TestS3DuplicatePackEntriesFallBackToHealthyCandidate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := []byte("duplicate S3 footer fallback")
	_, packBytes, entries := makePackEntries(t, content, content)
	corrupt := newHTTPBackend(packstore.DefaultLimits(), func(request *http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("Content-Length", strconv.Itoa(len(packBytes)))
		if request.Method == http.MethodHead {
			return &http.Response{
				StatusCode: http.StatusOK, Header: header,
				Body: io.NopCloser(bytes.NewReader(nil)), ContentLength: int64(len(packBytes)),
				Request: request,
			}, nil
		}
		header.Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(packBytes)-1, len(packBytes)))
		return &http.Response{
			StatusCode: http.StatusPartialContent, Header: header,
			Body: io.NopCloser(bytes.NewReader(packBytes)), ContentLength: int64(len(packBytes)),
			Request: request,
		}, nil
	})
	attachTestBackend(corrupt)
	healthy := &memoryReadBackend{content: content}
	store, err := packstore.NewMultiStore(
		staticReadLocationResolver{resolution: packstore.Resolution{
			Member: true,
			Candidates: []packstore.ReadLocation{
				{StoreID: "corrupt", Generation: "corrupt-1", Pack: &entries[0]},
				{StoreID: "healthy", Generation: "healthy-1", Pack: &entries[0]},
			},
		}},
		staticReadBackendRegistry{"corrupt": corrupt, "healthy": healthy},
		packstore.MultiStoreOptions{},
	)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(store.Close()) })

	stream, size, err := store.OpenStream(t.Context(), entries[0].Hash)
	require.NoError(err)
	got, err := io.ReadAll(stream)
	require.NoError(errors.Join(err, stream.Close()))
	assert.Equal(content, got)
	assert.Equal(int64(len(content)), size)
	assert.Equal(1, healthy.opens)

	_, _, err = corrupt.OpenPack(t.Context(), entries[0].Hash, entries[0])
	require.ErrorIs(err, packstore.ErrPhysicalCorrupt)
	require.ErrorIs(err, pack.ErrCorrupt)
}

func TestOversizedS3ReplicaFallsBackToHealthyCandidate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := []byte("healthy replica content")
	_, packBytes, indexed := makePack(t, content)
	limits := packstore.DefaultLimits()
	limits.PackBytes = int64(len(packBytes) - 1)
	oversized := newHTTPBackend(limits, func(request *http.Request) (*http.Response, error) {
		require.Equal(http.MethodHead, request.Method)
		header := make(http.Header)
		header.Set("Content-Length", strconv.Itoa(len(packBytes)))
		return &http.Response{
			StatusCode: http.StatusOK, Header: header,
			Body:          io.NopCloser(bytes.NewReader(nil)),
			ContentLength: int64(len(packBytes)), Request: request,
		}, nil
	})
	attachTestBackend(oversized)
	healthy := &memoryReadBackend{content: content}
	store, err := packstore.NewMultiStore(
		staticReadLocationResolver{resolution: packstore.Resolution{
			Member: true,
			Candidates: []packstore.ReadLocation{
				{StoreID: "oversized", Generation: "oversized-1", Pack: &indexed},
				{StoreID: "healthy", Generation: "healthy-1", Pack: &indexed},
			},
		}},
		staticReadBackendRegistry{
			"oversized": oversized,
			"healthy":   healthy,
		},
		packstore.MultiStoreOptions{},
	)
	require.NoError(err)

	stream, size, err := store.OpenStream(t.Context(), indexed.Hash)
	require.NoError(err)
	got, err := io.ReadAll(stream)
	require.NoError(err)
	require.NoError(stream.Close())

	assert.Equal(int64(len(content)), size)
	assert.Equal(content, got)
	assert.Equal(1, healthy.opens)
}

func TestS3PackRepresentationLimitsFallBackToHealthyCandidate(t *testing.T) {
	content := []byte("healthy S3 representation fallback")
	_, packBytes, entries := makePackEntries(t, content, []byte("second footer entry"))
	tests := []struct {
		name      string
		dimension packstore.LimitDimension
		limit     func(packstore.Limits) packstore.Limits
	}{
		{
			name:      "footer bytes",
			dimension: packstore.LimitPackFooterBytes,
			limit: func(limits packstore.Limits) packstore.Limits {
				limits.FooterBytes = 1
				return limits
			},
		},
		{
			name:      "entry count",
			dimension: packstore.LimitPackEntryCount,
			limit: func(limits packstore.Limits) packstore.Limits {
				limits.PackEntries = 1
				return limits
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			limited := newHTTPBackend(tt.limit(packstore.DefaultLimits()), func(request *http.Request) (*http.Response, error) {
				header := make(http.Header)
				header.Set("Content-Length", strconv.Itoa(len(packBytes)))
				if request.Method == http.MethodHead {
					return &http.Response{
						StatusCode: http.StatusOK, Header: header,
						Body: io.NopCloser(bytes.NewReader(nil)), ContentLength: int64(len(packBytes)),
						Request: request,
					}, nil
				}
				header.Set(
					"Content-Range",
					"bytes 0-"+strconv.Itoa(len(packBytes)-1)+"/"+strconv.Itoa(len(packBytes)),
				)
				return &http.Response{
					StatusCode: http.StatusPartialContent, Header: header,
					Body: io.NopCloser(bytes.NewReader(packBytes)), ContentLength: int64(len(packBytes)),
					Request: request,
				}, nil
			})
			attachTestBackend(limited)
			healthy := &memoryReadBackend{content: content}
			store, err := packstore.NewMultiStore(
				staticReadLocationResolver{resolution: packstore.Resolution{
					Member: true,
					Candidates: []packstore.ReadLocation{
						{StoreID: "limited", Generation: "limited-1", Pack: &entries[0]},
						{StoreID: "healthy", Generation: "healthy-1", Pack: &entries[0]},
					},
				}},
				staticReadBackendRegistry{"limited": limited, "healthy": healthy},
				packstore.MultiStoreOptions{},
			)
			require.NoError(err)
			t.Cleanup(func() { require.NoError(store.Close()) })

			stream, size, err := store.OpenStream(t.Context(), entries[0].Hash)
			require.NoError(err)
			got, err := io.ReadAll(stream)
			require.NoError(errors.Join(err, stream.Close()))
			assert.Equal(content, got)
			assert.Equal(int64(len(content)), size)

			_, _, err = limited.OpenPack(t.Context(), entries[0].Hash, entries[0])
			require.ErrorIs(err, packstore.ErrPhysicalCorrupt)
			require.ErrorIs(err, packstore.ErrBlobTooLarge)
			var limit *packstore.LimitError
			require.ErrorAs(err, &limit)
			assert.Equal(tt.dimension, limit.Dimension)
		})
	}
}

func TestPackBodyClassifiesTerminalReadCorruption(t *testing.T) {
	body, _ := newPackBody(t, true)

	_, err := io.ReadAll(body)

	require.ErrorIs(t, err, pack.ErrCorrupt)
	require.ErrorIs(t, err, packstore.ErrPhysicalCorrupt)
}

func TestPackBodyClassifiesTerminalVerifyCorruption(t *testing.T) {
	body, _ := newPackBody(t, true)

	err := body.Verify()

	require.ErrorIs(t, err, pack.ErrCorrupt)
	require.ErrorIs(t, err, packstore.ErrPhysicalCorrupt)
}

func TestPackBodyDoesNotClassifyIncompleteCloseAsCorrupt(t *testing.T) {
	body, _ := newPackBody(t, false)

	err := body.Close()

	require.ErrorIs(t, err, pack.ErrVerificationIncomplete)
	require.NotErrorIs(t, err, packstore.ErrPhysicalCorrupt)
}

func TestPackBodyPreservesVerifiedEOF(t *testing.T) {
	body, _ := newPackBody(t, false)

	got, err := io.ReadAll(body)

	require.NoError(t, err)
	assert.Equal(t, []byte("terminal S3 pack integrity"), got)
	assert.True(t, body.Verified())
}

func TestS3TerminalCorruptionDemotesGeneration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	body, indexed := newPackBody(t, true)
	primary := packstore.ReadLocation{
		StoreID: "primary", Generation: "primary-1", Pack: &indexed,
	}
	secondary := primary
	secondary.StoreID = "secondary"
	secondary.Generation = "secondary-1"
	health := packstore.NewHealth()

	err := body.Verify()
	require.ErrorIs(err, packstore.ErrPhysicalCorrupt)
	health.Observe(indexed.Hash, primary, err)

	ordered := health.Order(indexed.Hash, []packstore.ReadLocation{primary, secondary})
	require.Len(ordered, 2)
	assert.Equal(secondary, ordered[0])
	assert.Equal(primary, ordered[1])
}

func newPackBody(
	t *testing.T,
	corrupt bool,
) (*packBody, packstore.IndexEntry) {
	t.Helper()
	content := []byte("terminal S3 pack integrity")
	packID, packBytes, indexed := makePack(t, content)
	if corrupt {
		packBytes[indexed.Offset] ^= 0xff
	}
	path := filepath.Join(t.TempDir(), packID+".pack")
	require.NoError(t, os.WriteFile(path, packBytes, 0o600))
	reader, err := pack.OpenReader(path, nil)
	require.NoError(t, err)
	blob, err := reader.OpenBlob(t.Context(), reader.Entries()[0])
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = blob.Close()
		_ = reader.Close()
		_ = os.Remove(path)
	})
	return &packBody{blob: blob, reader: reader, path: path}, indexed
}

type staticReadLocationResolver struct {
	resolution packstore.Resolution
}

func (r staticReadLocationResolver) ResolveLocations(
	context.Context,
	packstore.Hash,
) (packstore.Resolution, error) {
	return r.resolution, nil
}

type staticReadBackendRegistry map[packstore.StoreID]packstore.ReadBackend

func (r staticReadBackendRegistry) Backend(
	id packstore.StoreID,
) (packstore.ReadBackend, bool) {
	backend, ok := r[id]
	return backend, ok
}

type memoryReadBackend struct {
	content []byte
	opens   int
}

func (b *memoryReadBackend) OpenLoose(
	context.Context,
	packstore.Hash,
	packstore.LooseLocation,
) (packstore.VerifiedReadCloser, int64, error) {
	return b.open()
}

func (b *memoryReadBackend) OpenPack(
	context.Context,
	packstore.Hash,
	packstore.IndexEntry,
) (packstore.VerifiedReadCloser, int64, error) {
	return b.open()
}

func (b *memoryReadBackend) open() (packstore.VerifiedReadCloser, int64, error) {
	b.opens++
	return &memoryVerifiedReader{
		Reader: bytes.NewReader(b.content),
	}, int64(len(b.content)), nil
}

type memoryVerifiedReader struct {
	*bytes.Reader
	verified bool
}

type failingRangeBody struct{ err error }

func (b *failingRangeBody) Read([]byte) (int, error) { return 0, b.err }
func (b *failingRangeBody) Close() error             { return nil }

func (r *memoryVerifiedReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if errors.Is(err, io.EOF) {
		r.verified = true
	}
	return n, err
}

func (r *memoryVerifiedReader) Verify() error {
	_, err := io.Copy(io.Discard, r)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func (r *memoryVerifiedReader) Verified() bool { return r.verified }

func (r *memoryVerifiedReader) Close() error {
	if r.verified {
		return nil
	}
	return pack.ErrVerificationIncomplete
}

func TestDownloadPackRangesClassifiesShortResponse(t *testing.T) {
	backend := newHTTPBackend(packstore.DefaultLimits(), func(request *http.Request) (*http.Response, error) {
		header := make(http.Header)
		if request.Method == http.MethodHead {
			header.Set("Content-Length", "2")
			return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(bytes.NewReader(nil)), ContentLength: 2, Request: request}, nil
		}
		header.Set("Content-Length", "1")
		header.Set("Content-Range", "bytes 0-1/2")
		return &http.Response{StatusCode: http.StatusPartialContent, Header: header, Body: io.NopCloser(bytes.NewReader([]byte("x"))), ContentLength: 1, Request: request}, nil
	})
	_, _, err := backend.downloadPackRanges(t.Context(), "0123456789abcdef0123456789abcdef")
	require.ErrorIs(t, err, packstore.ErrPhysicalCorrupt)
	require.NotErrorIs(t, err, packstore.ErrStoreUnavailable)
}
