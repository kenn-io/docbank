package api_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestSupersessionEndpoints(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	old := createFileWithContent(t, ts, s, "/old.txt", "old")
	next := createFileWithContent(t, ts, s, "/new.txt", "new")
	link := fmt.Sprintf("/api/v1/nodes/%d/supersedes/%d", next.ID, old.ID)
	window := fmt.Sprintf("/api/v1/nodes/%d/effective-window", next.ID)
	validity := fmt.Sprintf("/api/v1/nodes/%d/validity?as_of=2025-06-01", old.ID)
	resp, body := do(t, ts, http.MethodPut, link, nil, map[string]any{"note": "replacement"})
	require.Equal(t, http.StatusPreconditionRequired, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPut, link, map[string]string{"If-Match": `"1"`}, map[string]any{"note": "replacement"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var receipt api.ValidityReceipt
	require.NoError(t, json.Unmarshal([]byte(body), &receipt))
	assert.Equal(t, "/old.txt", receipt.Node.Path)
	assert.Equal(t, `"2"`, resp.Header.Get("ETag"))
	resp, body = do(t, ts, http.MethodPut, link, map[string]string{"If-Match": `"1"`}, map[string]any{})
	assert.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPut, window, map[string]string{"If-Match": `"1"`}, map[string]any{"effective_from": "2025-01-01"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &receipt))
	assert.Equal(t, "/new.txt", receipt.Node.Path)
	resp, body = get(t, ts, validity, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var view api.NodeValidity

	require.NoError(t, json.Unmarshal([]byte(body), &view))
	assert.Equal(t, "/old.txt", view.Node.Path)
	assert.Equal(t, "2025-06-01", view.AsOf)
	assert.Equal(t, "superseded", view.State)
	assert.False(t, view.Undated)
	require.Len(t, view.Successors, 1)
	assert.Equal(t, next.ID, view.Successors[0].SuccessorID)
	resp, body = get(t, ts, fmt.Sprintf("/api/v1/nodes/%d/validity?as_of=2025-02-30", old.ID), nil)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodDelete, link, map[string]string{"If-Match": `"2"`}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &receipt))
	assert.Equal(t, "/old.txt", receipt.Node.Path)
	assert.Equal(t, `"3"`, resp.Header.Get("ETag"))
	resp, body = do(t, ts, http.MethodDelete, link, map[string]string{"If-Match": `"3"`}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Equal(t, `"3"`, resp.Header.Get("ETag"))
	resp, body = do(t, ts, http.MethodDelete, link, map[string]string{"If-Match": `"2"`}, nil)
	assert.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodDelete, window, map[string]string{"If-Match": `"2"`}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &receipt))
	assert.Equal(t, "/new.txt", receipt.Node.Path)
	resp, body = get(t, ts, fmt.Sprintf("/api/v1/nodes/%d/validity?as_of=2025-06-01", next.ID), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &view))
	assert.Equal(t, "/new.txt", view.Node.Path)
	assert.Empty(t, view.EffectiveFrom)
}

func TestSupersessionRejectsInvalidDeclarations(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	old := createFileWithContent(t, ts, s, "/old.txt", "old")
	headers := map[string]string{"If-Match": `"1"`}
	resp, body := do(t, ts, http.MethodPut, fmt.Sprintf("/api/v1/nodes/%d/supersedes/%d", old.ID, old.ID), headers, map[string]any{})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
}
