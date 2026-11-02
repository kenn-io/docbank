package s3store

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"
)

func TestInventoryEncodedKeysAcrossPages(t *testing.T) {
	const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const packID = "01arz3ndektsv4rrffq69g5fav"
	keys := []string{
		"name space/.kit-store.json",
		"name space/loose/aa/" + hash,
		"name space/packs/01/" + packID + ".pack",
		"name space/foreign/\x01+entry",
		"name space/foreign/literal%2F+name/nested",
		"name space/loose%2Faa%2F" + hash,
	}
	requests := 0
	backend := newHTTPBackend(packstore.DefaultLimits(), func(r *http.Request) (*http.Response, error) {
		query := r.URL.Query()
		assert.Equal(t, "name space/", query.Get("prefix"))
		assert.Equal(t, "1", query.Get("max-keys"))
		if requests > 0 {
			assert.Equal(t, fmt.Sprintf("page/%%2B+%d", requests), query.Get("continuation-token"))
		}
		key := keys[requests]
		if query.Get("encoding-type") == "url" {
			key = url.QueryEscape(key)
		}
		requests++
		next := "<IsTruncated>false</IsTruncated>"
		if requests < len(keys) {
			next = fmt.Sprintf("<IsTruncated>true</IsTruncated><NextContinuationToken>page/%%2B+%d</NextContinuationToken>", requests)
		}
		return xmlResponse(r, http.StatusOK, "<ListBucketResult><EncodingType>url</EncodingType><Contents><Key>"+key+"</Key><Size>6</Size></Contents>"+next+"</ListBucketResult>"), nil
	})
	backend.keys = keyspace{prefix: "name space"}
	backend.page = 1
	var objects []packstore.InventoryObject
	var unknown []string
	var cursor packstore.InventoryCursor
	for i := range keys {
		page, err := backend.Inventory(t.Context(), cursor)
		require.NoError(t, err)
		objects = append(objects, page.Objects...)
		unknown = append(unknown, page.Unknown...)
		if i+1 < len(keys) {
			assert.Equal(t, packstore.InventoryCursor(fmt.Sprintf("page/%%2B+%d", i+1)), page.NextCursor)
		} else {
			assert.Empty(t, page.NextCursor)
		}
		cursor = page.NextCursor
	}
	assert.Equal(t, []packstore.InventoryObject{
		{Ref: packstore.ObjectRef{LooseHash: hash, LooseEncoding: packstore.LooseEncodingRaw}, StoredSize: 6},
		{Ref: packstore.ObjectRef{PackID: packID}, StoredSize: 6},
	}, objects)
	assert.Equal(t, keys[3:], unknown)
	assert.Equal(t, len(keys), requests)
}

func TestProbeDisarmsCleanupAfterExplicitDelete(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	state, backend := newProbeHTTPBackend(t, false)

	report, err := backend.Probe(t.Context())

	require.NoError(err)
	assert.Equal(CapabilityReport{
		StrongReadAfterWrite: true,
		RepeatableReads:      true,
		RangeReads:           true,
		Listing:              true,
		MultipartPublication: true,
		ConditionalWrites:    true,
		Delete:               true,
	}, report)
	assert.Equal(2, state.ownershipReads)
	assert.Equal(1, state.deletes)
	assert.Equal([]int{31, 37}, state.conditionalWriteBodies)
	assert.Equal([]int64{31, 37}, state.conditionalWriteLengths)
}

func TestProbeRejectsAcknowledgedDeleteThatLeavesObject(t *testing.T) {
	assert := assert.New(t)
	state, backend := newProbeHTTPBackend(t, false)
	state.ignoreDelete = true

	report, err := backend.Probe(t.Context())

	require.Error(t, err)
	assert.False(report.Delete)
	assert.Equal(2, state.deletes, "failed verification must leave cleanup armed")
	assert.NotNil(state.object)
}

func TestProbeCleanupUsesFreshDeadline(t *testing.T) {
	assert := assert.New(t)
	state, backend := newProbeHTTPBackend(t, true)

	_, err := backend.Probe(t.Context())

	require.Error(t, err)
	assert.True(state.cleanupOwnershipHadDeadline)
	assert.True(state.cleanupDeleteHadDeadline)
	assert.Equal(1, state.deletes)
}

