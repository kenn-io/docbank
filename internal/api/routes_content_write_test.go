package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func sendContentReplacement(
	t *testing.T, tsURL string, client *http.Client, nodeID, revision int64,
	content []byte, expectedHash string, expectedSize int64, contentType string,
) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut,
		fmt.Sprintf("%s/api/v1/nodes/%d/content", tsURL, nodeID), bytes.NewReader(content))
	require.NoError(t, err)
	req.Header.Set("If-Match", fmt.Sprintf("%q", strconv.FormatInt(revision, 10)))
	req.Header.Set(api.BlobHashHeader, expectedHash)
	req.Header.Set(api.BlobSizeHeader, strconv.FormatInt(expectedSize, 10))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, body
}

func TestContentReplacementCreatesVerifiedImmutableHead(t *testing.T) {
	ts, s := newTestServer(t, nil)
	created := createFileWithContent(t, ts, s, "/report.bin", "old content")
	oldVersion := created.CurrentVersionID
	content := []byte{0x00, 0xff, 0x10, 0x80, 'n', 'e', 'w'}
	hash := uploadIdentity(content)

	resp, body := sendContentReplacement(t, ts.URL, ts.Client(), created.ID, created.Revision,
		content, hash, int64(len(content)), "application/octet-stream")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Equal(t, `"2"`, resp.Header.Get("ETag"))
	var receipt api.ContentReplacementReceipt
	require.NoError(t, json.Unmarshal(body, &receipt))
	assert.Equal(t, created.ID, receipt.Node.ID)
	assert.Equal(t, created.Revision+1, receipt.Node.Revision)
	assert.Equal(t, receipt.Version.ID, receipt.Node.CurrentVersionID)
	assert.Equal(t, "content_replace", receipt.Version.TransitionKind)
	assert.Equal(t, hash, receipt.ComputedHash)
	assert.Equal(t, int64(len(content)), receipt.ComputedSize)
	assert.Equal(t, hash, receipt.Version.BlobHash)
	assert.Equal(t, "application/octet-stream", receipt.Version.MimeType)

	contentResp, err := ts.Client().Get(fmt.Sprintf("%s/api/v1/nodes/%d/content", ts.URL, created.ID))
	require.NoError(t, err)
	currentBody, err := io.ReadAll(contentResp.Body)
	require.NoError(t, err)
	require.NoError(t, contentResp.Body.Close())
	assert.Equal(t, http.StatusOK, contentResp.StatusCode)
	assert.Equal(t, content, currentBody, "opaque binary bytes must bypass JSON text validation unchanged")
	assert.Equal(t, receipt.Version.ID, contentResp.Header.Get(api.ContentVersionHeader))

	oldResp, err := ts.Client().Get(ts.URL + "/api/v1/versions/" + oldVersion + "/content")
	require.NoError(t, err)
	oldBody, err := io.ReadAll(oldResp.Body)
	require.NoError(t, err)
	require.NoError(t, oldResp.Body.Close())
	assert.Equal(t, http.StatusOK, oldResp.StatusCode)
	assert.Equal(t, []byte("old content"), oldBody)

	versions, total, err := s.ContentVersions(t.Context(), created.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 2, total)
	require.Len(t, versions, 2)
	assert.Equal(t, receipt.Version.ID, versions[0].ID)
	assert.Equal(t, oldVersion, versions[1].ID)
}

func TestContentReplacementRejectsMismatchWithoutChangingAuthority(t *testing.T) {
	tests := []struct {
		name         string
		revision     func(store.Node) int64
		expectedHash func([]byte) string
		expectedSize func([]byte) int64
		wantStatus   int
		wantCode     string
	}{
		{name: "stale revision", revision: func(n store.Node) int64 { return n.Revision + 1 },
			wantStatus: http.StatusPreconditionFailed, wantCode: "stale_revision"},
		{name: "digest mismatch", expectedHash: func([]byte) string { return uploadIdentity([]byte("other")) },
			wantStatus: http.StatusUnprocessableEntity, wantCode: "digest_mismatch"},
		{name: "short declaration", expectedSize: func(body []byte) int64 { return int64(len(body) - 1) },
			wantStatus: http.StatusUnprocessableEntity, wantCode: "size_mismatch"},
		{name: "long declaration", expectedSize: func(body []byte) int64 { return int64(len(body) + 1) },
			wantStatus: http.StatusUnprocessableEntity, wantCode: "size_mismatch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts, s := newTestServer(t, nil)
			created := createFileWithContent(t, ts, s, "/report.txt", "old content")
			content := []byte("replacement")
			revision := created.Revision
			if tt.revision != nil {
				revision = tt.revision(created)
			}
			hash := uploadIdentity(content)
			if tt.expectedHash != nil {
				hash = tt.expectedHash(content)
			}
			size := int64(len(content))
			if tt.expectedSize != nil {
				size = tt.expectedSize(content)
			}
			resp, body := sendContentReplacement(t, ts.URL, ts.Client(), created.ID, revision,
				content, hash, size, "text/plain")
			assert.Equal(t, tt.wantStatus, resp.StatusCode, string(body))
			assert.Contains(t, string(body), fmt.Sprintf(`"code":%q`, tt.wantCode))

			unchanged, err := s.NodeByID(t.Context(), created.ID)
			require.NoError(t, err)
			assert.Equal(t, created.Revision, unchanged.Revision)
			assert.Equal(t, created.CurrentVersionID, unchanged.CurrentVersionID)
			versions, total, err := s.ContentVersions(t.Context(), created.ID, 10, 0)
			require.NoError(t, err)
			assert.Equal(t, 1, total)
			require.Len(t, versions, 1)
		})
	}
}

func TestContentReplacementRequiresPreconditionAndFileTarget(t *testing.T) {
	ts, s := newTestServer(t, nil)
	content := []byte("replacement")
	hash := uploadIdentity(content)

	req, err := http.NewRequest(http.MethodPut,
		fmt.Sprintf("%s/api/v1/nodes/%d/content", ts.URL, s.RootID()), bytes.NewReader(content))
	require.NoError(t, err)
	req.Header.Set(api.BlobHashHeader, hash)
	req.Header.Set(api.BlobSizeHeader, strconv.Itoa(len(content)))
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusPreconditionRequired, resp.StatusCode, string(body))
	assert.Contains(t, string(body), `"code":"precondition_required"`)

	root, err := s.NodeByID(t.Context(), s.RootID())
	require.NoError(t, err)
	resp, body = sendContentReplacement(t, ts.URL, ts.Client(), root.ID, root.Revision,
		content, hash, int64(len(content)), "")
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, string(body))
	assert.Contains(t, string(body), `"code":"not_file"`)
	blobs, err := s.AllBlobs(t.Context())
	require.NoError(t, err)
	assert.Empty(t, blobs, "a failed replacement must not grant blob authority")
}
