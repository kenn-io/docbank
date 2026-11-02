package s3store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

func TestVerifyPackObjectRejectsMismatchedContentLengthBeforeRead(t *testing.T) {
	body := &countingReadCloser{reader: bytes.NewReader(bytes.Repeat([]byte("x"), 100))}
	backend := newHTTPBackend(packstore.DefaultLimits(), func(request *http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("Content-Length", "100")
		return &http.Response{
			StatusCode: http.StatusOK, Header: header, Body: body,
			ContentLength: 100, Request: request,
		}, nil
	})

	_, _, err := backend.verifyPackObject(
		t.Context(),
		"0123456789abcdef0123456789abcdef",
		1,
	)

	require.ErrorIs(t, err, packstore.ErrPhysicalCorrupt)
	assert.Zero(t, body.read)
}

func TestVerifyPackObjectBoundsReadWhenContentLengthMissing(t *testing.T) {
	const expectedSize = int64(1)
	body := &countingReadCloser{reader: bytes.NewReader(bytes.Repeat([]byte("x"), 100))}
	backend := newHTTPBackend(packstore.DefaultLimits(), func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header), Body: body,
			ContentLength: -1, Request: request,
		}, nil
	})

	_, _, err := backend.verifyPackObject(
		t.Context(),
		"0123456789abcdef0123456789abcdef",
		expectedSize,
	)

	require.ErrorIs(t, err, packstore.ErrPhysicalCorrupt)
	assert.LessOrEqual(t, body.read, expectedSize+1)
}

func TestVerifyRawObjectBoundsReadWhenContentLengthMissing(t *testing.T) {
	const expectedSize = int64(1)
	body := &countingReadCloser{reader: bytes.NewReader(bytes.Repeat([]byte("x"), 100))}
	backend := newHTTPBackend(packstore.DefaultLimits(), func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header), Body: body,
			ContentLength: -1, Request: request,
		}, nil
	})

	err := backend.verifyRawObject(
		t.Context(),
		"loose/test",
		packstore.Hash("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"),
		expectedSize,
	)

	require.ErrorIs(t, err, packstore.ErrPhysicalCorrupt)
	assert.LessOrEqual(t, body.read, expectedSize+1)
}

func TestVerifyPackObjectEnforcesConfiguredBlobLimit(t *testing.T) {
	content := []byte("published blob exceeds configured verification limit")
	packID, packBytes, _ := makePack(t, content)
	limits := packstore.DefaultLimits()
	limits.BlobBytes = int64(len(content) - 1)
	backend := newHTTPBackend(limits, func(request *http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("Content-Length", strconv.Itoa(len(packBytes)))
		return &http.Response{
			StatusCode: http.StatusOK, Header: header,
			Body:          io.NopCloser(bytes.NewReader(packBytes)),
			ContentLength: int64(len(packBytes)), Request: request,
		}, nil
	})

	_, _, err := backend.verifyPackObject(
		t.Context(),
		packID,
		int64(len(packBytes)),
	)

	require.ErrorIs(t, err, packstore.ErrBlobTooLarge)
	var limit *packstore.LimitError
	require.ErrorAs(t, err, &limit)
	assert.Equal(t, packstore.LimitBlobRawBytes, limit.Dimension)
}

func TestVerifyPackObjectEnforcesConfiguredDecoderWindowLimit(t *testing.T) {
	content := bytes.Repeat([]byte("window policy "), 1<<16)
	packID, packBytes := makeEncodedPack(
		t,
		content,
		uint64(len(content)),
		8<<20,
	)
	limits := packstore.DefaultLimits()
	limits.BlobBytes = 2 << 20
	backend := newHTTPBackend(limits, func(request *http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("Content-Length", strconv.Itoa(len(packBytes)))
		return &http.Response{
			StatusCode: http.StatusOK, Header: header,
			Body:          io.NopCloser(bytes.NewReader(packBytes)),
			ContentLength: int64(len(packBytes)), Request: request,
		}, nil
	})

	_, _, err := backend.verifyPackObject(
		t.Context(),
		packID,
		int64(len(packBytes)),
	)

	require.ErrorIs(t, err, packstore.ErrBlobTooLarge)
	var limit *packstore.LimitError
	require.ErrorAs(t, err, &limit)
	assert.Equal(t, packstore.LimitBlobWindowBytes, limit.Dimension)
}

