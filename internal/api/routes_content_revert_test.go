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
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/api"
)

func postContentRevert(
	t *testing.T, tsURL string, client *http.Client, nodeID, revision int64, sourceVersionID string,
) (*http.Response, []byte) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"source_version_id": sourceVersionID})
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("%s/api/v1/nodes/%d/revert", tsURL, nodeID), bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if revision >= 0 {
		req.Header.Set("If-Match", fmt.Sprintf("%q", strconv.FormatInt(revision, 10)))
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	responseBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, responseBody
}

func TestContentRevertAdoptsPackedPriorVersionWithoutCopyingBlob(t *testing.T) {
	ts, s := newTestServer(t, nil)
	blobs := s.Blobs
	created := createFileWithContent(t, ts, s, "/report.txt", "original")
	packed, err := blobs.Maintainer().Pack(t.Context(), packstore.PackOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, packed.BlobsPacked)

	replacement := []byte("replacement")
	replacementHash := uploadIdentity(replacement)
	resp, body := sendContentReplacement(t, ts.URL, ts.Client(), created.ID, created.Revision,
		replacement, replacementHash, int64(len(replacement)), "text/markdown")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var replaced api.ContentReplacementReceipt
	require.NoError(t, json.Unmarshal(body, &replaced))
	beforeStats, err := blobs.Stats(t.Context())
	require.NoError(t, err)

	resp, body = postContentRevert(t, ts.URL, ts.Client(), created.ID, replaced.Node.Revision,
		created.CurrentVersionID)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Equal(t, `"3"`, resp.Header.Get("ETag"))
	var receipt api.ContentReversionReceipt
	require.NoError(t, json.Unmarshal(body, &receipt))
	assert.Equal(t, created.CurrentVersionID, receipt.SourceVersion.ID)
	assert.Equal(t, created.BlobHash, receipt.Version.BlobHash)
	assert.Equal(t, created.MimeType, receipt.Version.MimeType)
	assert.Equal(t, "content_revert", receipt.Version.TransitionKind)
	require.NotNil(t, receipt.Version.SourceVersionID)
	assert.Equal(t, receipt.SourceVersion.ID, *receipt.Version.SourceVersionID)
	assert.Equal(t, receipt.Version.ID, receipt.Node.CurrentVersionID)
	afterStats, err := blobs.Stats(t.Context())
	require.NoError(t, err)
	assert.Equal(t, beforeStats, afterStats, "reversion must not copy loose or packed bytes")

	currentResp, err := ts.Client().Get(fmt.Sprintf("%s/api/v1/nodes/%d/content", ts.URL, created.ID))
	require.NoError(t, err)
	current, err := io.ReadAll(currentResp.Body)
	require.NoError(t, err)
	require.NoError(t, currentResp.Body.Close())
	assert.Equal(t, http.StatusOK, currentResp.StatusCode)
	assert.Equal(t, "original", string(current))
	assert.Equal(t, receipt.Version.ID, currentResp.Header.Get(api.ContentVersionHeader))
}

func TestContentRevertRejectsStaleAndInvalidSources(t *testing.T) {
	ts, s := newTestServer(t, nil)
	first := createFileWithContent(t, ts, s, "/first.txt", "first")
	second := createFileWithContent(t, ts, s, "/second.txt", "second")
	replacement := []byte("new first")
	replacementHash := uploadIdentity(replacement)
	resp, body := sendContentReplacement(t, ts.URL, ts.Client(), first.ID, first.Revision,
		replacement, replacementHash, int64(len(replacement)), "text/plain")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var replaced api.ContentReplacementReceipt
	require.NoError(t, json.Unmarshal(body, &replaced))

	tests := []struct {
		name       string
		revision   int64
		source     string
		wantStatus int
		wantCode   string
	}{
		{name: "missing precondition", revision: -1, source: first.CurrentVersionID,
			wantStatus: http.StatusPreconditionRequired, wantCode: "precondition_required"},
		{name: "stale", revision: first.Revision, source: first.CurrentVersionID,
			wantStatus: http.StatusPreconditionFailed, wantCode: "stale_revision"},
		{name: "other node", revision: replaced.Node.Revision, source: second.CurrentVersionID,
			wantStatus: http.StatusUnprocessableEntity, wantCode: "version_node_mismatch"},
		{name: "current", revision: replaced.Node.Revision, source: replaced.Version.ID,
			wantStatus: http.StatusUnprocessableEntity, wantCode: "version_already_current"},
		{name: "missing", revision: replaced.Node.Revision,
			source:     "11111111-1111-4111-8111-111111111111",
			wantStatus: http.StatusNotFound, wantCode: "not_found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, responseBody := postContentRevert(
				t, ts.URL, ts.Client(), first.ID, tt.revision, tt.source,
			)
			assert.Equal(t, tt.wantStatus, resp.StatusCode, string(responseBody))
			assert.Contains(t, string(responseBody), fmt.Sprintf(`"code":%q`, tt.wantCode))
		})
	}

	unchanged, err := s.NodeByID(t.Context(), first.ID)
	require.NoError(t, err)
	assert.Equal(t, replaced.Node.Revision, unchanged.Revision)
	versions, total, err := s.ContentVersions(t.Context(), first.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 2, total)
	require.Len(t, versions, 2)
	assert.Equal(t, "content_replace", versions[0].TransitionKind)
}
