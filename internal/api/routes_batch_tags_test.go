package api_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestBatchTagsHTTPReplayAndStaleAtomicity(t *testing.T) {
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	one, err := s.Mkdir(ctx, s.RootID(), "one")
	require.NoError(t, err)
	two, err := s.Mkdir(ctx, s.RootID(), "two")
	require.NoError(t, err)
	tag, err := s.CreateTag(ctx, "Review")
	require.NoError(t, err)
	targets := []map[string]any{{"node_id": one.ID, "revision": one.Revision}, {"node_id": two.ID, "revision": two.Revision + 1}}
	request := map[string]any{"operation_id": "11111111-1111-4111-8111-111111111111", "tag_id": tag.ID, "assign": true, "nodes": targets}
	resp, body := do(t, ts, http.MethodPost, "/api/v1/batch/tags", nil, request)
	require.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, body)
	require.Contains(t, body, `"code":"stale_revision"`)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/batch/tags/preview", nil,
		map[string]any{"tag_id": tag.ID, "nodes": targets})
	require.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, body)
	require.Contains(t, body, `"code":"stale_revision"`)
	after, err := s.NodeByID(ctx, one.ID)
	require.NoError(t, err)
	require.Equal(t, one.Revision, after.Revision)
	targets[1]["revision"] = two.Revision
	resp, body = do(t, ts, http.MethodPost, "/api/v1/batch/tags", nil, request)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var receipt struct {
		OperationID     string `json:"operation_id"`
		TagID           string `json:"tag_id"`
		Assign          bool   `json:"assign"`
		TagRevision     int64  `json:"tag_revision"`
		AssignmentCount int    `json:"assignment_count"`
		Nodes           []struct {
			NodeID           int64 `json:"node_id"`
			ExpectedRevision int64 `json:"expected_revision"`
			Revision         int64 `json:"revision"`
			Changed          bool  `json:"changed"`
		} `json:"nodes"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &receipt))
	require.Equal(t, request["operation_id"], receipt.OperationID)
	require.Equal(t, tag.ID, receipt.TagID)
	require.True(t, receipt.Assign)
	require.Equal(t, tag.Revision+2, receipt.TagRevision)
	require.Equal(t, 2, receipt.AssignmentCount)
	require.Len(t, receipt.Nodes, 2)
	require.Equal(t, one.ID, receipt.Nodes[0].NodeID)
	require.Equal(t, two.ID, receipt.Nodes[1].NodeID)
	for _, result := range receipt.Nodes {
		require.True(t, result.Changed)
		require.Equal(t, result.ExpectedRevision+1, result.Revision)
	}
	_, err = s.UnassignTag(ctx, tag.ID, one.ID, receipt.Nodes[0].Revision)
	require.NoError(t, err)
	resp, replay := do(t, ts, http.MethodPost, "/api/v1/batch/tags", nil, request)
	require.Equal(t, http.StatusOK, resp.StatusCode, replay)
	require.JSONEq(t, body, replay)
	request["assign"] = false
	resp, body = do(t, ts, http.MethodPost, "/api/v1/batch/tags", nil, request)
	require.Equal(t, http.StatusConflict, resp.StatusCode, body)
	require.Contains(t, body, "batch_tag_operation_conflict")
}

func TestBatchTagsHTTPValidationAndPreview(t *testing.T) {
	ts, s := newTestServer(t, nil)
	node, err := s.Mkdir(t.Context(), s.RootID(), "selected")
	require.NoError(t, err)
	tag, err := s.CreateTag(t.Context(), "Review")
	require.NoError(t, err)
	request := map[string]any{"operation_id": "11111111-1111-4111-8111-111111111111", "tag_id": tag.ID, "nodes": []map[string]any{{"node_id": node.ID, "revision": node.Revision}}}
	resp, body := do(t, ts, http.MethodPost, "/api/v1/batch/tags", nil, request)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	request["assign"] = false
	resp, body = do(t, ts, http.MethodPost, "/api/v1/batch/tags", nil, request)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &result))
	require.Equal(t, false, result["assign"])
	preview := map[string]any{"tag_id": tag.ID, "nodes": request["nodes"]}
	resp, body = do(t, ts, http.MethodPost, "/api/v1/batch/tags/preview", nil, preview)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var observed struct {
		Nodes []struct {
			NodeID   int64 `json:"node_id"`
			Revision int64 `json:"revision"`
			Assigned bool  `json:"assigned"`
		} `json:"nodes"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &observed))
	require.Len(t, observed.Nodes, 1)
	require.Equal(t, node.ID, observed.Nodes[0].NodeID)
	require.Equal(t, node.Revision, observed.Nodes[0].Revision)
	require.False(t, observed.Nodes[0].Assigned)
	for _, nodes := range []any{[]any{}, []map[string]any{{"node_id": node.ID, "revision": 0}}, []map[string]any{{"node_id": node.ID, "revision": node.Revision}, {"node_id": node.ID, "revision": node.Revision}}} {
		request["nodes"] = nodes
		resp, body = do(t, ts, http.MethodPost, "/api/v1/batch/tags", nil, request)
		require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	}
}