func TestVerifyPackObjectRejectsDecodedLengthMismatch(t *testing.T) {
	content := bytes.Repeat([]byte("remote decoded length authority "), 4096)
	tests := []struct {
		name   string
		rawLen uint64
	}{
		{name: "decoded content is longer", rawLen: uint64(len(content) - 1)},
		{name: "decoded content is shorter", rawLen: uint64(len(content) + 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			packID, packBytes := makeEncodedPack(t, content, tt.rawLen, 0)
			backend := newHTTPBackend(
				packstore.DefaultLimits(),
				func(request *http.Request) (*http.Response, error) {
					header := make(http.Header)
					header.Set("Content-Length", strconv.Itoa(len(packBytes)))
					return &http.Response{
						StatusCode: http.StatusOK, Header: header,
						Body:          io.NopCloser(bytes.NewReader(packBytes)),
						ContentLength: int64(len(packBytes)), Request: request,
					}, nil
				},
			)

			_, _, err := backend.verifyPackObject(
				t.Context(),
				packID,
				int64(len(packBytes)),
			)

			require.ErrorIs(t, err, pack.ErrCorrupt)
			require.ErrorIs(t, err, packstore.ErrPhysicalCorrupt)
		})
	}
}

func TestPublishPackRejectsKnownConfiguredLimitBeforeMultipart(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	limits := packstore.DefaultLimits()
	limits.PackBytes = 8
	owner := packstore.Ownership{
		Format: packstore.OwnershipFormatV1,
		Vault:  "test-vault",
		Store:  "archive",
		Epoch:  "epoch-1",
	}
	marker, err := packstore.MarshalOwnership(owner)
	require.NoError(err)
	var requests int
	backend := newHTTPBackend(limits, func(request *http.Request) (*http.Response, error) {
		requests++
		if requests == 1 && request.Method == http.MethodGet {
			header := make(http.Header)
			header.Set("Content-Length", strconv.Itoa(len(marker)))
			return &http.Response{
				StatusCode: http.StatusOK, Header: header,
				Body:          io.NopCloser(bytes.NewReader(marker)),
				ContentLength: int64(len(marker)), Request: request,
			}, nil
		}
		return xmlResponse(
			request,
			http.StatusInternalServerError,
			`<Error><Code>UnexpectedRequest</Code></Error>`,
		), nil
	})
	backend.setOwnership(owner, `"owner-etag"`)
	source := &countingReadCloser{
		reader: bytes.NewReader(bytes.Repeat([]byte("x"), 9)),
	}

	_, err = backend.PublishPack(
		t.Context(),
		pack.NewPackID(),
		source,
		packstore.PublishOptions{
			ExpectedSize: 9, SizeKnown: true, MaxBytes: 100,
		},
	)

	require.ErrorIs(err, packstore.ErrBlobTooLarge)
	var limit *packstore.LimitError
	require.ErrorAs(err, &limit)
	assert.Equal(packstore.LimitPackContainerBytes, limit.Dimension)
	assert.Zero(source.read)
	assert.Equal(1, requests)
}

func TestPublishPackCapsCallerLimitBeforeMultipart(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	limits := packstore.DefaultLimits()
	limits.PackBytes = 8
	owner := packstore.Ownership{
		Format: packstore.OwnershipFormatV1,
		Vault:  "test-vault",
		Store:  "archive",
		Epoch:  "epoch-1",
	}
	marker, err := packstore.MarshalOwnership(owner)
	require.NoError(err)
	var creates, uploads, completes, aborts int
	backend := newHTTPBackend(limits, func(request *http.Request) (*http.Response, error) {
		query := request.URL.Query()
		switch {
		case request.Method == http.MethodGet:
			header := make(http.Header)
			header.Set("Content-Length", strconv.Itoa(len(marker)))
			return &http.Response{
				StatusCode: http.StatusOK, Header: header,
				Body:          io.NopCloser(bytes.NewReader(marker)),
				ContentLength: int64(len(marker)), Request: request,
			}, nil
		case request.Method == http.MethodPost && query.Has("uploads"):
			creates++
			return xmlResponse(request, http.StatusOK,
				`<InitiateMultipartUploadResult>`+
					`<Bucket>test-bucket</Bucket><Key>packs/test</Key>`+
					`<UploadId>upload-1</UploadId>`+
					`</InitiateMultipartUploadResult>`), nil
		case request.Method == http.MethodPut && query.Get("uploadId") == "upload-1":
			uploads++
			response := xmlResponse(request, http.StatusOK, "")
			response.Header.Set("ETag", `"part-etag"`)
			return response, nil
		case request.Method == http.MethodPost && query.Get("uploadId") == "upload-1":
			completes++
			return xmlResponse(request, http.StatusOK, ""), nil
		case request.Method == http.MethodDelete && query.Get("uploadId") == "upload-1":
			aborts++
			return xmlResponse(request, http.StatusNoContent, ""), nil
		default:
			return xmlResponse(request, http.StatusInternalServerError,
				`<Error><Code>UnexpectedRequest</Code></Error>`), nil
		}
	})
	backend.setOwnership(owner, `"owner-etag"`)

	_, err = backend.PublishPack(
		t.Context(),
		pack.NewPackID(),
		bytes.NewReader(bytes.Repeat([]byte("x"), 9)),
		packstore.PublishOptions{MaxBytes: 100},
	)

	require.ErrorIs(err, packstore.ErrBlobTooLarge)
	var limit *packstore.LimitError
	require.ErrorAs(err, &limit)
	assert.Equal(packstore.LimitPackContainerBytes, limit.Dimension)
	assert.Zero(creates)
	assert.Zero(uploads)
	assert.Zero(completes)
	assert.Zero(aborts)
}

