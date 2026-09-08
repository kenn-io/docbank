package geminiembed

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type successfulFilesLifecycle struct {
	t         *testing.T
	data      []byte
	mediaType string
	filename  string
	fileName  string
	fileURI   string
	timeline  fileTestTimeline
	embedBody string

	mu       sync.Mutex
	requests []fileTestRequest
}

type fileTestTimeline struct {
	created time.Time
	updated time.Time
	expires time.Time
}

type fileTestRequest struct {
	method string
	url    string
	body   []byte
}

func newSuccessfulFilesLifecycle(t *testing.T, data []byte, mediaType, filename string) *successfulFilesLifecycle {
	t.Helper()
	created := time.Now().UTC().Truncate(time.Second)
	return &successfulFilesLifecycle{
		t: t, data: append([]byte(nil), data...), mediaType: mediaType, filename: filename,
		fileName: "files/file-123", fileURI: origin + "/v1beta/files/file-123",
		timeline:  fileTestTimeline{created: created, updated: created, expires: created.Add(48 * time.Hour)},
		embedBody: `{"embedding":{"values":[` + testVectorJSON(128) + `]}}`,
	}
}

func (lifecycle *successfulFilesLifecycle) RoundTrip(request *http.Request) (*http.Response, error) {
	lifecycle.t.Helper()
	bodyReader := request.Body
	if bodyReader == nil {
		bodyReader = http.NoBody
	}
	body, err := io.ReadAll(bodyReader)
	require.NoError(lifecycle.t, err)
	lifecycle.mu.Lock()
	lifecycle.requests = append(lifecycle.requests, fileTestRequest{method: request.Method, url: request.URL.String(), body: append([]byte(nil), body...)})
	step := len(lifecycle.requests)
	lifecycle.mu.Unlock()
	for key, values := range request.Header {
		assert.NotContains(lifecycle.t, key, lifecycle.filename)
		for _, value := range values {
			assert.NotContains(lifecycle.t, value, lifecycle.filename)
		}
	}
	assert.NotContains(lifecycle.t, request.URL.String(), lifecycle.filename)
	assert.NotContains(lifecycle.t, string(body), lifecycle.filename)

	switch step {
	case 1:
		require.Equal(lifecycle.t, http.MethodPost, request.Method)
		assert.Equal(lifecycle.t, origin+"/upload/v1beta/files", request.URL.String())
		assert.Equal(lifecycle.t, "resumable", request.Header.Get("X-Goog-Upload-Protocol"))
		assert.Equal(lifecycle.t, "start", request.Header.Get("X-Goog-Upload-Command"))
		assert.Equal(lifecycle.t, strconv.Itoa(len(lifecycle.data)), request.Header.Get("X-Goog-Upload-Header-Content-Length"))
		assert.Equal(lifecycle.t, lifecycle.mediaType, request.Header.Get("X-Goog-Upload-Header-Content-Type"))
		if string(body) != `{"file":{}}` {
			lifecycle.t.Errorf("start body = %q, want exact empty file object", body)
		}
		response := geminiJSONResponse(request, "")
		response.Header.Set("X-Goog-Upload-Url", origin+"/upload/v1beta/files?upload_id=synthetic-session-123&upload_protocol=resumable")
		return response, nil
	case 2:
		require.Equal(lifecycle.t, http.MethodPost, request.Method)
		assert.Equal(lifecycle.t, origin+"/upload/v1beta/files?upload_id=synthetic-session-123&upload_protocol=resumable", request.URL.String())
		assert.Equal(lifecycle.t, "upload, finalize", request.Header.Get("X-Goog-Upload-Command"))
		assert.Equal(lifecycle.t, "0", request.Header.Get("X-Goog-Upload-Offset"))
		assert.Equal(lifecycle.t, lifecycle.data, body)
		response := geminiJSONResponse(request, `{"file":`+lifecycle.fileJSON("PROCESSING")+`}`)
		response.Header.Set("X-Goog-Upload-Status", "final")
		return response, nil
	case 3:
		require.Equal(lifecycle.t, http.MethodGet, request.Method)
		assert.Equal(lifecycle.t, lifecycle.fileURI, request.URL.String())
		return geminiJSONResponse(request, lifecycle.fileJSON("PROCESSING")), nil
	case 4:
		require.Equal(lifecycle.t, http.MethodGet, request.Method)
		assert.Equal(lifecycle.t, lifecycle.fileURI, request.URL.String())
		return geminiJSONResponse(request, lifecycle.fileJSON("ACTIVE")), nil
	case 5:
		require.Equal(lifecycle.t, http.MethodPost, request.Method)
		assert.Equal(lifecycle.t, origin+embedPath, request.URL.String())
		want := `{"model":"models/gemini-embedding-2","content":{"parts":[{"fileData":{"mimeType":"` + lifecycle.mediaType + `","fileUri":"` + lifecycle.fileURI + `"}}]},"outputDimensionality":128}`
		assert.Equal(lifecycle.t, want, string(body))
		return geminiJSONResponse(request, lifecycle.embedBody), nil
	case 6:
		require.Equal(lifecycle.t, http.MethodDelete, request.Method)
		assert.Equal(lifecycle.t, lifecycle.fileURI, request.URL.String())
		return geminiJSONResponse(request, `{}`), nil
	default:
		return nil, fmt.Errorf("unexpected Files lifecycle request %d", step)
	}
}

func (lifecycle *successfulFilesLifecycle) fileJSON(state string) string {
	return fileTestJSON(lifecycle.data, lifecycle.mediaType, lifecycle.fileName, lifecycle.fileURI, state, lifecycle.timeline)
}

func fileTestJSON(data []byte, mediaType, fileName, fileURI, state string, timeline fileTestTimeline) string {
	digest := sha256.Sum256(data)
	return `{"name":"` + fileName + `","displayName":"","mimeType":"` + mediaType +
		`","sizeBytes":"` + strconv.Itoa(len(data)) + `","createTime":"` + timeline.created.Format(time.RFC3339Nano) +
		`","updateTime":"` + timeline.updated.Format(time.RFC3339Nano) + `","expirationTime":"` + timeline.expires.Format(time.RFC3339Nano) +
		`","sha256Hash":"` + base64.StdEncoding.EncodeToString(digest[:]) + `","uri":"` + fileURI +
		`","downloadUri":"","state":"` + state + `","source":"UPLOADED"}`
}

func (lifecycle *successfulFilesLifecycle) snapshot() []fileTestRequest {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	result := make([]fileTestRequest, len(lifecycle.requests))
	copy(result, lifecycle.requests)
	return result
}

var _ http.RoundTripper = (*successfulFilesLifecycle)(nil)

func fileTestRequestBodies(requests []fileTestRequest) string {
	var bodies strings.Builder
	for _, request := range requests {
		bodies.Write(request.body)
	}
	return bodies.String()
}