func TestBatchTagsHTTPBrowserCapabilityIsExact(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp, body := do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var session struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &session))
	headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: session.Token}
	for _, path := range []string{"/api/v1/batch/tags", "/api/v1/batch/tags/preview"} {
		resp, body = do(t, ts, http.MethodPost, path, headers, map[string]any{})
		require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
		resp, body = do(t, ts, http.MethodPost, path, map[string]string{"X-Api-Key": ""}, map[string]any{})
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode, body)
		for _, suffix := range []string{"?extra=true", "/extra"} {
			resp, body = do(t, ts, http.MethodPost, path+suffix, headers, map[string]any{})
			require.Equal(t, http.StatusForbidden, resp.StatusCode, body)
		}
		resp, body = do(t, ts, http.MethodDelete, path, headers, nil)
		require.Equal(t, http.StatusForbidden, resp.StatusCode, body)
	}
}

func TestBatchTagsHTTPBoundsAndUnavailableTargets(t *testing.T) {
	ts, s := newTestServer(t, nil)
	node, err := s.Mkdir(t.Context(), s.RootID(), "selected")
	require.NoError(t, err)
	tag, err := s.CreateTag(t.Context(), "Review")
	require.NoError(t, err)
	request := map[string]any{"operation_id": "11111111-1111-4111-8111-111111111111", "tag_id": tag.ID, "assign": true,
		"nodes": []map[string]any{{"node_id": node.ID, "revision": node.Revision}}}
	for _, field := range []string{"operation_id", "tag_id", "assign", "nodes"} {
		original := request[field]
		request[field] = nil
		resp, body := do(t, ts, http.MethodPost, "/api/v1/batch/tags", nil, request)
		require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
		request[field] = original
	}
	tooMany := make([]map[string]any, 1001)
	for i := range tooMany {
		tooMany[i] = map[string]any{"node_id": i + 1, "revision": 1}
	}
	request["nodes"] = tooMany
	resp, body := do(t, ts, http.MethodPost, "/api/v1/batch/tags", nil, request)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	request["nodes"] = []map[string]any{{"node_id": 999999, "revision": 1}}
	resp, body = do(t, ts, http.MethodPost, "/api/v1/batch/tags", nil, request)
	require.Equal(t, http.StatusNotFound, resp.StatusCode, body)
	trashed, _, err := s.Trash(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	request["nodes"] = []map[string]any{{"node_id": node.ID, "revision": trashed.Revision}}
	resp, body = do(t, ts, http.MethodPost, "/api/v1/batch/tags", nil, request)
	require.Equal(t, http.StatusNotFound, resp.StatusCode, body)
	preview := map[string]any{"tag_id": tag.ID, "nodes": request["nodes"]}
	resp, body = do(t, ts, http.MethodPost, "/api/v1/batch/tags/preview", nil, preview)
	require.Equal(t, http.StatusNotFound, resp.StatusCode, body)
	require.NoError(t, s.Close())
	resp, body = do(t, ts, http.MethodPost, "/api/v1/batch/tags", nil, request)
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode, body)
}