func TestPublishPackRejectsExactSizeMismatchBeforeMultipart(t *testing.T) {
	owner := packstore.Ownership{
		Format: packstore.OwnershipFormatV1,
		Vault:  "test-vault",
		Store:  "archive",
		Epoch:  "epoch-1",
	}
	marker, err := packstore.MarshalOwnership(owner)
	require.NoError(t, err)
	for _, tt := range []struct {
		name         string
		expectedSize int64
	}{
		{name: "source is short", expectedSize: 6},
		{name: "source is overlong", expectedSize: 4},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			var creates, uploads, completes, aborts int
			backend := newHTTPBackend(packstore.DefaultLimits(), func(request *http.Request) (*http.Response, error) {
				query := request.URL.Query()
				switch {
				case request.Method == http.MethodGet:
					header := make(http.Header)
					header.Set("Content-Length", strconv.Itoa(len(marker)))
					return &http.Response{
						StatusCode: http.StatusOK, Header: header,
						Body:          io.NopCloser(bytes.NewReader(marker)),
						ContentLength: int64(len(marker)), Request: request,
					}, nil
				case request.Method == http.MethodPost && query.Has("uploads"):
					creates++
					return xmlResponse(request, http.StatusOK,
						`<InitiateMultipartUploadResult>`+
							`<Bucket>test-bucket</Bucket><Key>packs/test</Key>`+
							`<UploadId>upload-1</UploadId>`+
							`</InitiateMultipartUploadResult>`), nil
				case request.Method == http.MethodPut && query.Get("uploadId") == "upload-1":
					uploads++
					response := xmlResponse(request, http.StatusOK, "")
					response.Header.Set("ETag", `"part-etag"`)
					return response, nil
				case request.Method == http.MethodPost && query.Get("uploadId") == "upload-1":
					completes++
					return xmlResponse(request, http.StatusOK, ""), nil
				case request.Method == http.MethodDelete && query.Get("uploadId") == "upload-1":
					aborts++
					return xmlResponse(request, http.StatusNoContent, ""), nil
				default:
					return xmlResponse(request, http.StatusInternalServerError,
						`<Error><Code>UnexpectedRequest</Code></Error>`), nil
				}
			})
			backend.setOwnership(owner, `"owner-etag"`)

			_, err := backend.PublishPack(
				t.Context(),
				pack.NewPackID(),
				strings.NewReader("hello"),
				packstore.PublishOptions{ExpectedSize: tt.expectedSize, SizeKnown: true},
			)

			require.ErrorIs(t, err, packstore.ErrContentMismatch)
			assert.Zero(creates)
			assert.Zero(uploads)
			assert.Zero(completes)
			assert.Zero(aborts)
		})
	}
}

