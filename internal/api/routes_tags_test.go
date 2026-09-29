package api_test

import (
	"encoding/json/v2"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoVisibilityTags(t *testing.T) {
	ts, s := newTestServer(t, nil)
	ownerA, err := s.CreatePhotoOwner(t.Context(), "Tag owner A")
	require.NoError(t, err)
	ownerB, err := s.CreatePhotoOwner(t.Context(), "Tag owner B")
	require.NoError(t, err)
	photoA, err := s.CreateFile(store.WithPhotoOwner(t.Context(), ownerA.ID), s.RootID(), "tag-a.jpg", testHash("tag-a"), 5, "image/jpeg")
	require.NoError(t, err)
	photoB, err := s.CreateFile(store.WithPhotoOwner(t.Context(), ownerB.ID), s.RootID(), "tag-b.jpg", testHash("tag-b"), 5, "image/jpeg")
	require.NoError(t, err)
	ordinary, err := s.CreateFile(t.Context(), s.RootID(), "tag-ordinary.txt", testHash("tag-ordinary"), 5, "text/plain")
	require.NoError(t, err)
	tag, err := s.CreateTag(t.Context(), "photo-visible")
	require.NoError(t, err)

	ownerAHeaders := map[string]string{api.WebSessionHeader: issuePhotoOwnerSession(t, ts, ownerA.ID), "X-Api-Key": ""}
	ownerBHeaders := map[string]string{api.WebSessionHeader: issuePhotoOwnerSession(t, ts, ownerB.ID), "X-Api-Key": ""}
	assignmentPath := func(nodeID int64) string {
		return fmt.Sprintf("/api/v1/nodes/%d/tags/%s", nodeID, tag.ID)
	}
	ifMatch := func(revision int64) map[string]string {
		return map[string]string{"If-Match": strconv.Quote(strconv.FormatInt(revision, 10))}
	}

	response, body := do(t, ts, http.MethodPut, assignmentPath(photoA.ID), mergeHeaders(ownerAHeaders, ifMatch(photoA.Revision)), nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var receipt api.TagAssignmentReceipt
	require.NoError(t, json.Unmarshal([]byte(body), &receipt))
	assert.True(t, receipt.Changed)
	assert.Equal(t, 1, receipt.Tag.AssignmentCount)
	assert.Equal(t, photoA.Revision+1, receipt.Node.Revision)

	response, body = do(t, ts, http.MethodPut, assignmentPath(photoA.ID), mergeHeaders(ownerBHeaders, ifMatch(receipt.Node.Revision)), nil)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
	response, body = do(t, ts, http.MethodDelete, assignmentPath(photoA.ID), mergeHeaders(ownerBHeaders, ifMatch(receipt.Node.Revision)), nil)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
	afterForeign, err := s.NodeByID(t.Context(), photoA.ID)
	require.NoError(t, err)
	assert.Equal(t, receipt.Node.Revision, afterForeign.Revision)

	response, body = do(t, ts, http.MethodPut, assignmentPath(photoB.ID), mergeHeaders(ownerBHeaders, ifMatch(photoB.Revision)), nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &receipt))
	assert.Equal(t, 1, receipt.Tag.AssignmentCount)

	response, body = do(t, ts, http.MethodPut, assignmentPath(ordinary.ID), mergeHeaders(ownerBHeaders, ifMatch(ordinary.Revision)), nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &receipt))
	assert.Equal(t, 2, receipt.Tag.AssignmentCount)

	assertTagCount := func(headers map[string]string, count int) {
		t.Helper()
		response, body := get(t, ts, "/api/v1/tags?limit=10&offset=0", headers)
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		var page api.TagPage
		require.NoError(t, json.Unmarshal([]byte(body), &page))
		require.Equal(t, 1, page.Total)
		require.Len(t, page.Items, 1)
		assert.Equal(t, count, page.Items[0].AssignmentCount)

		response, body = get(t, ts, "/api/v1/tags/by-name?name=photo-visible", headers)
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		var resolved api.Tag
		require.NoError(t, json.Unmarshal([]byte(body), &resolved))
		assert.Equal(t, count, resolved.AssignmentCount)

		response, body = get(t, ts, "/api/v1/tags/"+tag.ID, headers)
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		require.NoError(t, json.Unmarshal([]byte(body), &resolved))
		assert.Equal(t, count, resolved.AssignmentCount)
	}
	assertTagCount(ownerAHeaders, 2)
	assertTagCount(ownerBHeaders, 2)

	response, body = get(t, ts, fmt.Sprintf("/api/v1/nodes/%d/tags?limit=10&offset=0", ordinary.ID), ownerBHeaders)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var ordinaryTags api.TagPage
	require.NoError(t, json.Unmarshal([]byte(body), &ordinaryTags))
	require.Equal(t, 1, ordinaryTags.Total)
	require.Len(t, ordinaryTags.Items, 1)
	assert.Equal(t, 2, ordinaryTags.Items[0].AssignmentCount)

	response, body = get(t, ts, fmt.Sprintf("/api/v1/nodes/%d/tags?limit=10&offset=0", photoA.ID), ownerBHeaders)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
	response, body = get(t, ts, "/api/v1/tags/"+tag.ID+"/nodes?limit=10&offset=0", ownerAHeaders)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var ownerATagged api.TaggedNodePage
	require.NoError(t, json.Unmarshal([]byte(body), &ownerATagged))
	assert.Equal(t, 2, ownerATagged.Total)
	assert.Len(t, ownerATagged.Items, 2)
	response, body = get(t, ts, "/api/v1/tags/"+tag.ID+"/nodes?limit=10&offset=0", ownerBHeaders)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var ownerBTagged api.TaggedNodePage
	require.NoError(t, json.Unmarshal([]byte(body), &ownerBTagged))
	assert.Equal(t, 2, ownerBTagged.Total)
	assert.Len(t, ownerBTagged.Items, 2)

	response, body = do(t, ts, http.MethodDelete, assignmentPath(photoB.ID), mergeHeaders(ownerBHeaders, ifMatch(receipt.Node.Revision)), nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &receipt))
	assert.True(t, receipt.Changed)
	assert.Equal(t, 1, receipt.Tag.AssignmentCount)
	response, body = do(t, ts, http.MethodDelete, assignmentPath(photoA.ID), mergeHeaders(ownerAHeaders, ifMatch(afterForeign.Revision)), nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &receipt))
	assert.True(t, receipt.Changed)
	assert.Equal(t, 1, receipt.Tag.AssignmentCount)
}