func TestProbeRejectsIgnoredConditionalWrites(t *testing.T) {
	tests := []struct {
		name  string
		apply func(*probeHTTPState)
	}{
		{
			name: "conditional create",
			apply: func(state *probeHTTPState) {
				state.ignoreConditionalCreate = true
			},
		},
		{
			name: "conditional replacement",
			apply: func(state *probeHTTPState) {
				state.ignoreConditionalReplace = true
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, backend := newProbeHTTPBackend(t, false)
			tt.apply(state)

			_, err := backend.Probe(t.Context())

			require.Error(t, err)
		})
	}
}

func TestProbeRejectsAppliedStaleConditionalReplacement(t *testing.T) {
	state, backend := newProbeHTTPBackend(t, false)
	state.applyStaleWrite = true

	_, err := backend.Probe(t.Context())

	require.ErrorIs(t, err, packstore.ErrPhysicalCorrupt)
}

func TestReadProbeBodyBoundsAndValidatesResponse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	expected := []byte("probe")
	got, err := readProbeBody(bytes.NewReader(expected), nil, expected)
	require.NoError(err)
	assert.Equal(expected, got)

	oversized := bytes.NewReader(bytes.Repeat([]byte("x"), 64))
	_, err = readProbeBody(oversized, nil, expected)
	require.ErrorContains(err, "exceeds expected length")
	assert.Equal(64-len(expected)-1, oversized.Len())

	contentLength := int64(len(expected) + 1)
	_, err = readProbeBody(bytes.NewReader(expected), &contentLength, expected)
	require.ErrorContains(err, "response length")
}

type probeHTTPState struct {
	t                           *testing.T
	marker                      []byte
	payload                     []byte
	object                      []byte
	objectETag                  string
	probeKey                    string
	failFirstProbeRead          bool
	ignoreConditionalCreate     bool
	ignoreConditionalReplace    bool
	applyStaleWrite             bool
	ignoreDelete                bool
	probeReads                  int
	multipartCreates            int
	ownershipReads              int
	deletes                     int
	conditionalWriteBodies      []int
	conditionalWriteLengths     []int64
	pendingUploads              map[string][]byte
	cleanupOwnershipHadDeadline bool
	cleanupDeleteHadDeadline    bool
}

func newProbeHTTPBackend(t *testing.T, failFirstProbeRead bool) (*probeHTTPState, *Backend) {
	t.Helper()
	owner := packstore.Ownership{
		Format: packstore.OwnershipFormatV1,
		Vault:  "test-vault",
		Store:  "archive",
		Epoch:  "epoch-1",
	}
	marker, err := packstore.MarshalOwnership(owner)
	require.NoError(t, err)
	state := &probeHTTPState{
		t:                  t,
		marker:             marker,
		payload:            bytes.Repeat([]byte("kit-s3-probe\n"), 512<<10),
		failFirstProbeRead: failFirstProbeRead,
		pendingUploads:     make(map[string][]byte),
	}
	backend := newHTTPBackend(packstore.DefaultLimits(), state.roundTrip)
	backend.part = int64(len(state.payload))
	backend.setOwnership(owner, `"owner-etag"`)
	return state, backend
}