func TestPublishPackValidatesEveryEntryBeforeMultipart(t *testing.T) {
	owner := packstore.Ownership{
		Format: packstore.OwnershipFormatV1,
		Vault:  "test-vault",
		Store:  "archive",
		Epoch:  "epoch-1",
	}
	marker, err := packstore.MarshalOwnership(owner)
	require.NoError(t, err)
	forgedLimits := packstore.DefaultLimits()
	forgedLimits.BlobBytes = 16
	forgedID, forgedBytes := makeEncodedPack(t, []byte("x"), 17, 0)
	duplicateID, duplicateBytes, _ := makePackEntries(
		t, []byte("duplicate blob id"), []byte("duplicate blob id"),
	)
	tests := []struct {
		name      string
		limits    packstore.Limits
		packID    string
		packBytes []byte
		wantErr   error
		limit     packstore.LimitDimension
	}{
		{
			name: "malformed pack", limits: packstore.DefaultLimits(),
			packID: pack.NewPackID(), packBytes: []byte("not a pack"),
			wantErr: pack.ErrBadMagic,
		},
		{
			name: "forged blob limit", limits: forgedLimits,
			packID: forgedID, packBytes: forgedBytes,
			wantErr: packstore.ErrBlobTooLarge, limit: packstore.LimitBlobRawBytes,
		},
		{
			name: "duplicate blob id", limits: packstore.DefaultLimits(),
			packID: duplicateID, packBytes: duplicateBytes,
			wantErr: pack.ErrCorrupt,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var gets, multipartCreates, multipartCompletes int
			backend := newHTTPBackend(tt.limits, func(request *http.Request) (*http.Response, error) {
				query := request.URL.Query()
				switch {
				case request.Method == http.MethodGet:
					gets++
					body := marker
					if gets > 1 {
						body = tt.packBytes
					}
					header := make(http.Header)
					header.Set("Content-Length", strconv.Itoa(len(body)))
					return &http.Response{
						StatusCode: http.StatusOK, Header: header,
						Body:          io.NopCloser(bytes.NewReader(body)),
						ContentLength: int64(len(body)), Request: request,
					}, nil
				case request.Method == http.MethodPost && query.Has("uploads"):
					multipartCreates++
					return xmlResponse(request, http.StatusOK,
						`<InitiateMultipartUploadResult>`+
							`<Bucket>test-bucket</Bucket><Key>packs/test</Key>`+
							`<UploadId>upload-1</UploadId>`+
							`</InitiateMultipartUploadResult>`), nil
				case request.Method == http.MethodPut && query.Get("uploadId") == "upload-1":
					response := xmlResponse(request, http.StatusOK, "")
					response.Header.Set("ETag", `"part-etag"`)
					return response, nil
				case request.Method == http.MethodPost && query.Get("uploadId") == "upload-1":
					multipartCompletes++
					return xmlResponse(request, http.StatusOK,
						`<CompleteMultipartUploadResult>`+
							`<Location>https://example.test/test-bucket/packs/test</Location>`+
							`<Bucket>test-bucket</Bucket><Key>packs/test</Key>`+
							`<ETag>&quot;complete-etag&quot;</ETag>`+
							`</CompleteMultipartUploadResult>`), nil
				default:
					return xmlResponse(request, http.StatusInternalServerError,
						`<Error><Code>UnexpectedRequest</Code></Error>`), nil
				}
			})
			backend.setOwnership(owner, `"owner-etag"`)

			_, err := backend.PublishPack(
				t.Context(),
				tt.packID,
				bytes.NewReader(tt.packBytes),
				packstore.PublishOptions{},
			)

			require.ErrorIs(err, tt.wantErr)
			if tt.limit != "" {
				var limit *packstore.LimitError
				require.ErrorAs(err, &limit)
				assert.Equal(tt.limit, limit.Dimension)
			}
			assert.Zero(multipartCreates)
			assert.Zero(multipartCompletes)
		})
	}
}

func TestPublishPackRejectsInvalidDurabilityBeforeStaging(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	owner := packstore.Ownership{
		Format: packstore.OwnershipFormatV1,
		Vault:  "test-vault",
		Store:  "archive",
		Epoch:  "epoch-1",
	}
	marker, err := packstore.MarshalOwnership(owner)
	require.NoError(err)
	packID, packBytes, _ := makePack(t, []byte("invalid pack durability"))
	multipartCreates := 0
	backend := newHTTPBackend(packstore.DefaultLimits(), func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodGet {
			header := make(http.Header)
			header.Set("Content-Length", strconv.Itoa(len(marker)))
			return &http.Response{
				StatusCode: http.StatusOK, Header: header,
				Body:          io.NopCloser(bytes.NewReader(marker)),
				ContentLength: int64(len(marker)), Request: request,
			}, nil
		}
		if request.Method == http.MethodPost && request.URL.Query().Has("uploads") {
			multipartCreates++
		}
		return xmlResponse(request, http.StatusInternalServerError,
			`<Error><Code>UnexpectedRequest</Code></Error>`), nil
	})
	backend.setOwnership(owner, `"owner-etag"`)
	source := &countingReadCloser{reader: bytes.NewReader(packBytes)}

	_, err = backend.PublishPack(
		t.Context(), packID, source,
		packstore.PublishOptions{Durability: packstore.Durability(99)},
	)

	require.ErrorIs(err, packstore.ErrInvalidPolicy)
	assert.Zero(source.read)
	assert.Zero(multipartCreates)
}