func mergeHeaders(base, extra map[string]string) map[string]string {
	merged := make(map[string]string, len(base)+len(extra))
	maps.Copy(merged, base)
	maps.Copy(merged, extra)
	return merged
}

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

	resp, body = get(t, ts,
		"/api/v1/tags/"+tag.ID+"/nodes?limit=10&offset=0&live_only=true", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &nodes))
	assert.Equal(t, 1, nodes.Total)
	assert.Equal(t, 0, nodes.OmittedTrashed)
	require.Len(t, nodes.Items, 1)
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

func TestSharedAuditedTagDefinitionChangesHTTP(t *testing.T) {
	ts, s := newTestServer(t, nil)
	first, err := s.Mkdir(t.Context(), s.RootID(), "taxes")
	require.NoError(t, err)
	second, err := s.Mkdir(t.Context(), s.RootID(), "contracts")
	require.NoError(t, err)
	for _, node := range []int64{first.ID, second.ID} {
		plan, err := s.PreviewInitialAudit(t.Context(), node, "api", nil)
		require.NoError(t, err)
		_, err = s.EnableInitialAudit(t.Context(), plan)
		require.NoError(t, err)
	}
	tag, err := s.CreateTag(t.Context(), "records")
	require.NoError(t, err)
	firstAssignment, err := s.AssignTag(t.Context(), tag.ID, first.ID, first.Revision)
	require.NoError(t, err)
	secondAssignment, err := s.AssignTag(t.Context(), tag.ID, second.ID, second.Revision)
	require.NoError(t, err)

	resp, body := do(t, ts, http.MethodPatch, "/api/v1/tags/"+tag.ID,
		map[string]string{"If-Match": strconv.Quote(strconv.FormatInt(
			secondAssignment.Tag.Revision, 10,
		))}, map[string]any{"name": "permanent records"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var renamed api.Tag
	require.NoError(t, json.Unmarshal([]byte(body), &renamed))
	assert.Equal(t, "permanent records", renamed.Name)
	firstAfter, err := s.NodeByID(t.Context(), first.ID)
	require.NoError(t, err)
	secondAfter, err := s.NodeByID(t.Context(), second.ID)
	require.NoError(t, err)
	assert.Equal(t, firstAssignment.Node.Revision+1, firstAfter.Revision)
	assert.Equal(t, secondAssignment.Node.Revision+1, secondAfter.Revision)

	resp, body = do(t, ts, http.MethodDelete, "/api/v1/tags/"+tag.ID,
		map[string]string{"If-Match": strconv.Quote(strconv.FormatInt(renamed.Revision, 10))}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var deleted api.TagDeletionReceipt
	require.NoError(t, json.Unmarshal([]byte(body), &deleted))
	assert.Equal(t, 2, deleted.RemovedAssignments)
	require.NoError(t, s.ValidateMetadata(t.Context()))
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
	_, _, err = s.Move(t.Context(), left.ID, right.ID, "moved", left.Revision)
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