func (s *probeHTTPState) roundTrip(request *http.Request) (*http.Response, error) {
	s.t.Helper()
	query := request.URL.Query()
	key := strings.TrimPrefix(request.URL.Path, "/test-bucket/")
	switch {
	case request.Method == http.MethodGet && key == ".kit-store.json":
		s.ownershipReads++
		if s.ownershipReads == 2 && s.failFirstProbeRead {
			_, s.cleanupOwnershipHadDeadline = request.Context().Deadline()
		}
		response := bytesResponse(request, s.marker)
		response.Header.Set("ETag", `"owner-etag"`)
		return response, nil
	case request.Method == http.MethodPost && query.Has("uploads"):
		s.probeKey = key
		s.multipartCreates++
		uploadID := fmt.Sprintf("upload-%d", s.multipartCreates)
		return xmlResponse(request, http.StatusOK,
			`<InitiateMultipartUploadResult>`+
				`<Bucket>test-bucket</Bucket><Key>`+key+`</Key>`+
				`<UploadId>`+uploadID+`</UploadId>`+
				`</InitiateMultipartUploadResult>`), nil
	case request.Method == http.MethodPut && query.Get("uploadId") != "":
		uploadID := query.Get("uploadId")
		part, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		s.pendingUploads[uploadID] = append(s.pendingUploads[uploadID], part...)
		response := xmlResponse(request, http.StatusOK, "")
		response.Header.Set("ETag", `"part-etag"`)
		return response, nil
	case request.Method == http.MethodPost && query.Get("uploadId") != "":
		uploadID := query.Get("uploadId")
		if s.object != nil && !s.ignoreConditionalCreate {
			return xmlResponse(request, http.StatusPreconditionFailed,
				`<Error><Code>PreconditionFailed</Code></Error>`), nil
		}
		s.object = append([]byte(nil), s.pendingUploads[uploadID]...)
		s.objectETag = fmt.Sprintf(`"complete-etag-%d"`, s.multipartCreates)
		response := xmlResponse(request, http.StatusOK,
			`<CompleteMultipartUploadResult>`+
				`<Bucket>test-bucket</Bucket><Key>`+s.probeKey+`</Key>`+
				`<ETag>`+s.objectETag+`</ETag>`+
				`</CompleteMultipartUploadResult>`)
		response.Header.Set("ETag", s.objectETag)
		return response, nil
	case request.Method == http.MethodDelete && query.Get("uploadId") != "":
		delete(s.pendingUploads, query.Get("uploadId"))
		return xmlResponse(request, http.StatusNoContent, ""), nil
	case request.Method == http.MethodGet && query.Get("list-type") == "2":
		return xmlResponse(request, http.StatusOK,
			`<ListBucketResult>`+
				`<Name>test-bucket</Name><Prefix>`+s.probeKey+`</Prefix>`+
				`<KeyCount>1</KeyCount><MaxKeys>2</MaxKeys><IsTruncated>false</IsTruncated>`+
				`<Contents><Key>`+s.probeKey+`</Key><Size>`+
				strconv.Itoa(len(s.object))+`</Size></Contents>`+
				`</ListBucketResult>`), nil
	case request.Method == http.MethodGet && key == s.probeKey:
		s.probeReads++
		if s.failFirstProbeRead {
			return nil, errors.New("probe read failed")
		}
		if request.Header.Get("Range") == "bytes=5-20" {
			response := bytesResponse(request, s.object[5:21])
			response.Header.Set("ETag", s.objectETag)
			return response, nil
		}
		response := bytesResponse(request, s.object)
		response.Header.Set("ETag", s.objectETag)
		return response, nil
	case request.Method == http.MethodPut && key == s.probeKey:
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		s.conditionalWriteBodies = append(s.conditionalWriteBodies, len(body))
		s.conditionalWriteLengths = append(s.conditionalWriteLengths, request.ContentLength)
		if request.Header.Get("If-Match") != s.objectETag && !s.ignoreConditionalReplace {
			if s.applyStaleWrite {
				s.object = append([]byte(nil), body...)
				s.objectETag = `"stale-replacement-etag"`
			}
			return xmlResponse(request, http.StatusPreconditionFailed,
				`<Error><Code>PreconditionFailed</Code></Error>`), nil
		}
		s.object = append([]byte(nil), body...)
		s.objectETag = `"replacement-etag"`
		response := xmlResponse(request, http.StatusOK, "")
		response.Header.Set("ETag", s.objectETag)
		return response, nil
	case request.Method == http.MethodDelete && key == s.probeKey:
		s.deletes++
		if s.failFirstProbeRead {
			_, s.cleanupDeleteHadDeadline = request.Context().Deadline()
		}
		if !s.ignoreDelete {
			s.object = nil
		}
		return xmlResponse(request, http.StatusNoContent, ""), nil
	case request.Method == http.MethodHead && key == s.probeKey:
		if s.object == nil {
			return xmlResponse(request, http.StatusNotFound,
				`<Error><Code>NoSuchKey</Code></Error>`), nil
		}
		response := xmlResponse(request, http.StatusOK, "")
		response.Header.Set("Content-Length", strconv.Itoa(len(s.object)))
		return response, nil
	default:
		return xmlResponse(request, http.StatusInternalServerError,
			`<Error><Code>UnexpectedRequest</Code></Error>`), nil
	}
}

func bytesResponse(request *http.Request, body []byte) *http.Response {
	header := make(http.Header)
	header.Set("Content-Length", strconv.Itoa(len(body)))
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       request,
	}
}