func TestPublishLooseRejectsInvalidOptionsBeforeMultipart(t *testing.T) {
	owner := packstore.Ownership{
		Format: packstore.OwnershipFormatV1,
		Vault:  "test-vault",
		Store:  "archive",
		Epoch:  "epoch-1",
	}
	marker, err := packstore.MarshalOwnership(owner)
	require.NoError(t, err)
	content := []byte("hello")
	hash := hashOf(content)
	for _, tt := range []struct {
		name string
		opts packstore.PublishOptions
	}{
		{name: "negative maximum", opts: packstore.PublishOptions{MaxBytes: -1}},
		{name: "negative expected size", opts: packstore.PublishOptions{ExpectedSize: -1}},
		{name: "invalid durability", opts: packstore.PublishOptions{Durability: packstore.Durability(99)}},
		{name: "invalid dedup", opts: packstore.PublishOptions{Dedup: packstore.DedupVerification(99)}},
		{
			name: "invalid compression policy",
			opts: packstore.PublishOptions{Compression: packstore.LooseCompressionOptions{
				MinSavingsPercent: 101,
			}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var multipartCreates int
			backend := newHTTPBackend(packstore.DefaultLimits(), func(request *http.Request) (*http.Response, error) {
				if request.Method == http.MethodGet {
					header := make(http.Header)
					header.Set("Content-Length", strconv.Itoa(len(marker)))
					return &http.Response{
						StatusCode: http.StatusOK, Header: header,
						Body:          io.NopCloser(bytes.NewReader(marker)),
						ContentLength: int64(len(marker)), Request: request,
					}, nil
				}
				if request.Method == http.MethodPost && request.URL.Query().Has("uploads") {
					multipartCreates++
				}
				return xmlResponse(request, http.StatusInternalServerError,
					`<Error><Code>UnexpectedRequest</Code></Error>`), nil
			})
			backend.setOwnership(owner, `"owner-etag"`)

			_, err := backend.PublishLoose(
				t.Context(), hash, bytes.NewReader(content), tt.opts,
			)

			require.ErrorIs(t, err, packstore.ErrInvalidPolicy)
			assert.Zero(t, multipartCreates)
		})
	}
}

func TestRepairLooseAcceptsMaxInt64Limit(t *testing.T) {
	owner := packstore.Ownership{
		Format: packstore.OwnershipFormatV1,
		Vault:  "test-vault",
		Store:  "archive",
		Epoch:  "epoch-1",
	}
	marker, err := packstore.MarshalOwnership(owner)
	require.NoError(t, err)
	content := []byte("hello")
	hash := hashOf(content)
	var gets int
	backend := newHTTPBackend(packstore.DefaultLimits(), func(request *http.Request) (*http.Response, error) {
		switch request.Method {
		case http.MethodGet:
			gets++
			body := marker
			if gets == 2 {
				body = content
			}
			header := make(http.Header)
			header.Set("Content-Length", strconv.Itoa(len(body)))
			return &http.Response{
				StatusCode: http.StatusOK, Header: header,
				Body:          io.NopCloser(bytes.NewReader(body)),
				ContentLength: int64(len(body)), Request: request,
			}, nil
		case http.MethodPut:
			return xmlResponse(request, http.StatusOK, ""), nil
		default:
			return xmlResponse(request, http.StatusInternalServerError,
				`<Error><Code>UnexpectedRequest</Code></Error>`), nil
		}
	})
	backend.setOwnership(owner, `"owner-etag"`)

	receipt, err := backend.RepairLoose(
		t.Context(),
		hash,
		bytes.NewReader(content),
		packstore.PublishOptions{
			ExpectedSize: int64(len(content)), SizeKnown: true,
			MaxBytes: math.MaxInt64,
		},
	)

	require.NoError(t, err)
	assert.Equal(t, int64(len(content)), receipt.Location.LogicalSize)
}

func TestMultipartPublishBoundsPartBufferByPublicationLimit(t *testing.T) {
	assert := assert.New(t)
	var uploaded int64
	backend := newHTTPBackend(packstore.DefaultLimits(), func(request *http.Request) (*http.Response, error) {
		query := request.URL.Query()
		switch {
		case request.Method == http.MethodPost && query.Has("uploads"):
			return xmlResponse(request, http.StatusOK,
				`<InitiateMultipartUploadResult>`+
					`<Bucket>test-bucket</Bucket><Key>packs/test</Key>`+
					`<UploadId>upload-1</UploadId>`+
					`</InitiateMultipartUploadResult>`), nil
		case request.Method == http.MethodPut && query.Get("uploadId") == "upload-1":
			uploaded = request.ContentLength
			response := xmlResponse(request, http.StatusOK, "")
			response.Header.Set("ETag", `"part-etag"`)
			return response, nil
		case request.Method == http.MethodPost && query.Get("uploadId") == "upload-1":
			return xmlResponse(request, http.StatusOK,
				`<CompleteMultipartUploadResult>`+
					`<Location>https://example.test/test-bucket/packs/test</Location>`+
					`<Bucket>test-bucket</Bucket><Key>packs/test</Key>`+
					`<ETag>&quot;complete-etag&quot;</ETag>`+
					`</CompleteMultipartUploadResult>`), nil
		default:
			return xmlResponse(request, http.StatusInternalServerError,
				`<Error><Code>UnexpectedRequest</Code></Error>`), nil
		}
	})
	source := &requestedReadRecorder{reader: strings.NewReader("hi")}

	result, err := backend.multipartPublish(
		t.Context(), "packs/test", source,
		multipartPublishOptions{maxBytes: 2},
	)

	require.NoError(t, err)
	assert.Equal(int64(2), result.size)
	assert.Equal(int64(2), uploaded)
	assert.Equal(3, source.maxRequest)
}

