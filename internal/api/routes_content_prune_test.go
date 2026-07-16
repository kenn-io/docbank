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
)

func postContentPrune(
	t *testing.T, tsURL string, client *http.Client, nodeID int64, revision *int64, body any,
) (*http.Response, []byte) {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("%s/api/v1/nodes/%d/versions/prune", tsURL, nodeID), bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if revision != nil {
		req.Header.Set("If-Match", strconv.Quote(strconv.FormatInt(*revision, 10)))
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	responseBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, responseBody
}

func TestContentVersionPrunePreviewsThenReleasesHistory(t *testing.T) {
	ts, s := newTestServer(t, nil)
	created := createFileWithContent(t, ts, s, "/report.txt", "old content")
	oldVersionID := created.CurrentVersionID
	oldHash := created.BlobHash
	replacement := []byte("current content")
	resp, body := sendContentReplacement(t, ts.URL, ts.Client(), created.ID, created.Revision,
		replacement, uploadIdentity(replacement), int64(len(replacement)), "text/plain")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var replaced api.ContentReplacementReceipt
	require.NoError(t, json.Unmarshal(body, &replaced))

	revision := replaced.Node.Revision
	resp, body = postContentPrune(t, ts.URL, ts.Client(), created.ID, &revision,
		map[string]any{"keep_newest": 1})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Equal(t, strconv.Quote(strconv.FormatInt(revision, 10)), resp.Header.Get("ETag"))
	var preview api.VersionPruneReport
	require.NoError(t, json.Unmarshal(body, &preview))
	assert.False(t, preview.Run)
	assert.False(t, preview.Changed)
	assert.Equal(t, 1, preview.UniqueBlobs)
	assert.Equal(t, 1, preview.ReleasableBlobs)
	assert.Equal(t, int64(len("old content")), preview.LooseBytesPendingGC)
	require.Len(t, preview.Candidates, 1)
	assert.Equal(t, oldVersionID, preview.Candidates[0].ID)
	_, err := s.ContentVersionByID(t.Context(), oldVersionID)
	require.NoError(t, err, "preview must not alter history")

	resp, body = postContentPrune(t, ts.URL, ts.Client(), created.ID, &revision,
		map[string]any{"keep_newest": 1, "run": true})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Equal(t, strconv.Quote(strconv.FormatInt(revision+1, 10)), resp.Header.Get("ETag"))
	var receipt api.VersionPruneReport
	require.NoError(t, json.Unmarshal(body, &receipt))
	assert.True(t, receipt.Run)
	assert.True(t, receipt.Changed)
	assert.Equal(t, 1, receipt.DeletedVersions)
	assert.Equal(t, revision+1, receipt.Node.Revision)
	assert.Equal(t, replaced.Version.ID, receipt.Node.CurrentVersionID)

	oldResp, err := ts.Client().Get(ts.URL + "/api/v1/versions/" + oldVersionID)
	require.NoError(t, err)
	require.NoError(t, oldResp.Body.Close())
	assert.Equal(t, http.StatusNotFound, oldResp.StatusCode)
	unreachable, err := s.UnreachableBlobs(t.Context())
	require.NoError(t, err)
	require.Len(t, unreachable, 1)
	assert.Equal(t, oldHash, unreachable[0].Hash,
		"pruning releases authority but leaves physical reclamation to GC")
}

func TestContentVersionPruneRequiresRevisionAndOneSelector(t *testing.T) {
	ts, s := newTestServer(t, nil)
	created := createFileWithContent(t, ts, s, "/report.txt", "content")

	resp, body := postContentPrune(t, ts.URL, ts.Client(), created.ID, nil,
		map[string]any{"all_prior": true})
	assert.Equal(t, http.StatusPreconditionRequired, resp.StatusCode, string(body))
	assert.Contains(t, string(body), `"code":"precondition_required"`)

	revision := created.Revision
	resp, body = postContentPrune(t, ts.URL, ts.Client(), created.ID, &revision,
		map[string]any{"keep_newest": 1, "all_prior": true})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, string(body))
	assert.Contains(t, string(body), `"code":"invalid_version_prune"`)

	resp, body = postContentPrune(t, ts.URL, ts.Client(), created.ID, &revision,
		map[string]any{"older_than": "0h", "all_prior": true, "run": true})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, string(body))
	assert.Contains(t, string(body), `"code":"invalid_version_prune"`)

	resp, body = postContentPrune(t, ts.URL, ts.Client(), created.ID, &revision,
		map[string]any{"version_ids": []string{"not-a-uuid"}})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, string(body))

	_, total, err := s.ContentVersions(t.Context(), created.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, total, "invalid execution must not alter history")

	stale := revision + 1
	resp, body = postContentPrune(t, ts.URL, ts.Client(), created.ID, &stale,
		map[string]any{"all_prior": true, "run": true})
	assert.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, string(body))
	assert.Contains(t, string(body), `"code":"stale_revision"`)
}
