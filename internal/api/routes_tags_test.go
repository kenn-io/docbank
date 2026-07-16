package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestTagLifecycleHTTP(t *testing.T) {
	ts, s := newTestServer(t, nil)
	node, err := s.Mkdir(t.Context(), s.RootID(), "records")
	require.NoError(t, err)

	resp, body := do(t, ts, http.MethodPost, "/api/v1/tags", nil,
		map[string]any{"name": "taxes"})
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var tag api.Tag
	require.NoError(t, json.Unmarshal([]byte(body), &tag))
	assert.Equal(t, "taxes", tag.Name)
	assert.Equal(t, int64(1), tag.Revision)
	assert.Regexp(t, `^[0-9a-f-]{36}$`, tag.ID)
	assert.Equal(t, `"1"`, resp.Header.Get("ETag"))

	resp, body = do(t, ts, http.MethodPost, "/api/v1/tags", nil,
		map[string]any{"name": "taxes"})
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Contains(t, body, `"code":"exists"`)

	resp, body = get(t, ts, "/api/v1/tags/by-name?name="+url.QueryEscape("taxes"), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var resolved api.Tag
	require.NoError(t, json.Unmarshal([]byte(body), &resolved))
	assert.Equal(t, tag.ID, resolved.ID)
	assert.Equal(t, `"1"`, resp.Header.Get("ETag"))

	assignmentPath := fmt.Sprintf("/api/v1/nodes/%d/tags/%s", node.ID, tag.ID)
	resp, body = do(t, ts, http.MethodPut, assignmentPath, nil, nil)
	assert.Equal(t, http.StatusPreconditionRequired, resp.StatusCode)
	assert.Contains(t, body, `"code":"precondition_required"`)
	assert.Contains(t, body, "read the target resource")

	resp, body = do(t, ts, http.MethodPut, assignmentPath,
		map[string]string{"If-Match": `"1"`}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var assigned api.TagAssignmentReceipt
	require.NoError(t, json.Unmarshal([]byte(body), &assigned))
	assert.True(t, assigned.Changed)
	assert.Equal(t, 1, assigned.Tag.AssignmentCount)
	assert.Equal(t, int64(2), assigned.Tag.Revision)
	assert.Equal(t, int64(2), assigned.Node.Revision)
	assert.Equal(t, "/records", assigned.Node.Path)
	assert.Equal(t, `"2"`, resp.Header.Get("ETag"))

	resp, body = do(t, ts, http.MethodPut, assignmentPath,
		map[string]string{"If-Match": `"2"`}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &assigned))
	assert.False(t, assigned.Changed)
	assert.Equal(t, int64(2), assigned.Node.Revision)

	resp, body = get(t, ts,
		fmt.Sprintf("/api/v1/nodes/%d/tags?limit=10&offset=0", node.ID), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var tags api.TagPage
	require.NoError(t, json.Unmarshal([]byte(body), &tags))
	assert.Equal(t, 1, tags.Total)
	require.Len(t, tags.Items, 1)
	assert.Equal(t, tag.ID, tags.Items[0].ID)

	resp, body = get(t, ts, "/api/v1/tags/"+tag.ID+"/nodes?limit=10&offset=0", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var nodes api.TaggedNodePage
	require.NoError(t, json.Unmarshal([]byte(body), &nodes))
	assert.Equal(t, 1, nodes.Total)
	require.Len(t, nodes.Items, 1)
	assert.Equal(t, node.ID, nodes.Items[0].Node.ID)
	assert.Equal(t, "/records", nodes.Items[0].Path)

	resp, body = do(t, ts, http.MethodPatch, "/api/v1/tags/"+tag.ID, nil,
		map[string]any{"name": "tax records"})
	assert.Equal(t, http.StatusPreconditionRequired, resp.StatusCode)
	assert.Contains(t, body, `"code":"precondition_required"`)
	assert.Contains(t, body, "read the target resource")

	resp, body = do(t, ts, http.MethodPatch, "/api/v1/tags/"+tag.ID,
		map[string]string{"If-Match": `"2"`}, map[string]any{"name": "tax records"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &tag))
	assert.Equal(t, "tax records", tag.Name)
	assert.Equal(t, int64(3), tag.Revision)
	assert.Equal(t, `"3"`, resp.Header.Get("ETag"))
	afterRename, err := s.NodeByID(t.Context(), node.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(3), afterRename.Revision)

	resp, body = do(t, ts, http.MethodDelete, "/api/v1/tags/"+tag.ID, nil, nil)
	assert.Equal(t, http.StatusPreconditionRequired, resp.StatusCode)
	assert.Contains(t, body, `"code":"precondition_required"`)

	resp, body = do(t, ts, http.MethodDelete, "/api/v1/tags/"+tag.ID,
		map[string]string{"If-Match": `"3"`}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var deleted api.TagDeletionReceipt
	require.NoError(t, json.Unmarshal([]byte(body), &deleted))
	assert.Equal(t, tag.ID, deleted.Tag.ID)
	assert.Equal(t, 1, deleted.RemovedAssignments)
	assert.Equal(t, int64(3), deleted.Tag.Revision)
	assert.Equal(t, `"3"`, resp.Header.Get("ETag"))
	afterDelete, err := s.NodeByID(t.Context(), node.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(4), afterDelete.Revision)

	resp, body = get(t, ts, "/api/v1/tags?limit=100&offset=0", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &tags))
	assert.Zero(t, tags.Total)
	assert.Empty(t, tags.Items)
}

func TestTagAssignmentRejectsStaleRevisionAndInvalidName(t *testing.T) {
	ts, s := newTestServer(t, nil)
	node, err := s.Mkdir(t.Context(), s.RootID(), "node")
	require.NoError(t, err)
	tag, err := s.CreateTag(t.Context(), "tag")
	require.NoError(t, err)
	resp, body := do(t, ts, http.MethodPatch, "/api/v1/tags/"+tag.ID,
		map[string]string{"If-Match": `"-1"`}, map[string]any{"name": "renamed"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, body, "positive resource revision")

	path := fmt.Sprintf("/api/v1/nodes/%d/tags/%s", node.ID, tag.ID)
	resp, body = do(t, ts, http.MethodPut, path,
		map[string]string{"If-Match": strconv.Quote("99")}, nil)
	assert.Equal(t, http.StatusPreconditionFailed, resp.StatusCode)
	assert.Contains(t, body, `"code":"stale_revision"`)
	resp, body = do(t, ts, http.MethodPut, path,
		map[string]string{"If-Match": strconv.Quote(strconv.FormatInt(node.Revision, 10))}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)

	resp, body = do(t, ts, http.MethodPatch, "/api/v1/tags/"+tag.ID,
		map[string]string{"If-Match": `"1"`}, map[string]any{"name": "renamed"})
	assert.Equal(t, http.StatusPreconditionFailed, resp.StatusCode)
	assert.Contains(t, body, `"code":"stale_revision"`)
	resp, body = do(t, ts, http.MethodDelete, "/api/v1/tags/"+tag.ID,
		map[string]string{"If-Match": `"1"`}, nil)
	assert.Equal(t, http.StatusPreconditionFailed, resp.StatusCode)
	assert.Contains(t, body, `"code":"stale_revision"`)
	current, err := s.TagByID(t.Context(), tag.ID)
	require.NoError(t, err)
	assert.Equal(t, "tag", current.Name)
	assert.Equal(t, int64(2), current.Revision)
	assert.Equal(t, 1, current.AssignmentCount)

	resp, body = do(t, ts, http.MethodPost, "/api/v1/tags", nil,
		map[string]any{"name": "bad\x00tag"})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Contains(t, body, `"code":"invalid_tag"`)
}

func TestTagPathAssignmentUsesCurrentTopology(t *testing.T) {
	ts, s := newTestServer(t, nil)
	left, err := s.Mkdir(t.Context(), s.RootID(), "left")
	require.NoError(t, err)
	right, err := s.Mkdir(t.Context(), s.RootID(), "right")
	require.NoError(t, err)
	leaf, err := s.Mkdir(t.Context(), left.ID, "leaf")
	require.NoError(t, err)
	tag, err := s.CreateTag(t.Context(), "topology")
	require.NoError(t, err)
	left, err = s.NodeByID(t.Context(), left.ID)
	require.NoError(t, err)
	_, err = s.Move(t.Context(), left.ID, right.ID, "moved", left.Revision)
	require.NoError(t, err)

	path := "/api/v1/path/tags/" + tag.ID
	resp, body := do(t, ts, http.MethodPut, path, nil,
		map[string]any{"path": "/left/leaf"})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Contains(t, body, `"code":"not_found"`)

	resp, body = do(t, ts, http.MethodPut, path, nil,
		map[string]any{"path": "/right/moved/leaf"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var assigned api.TagAssignmentReceipt
	require.NoError(t, json.Unmarshal([]byte(body), &assigned))
	assert.Equal(t, leaf.ID, assigned.Node.ID)
	assert.Equal(t, "/right/moved/leaf", assigned.Node.Path)
	assert.True(t, assigned.Changed)
	assert.Equal(t, `"2"`, resp.Header.Get("ETag"))

	resp, body = do(t, ts, http.MethodDelete, path, nil,
		map[string]any{"path": "/right/moved/leaf"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &assigned))
	assert.Equal(t, leaf.ID, assigned.Node.ID)
	assert.Equal(t, "/right/moved/leaf", assigned.Node.Path)
	assert.True(t, assigned.Changed)

	resp, body = do(t, ts, http.MethodPut, path, nil,
		map[string]any{"path": "right/moved/leaf"})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Contains(t, body, `"code":"validation"`)
}