func TestMultipartPublishAbortsDeduplicatedUpload(t *testing.T) {
	var aborts int
	backend := newHTTPBackend(packstore.DefaultLimits(), func(request *http.Request) (*http.Response, error) {
		query := request.URL.Query()
		switch {
		case request.Method == http.MethodPost && query.Has("uploads"):
			return xmlResponse(request, http.StatusOK,
				`<InitiateMultipartUploadResult>`+
					`<Bucket>test-bucket</Bucket><Key>packs/test</Key>`+
					`<UploadId>upload-1</UploadId>`+
					`</InitiateMultipartUploadResult>`), nil
		case request.Method == http.MethodPut && query.Get("uploadId") == "upload-1":
			response := xmlResponse(request, http.StatusOK, "")
			response.Header.Set("ETag", `"part-etag"`)
			return response, nil
		case request.Method == http.MethodPost && query.Get("uploadId") == "upload-1":
			return xmlResponse(request, http.StatusPreconditionFailed,
				`<Error><Code>PreconditionFailed</Code>`+
					`<Message>object already exists</Message></Error>`), nil
		case request.Method == http.MethodDelete && query.Get("uploadId") == "upload-1":
			aborts++
			return xmlResponse(request, http.StatusNoContent, ""), nil
		default:
			return xmlResponse(request, http.StatusInternalServerError,
				`<Error><Code>UnexpectedRequest</Code></Error>`), nil
		}
	})

	result, err := backend.multipartPublish(
		t.Context(),
		"packs/test",
		strings.NewReader("hello"),
		multipartPublishOptions{maxBytes: 5},
	)

	require.NoError(t, err)
	assert.False(t, result.created)
	assert.Equal(t, 1, aborts)
}

func TestRepairLooseCancelStopsBeforePut(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx, cancel := context.WithCancel(t.Context())
	source := &cancelAfterFirstRead{
		cancel: cancel,
		reader: strings.NewReader("hello"),
	}
	owner := packstore.Ownership{
		Format: packstore.OwnershipFormatV1,
		Vault:  "test-vault",
		Store:  "archive",
		Epoch:  "epoch-1",
	}
	marker, err := packstore.MarshalOwnership(owner)
	require.NoError(err)
	var puts int
	backend := newHTTPBackend(packstore.DefaultLimits(), func(request *http.Request) (*http.Response, error) {
		switch request.Method {
		case http.MethodGet:
			header := make(http.Header)
			header.Set("Content-Length", strconv.Itoa(len(marker)))
			return &http.Response{
				StatusCode: http.StatusOK, Header: header,
				Body:          io.NopCloser(bytes.NewReader(marker)),
				ContentLength: int64(len(marker)), Request: request,
			}, nil
		case http.MethodPut:
			puts++
		}
		return xmlResponse(request, http.StatusInternalServerError,
			`<Error><Code>UnexpectedRequest</Code></Error>`), nil
	})
	backend.setOwnership(owner, `"owner-etag"`)

	_, err = backend.RepairLoose(
		ctx,
		packstore.Hash("2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"),
		source,
		packstore.PublishOptions{ExpectedSize: 5, SizeKnown: true},
	)

	require.ErrorIs(err, context.Canceled)
	assert.Equal(1, source.reads)
	assert.Zero(puts)
}

func TestRepairLooseReadsOnlyOneByteBeyondExpectedSize(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	expected := []byte("known")
	sourceBytes := append(bytes.Clone(expected), bytes.Repeat([]byte("x"), 1<<20)...)
	source := &countingReadCloser{reader: bytes.NewReader(sourceBytes)}
	owner := packstore.Ownership{
		Format: packstore.OwnershipFormatV1,
		Vault:  "test-vault",
		Store:  "archive",
		Epoch:  "epoch-1",
	}
	marker, err := packstore.MarshalOwnership(owner)
	require.NoError(err)
	var puts int
	backend := newHTTPBackend(packstore.DefaultLimits(), func(request *http.Request) (*http.Response, error) {
		switch request.Method {
		case http.MethodGet:
			header := make(http.Header)
			header.Set("Content-Length", strconv.Itoa(len(marker)))
			return &http.Response{
				StatusCode: http.StatusOK, Header: header,
				Body:          io.NopCloser(bytes.NewReader(marker)),
				ContentLength: int64(len(marker)), Request: request,
			}, nil
		case http.MethodPut:
			puts++
		}
		return xmlResponse(request, http.StatusInternalServerError,
			`<Error><Code>UnexpectedRequest</Code></Error>`), nil
	})
	backend.setOwnership(owner, `"owner-etag"`)

	_, err = backend.RepairLoose(
		t.Context(),
		hashOf(expected),
		source,
		packstore.PublishOptions{
			ExpectedSize: int64(len(expected)), SizeKnown: true,
			MaxBytes: int64(len(sourceBytes)),
		},
	)

	require.ErrorIs(err, packstore.ErrContentMismatch)
	assert.Equal(int64(len(expected)+1), source.read)
	assert.Zero(puts)
}

func TestMultipartPublishCancelAbortsWithBoundedContext(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel := context.WithCancel(t.Context())
	source := &cancelAfterFirstRead{
		cancel: cancel,
		reader: strings.NewReader("hello"),
	}
	var uploads, completes, aborts int
	backend := newHTTPBackend(packstore.DefaultLimits(), func(request *http.Request) (*http.Response, error) {
		query := request.URL.Query()
		switch {
		case request.Method == http.MethodPost && query.Has("uploads"):
			return xmlResponse(request, http.StatusOK,
				`<InitiateMultipartUploadResult>`+
					`<Bucket>test-bucket</Bucket><Key>packs/test</Key>`+
					`<UploadId>upload-1</UploadId>`+
					`</InitiateMultipartUploadResult>`), nil
		case request.Method == http.MethodPut && query.Get("uploadId") == "upload-1":
			uploads++
			return xmlResponse(request, http.StatusOK, ""), nil
		case request.Method == http.MethodPost && query.Get("uploadId") == "upload-1":
			completes++
			return xmlResponse(request, http.StatusOK, ""), nil
		case request.Method == http.MethodDelete && query.Get("uploadId") == "upload-1":
			require.NoError(t, request.Context().Err())
			_, hasDeadline := request.Context().Deadline()
			assert.True(hasDeadline)
			aborts++
			return xmlResponse(request, http.StatusNoContent, ""), nil
		default:
			return xmlResponse(request, http.StatusInternalServerError,
				`<Error><Code>UnexpectedRequest</Code></Error>`), nil
		}
	})

	_, err := backend.multipartPublish(
		ctx, "packs/test", source, multipartPublishOptions{maxBytes: 5},
	)

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(1, source.reads)
	assert.Zero(uploads)
	assert.Zero(completes)
	assert.Equal(1, aborts)
}

func TestMultipartPublishReadsOnlyOneByteBeyondLimit(t *testing.T) {
	assert := assert.New(t)
	source := &countingReadCloser{reader: strings.NewReader("0123456789abcdefghij")}
	var uploads, aborts int
	backend := newHTTPBackend(packstore.DefaultLimits(), func(request *http.Request) (*http.Response, error) {
		query := request.URL.Query()
		switch {
		case request.Method == http.MethodPost && query.Has("uploads"):
			return xmlResponse(request, http.StatusOK,
				`<InitiateMultipartUploadResult>`+
					`<Bucket>test-bucket</Bucket><Key>packs/test</Key>`+
					`<UploadId>upload-1</UploadId>`+
					`</InitiateMultipartUploadResult>`), nil
		case request.Method == http.MethodPut && query.Get("uploadId") == "upload-1":
			uploads++
			response := xmlResponse(request, http.StatusOK, "")
			response.Header.Set("ETag", `"part-etag"`)
			return response, nil
		case request.Method == http.MethodDelete && query.Get("uploadId") == "upload-1":
			aborts++
			return xmlResponse(request, http.StatusNoContent, ""), nil
		default:
			return xmlResponse(request, http.StatusInternalServerError,
				`<Error><Code>UnexpectedRequest</Code></Error>`), nil
		}
	})
	backend.part = 5

	_, err := backend.multipartPublish(
		t.Context(), "packs/test", source,
		multipartPublishOptions{maxBytes: 7},
	)

	require.ErrorIs(t, err, packstore.ErrBlobTooLarge)
	assert.Equal(int64(8), source.read)
	assert.Equal(1, uploads)
	assert.Equal(1, aborts)
}

type countingReadCloser struct {
	reader io.Reader
	read   int64
}

func (r *countingReadCloser) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read += int64(n)
	return n, err
}

func (r *countingReadCloser) Close() error { return nil }

type cancelAfterFirstRead struct {
	cancel context.CancelFunc
	reader io.Reader
	reads  int
}

type requestedReadRecorder struct {
	reader     io.Reader
	maxRequest int
}

func (r *requestedReadRecorder) Read(p []byte) (int, error) {
	r.maxRequest = max(r.maxRequest, len(p))
	return r.reader.Read(p)
}

func (r *cancelAfterFirstRead) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	n, err := r.reader.Read(p)
	r.reads++
	if n > 0 {
		r.cancel()
	}
	return n, err
}

func makeEncodedPack(
	t *testing.T,
	content []byte,
	rawLen uint64,
	windowBytes int,
) (string, []byte) {
	t.Helper()
	require := require.New(t)
	t.Helper()
	var frame bytes.Buffer
	options := []zstd.EOption{zstd.WithEncoderConcurrency(1)}
	if windowBytes > 0 {
		options = append(options, zstd.WithWindowSize(windowBytes))
	}
	encoder, err := zstd.NewWriter(&frame, options...)
	require.NoError(err)
	_, err = encoder.Write(content)
	require.NoError(err)
	require.NoError(encoder.Close())
	staging := t.TempDir()
	writer, err := pack.NewWriter(staging, pack.WriterOptions{})
	require.NoError(err)
	_, err = writer.AppendEncoded(
		pack.ComputeBlobID(content),
		frame.Bytes(),
		rawLen,
		true,
	)
	require.NoError(err)
	packID := writer.ID()
	packPath := filepath.Join(staging, packID+".pack")
	_, err = writer.Seal(packPath)
	require.NoError(err)
	packBytes, err := os.ReadFile(packPath)
	require.NoError(err)
	return packID, packBytes
}

func xmlResponse(
	request *http.Request,
	statusCode int,
	body string,
) *http.Response {
	header := make(http.Header)
	header.Set("Content-Type", "application/xml")
	return &http.Response{
		StatusCode: statusCode,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

// errorWithFinalBytes returns its data in one read together with err.
type errorWithFinalBytes struct {
	data []byte
	err  error
}

func (s *errorWithFinalBytes) Read(p []byte) (int, error) {
	if len(s.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s.data)
	s.data = s.data[n:]
	if len(s.data) == 0 {
		return n, s.err
	}
	return n, nil
}

func TestMultipartPublishRejectsSourceErrorWithFinalBytes(t *testing.T) {
	errSource := errors.New("source failed")
	// The test backend uses 5-byte parts, so "hello" fills the part buffer
	// exactly and "hi" leaves it short.
	for name, tt := range map[string]struct {
		data  string
		final error
		cause error
	}{
		"full part":                     {data: "hello", final: errSource, cause: errSource},
		"full part joined to EOF":       {data: "hello", final: errors.Join(io.EOF, errSource), cause: errSource},
		"full part reports truncation":  {data: "hello", final: io.ErrUnexpectedEOF, cause: io.ErrUnexpectedEOF},
		"short read":                    {data: "hi", final: errSource, cause: errSource},
		"short read joined to EOF":      {data: "hi", final: errors.Join(io.EOF, errSource), cause: errSource},
		"short read reports truncation": {data: "hi", final: io.ErrUnexpectedEOF, cause: io.ErrUnexpectedEOF},
	} {
		t.Run(name, func(t *testing.T) {
			var completes int
			backend := newHTTPBackend(packstore.DefaultLimits(), func(request *http.Request) (*http.Response, error) {
				query := request.URL.Query()
				switch {
				case request.Method == http.MethodPost && query.Has("uploads"):
					return xmlResponse(request, http.StatusOK,
						`<InitiateMultipartUploadResult><Bucket>test-bucket</Bucket><Key>packs/test</Key>`+
							`<UploadId>upload-1</UploadId></InitiateMultipartUploadResult>`), nil
				case request.Method == http.MethodPut && query.Get("uploadId") == "upload-1":
					response := xmlResponse(request, http.StatusOK, "")
					response.Header.Set("ETag", `"part-etag"`)
					return response, nil
				case request.Method == http.MethodPost && query.Get("uploadId") == "upload-1":
					completes++
					return xmlResponse(request, http.StatusOK,
						`<CompleteMultipartUploadResult><Bucket>test-bucket</Bucket><Key>packs/test</Key>`+
							`<ETag>&quot;complete-etag&quot;</ETag></CompleteMultipartUploadResult>`), nil
				case request.Method == http.MethodDelete && query.Get("uploadId") == "upload-1":
					return xmlResponse(request, http.StatusNoContent, ""), nil
				default:
					return xmlResponse(request, http.StatusInternalServerError,
						`<Error><Code>UnexpectedRequest</Code></Error>`), nil
				}
			})
			_, err := backend.multipartPublish(t.Context(), "packs/test",
				&errorWithFinalBytes{data: []byte(tt.data), err: tt.final}, multipartPublishOptions{})
			require.ErrorIs(t, err, tt.cause)
			assert.Zero(t, completes)
		})
	}
}
