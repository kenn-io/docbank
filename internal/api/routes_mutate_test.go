package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestCreateDirectory(t *testing.T) {
	ts, s := newTestServer(t, nil)
	resp, body := do(t, ts, http.MethodPost, "/api/v1/nodes", nil,
		map[string]any{"parent_id": s.RootID(), "name": "taxes", "kind": "dir"})
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var n api.Node
	require.NoError(t, json.Unmarshal([]byte(body), &n))
	assert.Equal(t, "/taxes", n.Path)

	// Name collision → 409 exists; kind=file → 422 (multipart is planned).
	resp, body = do(t, ts, http.MethodPost, "/api/v1/nodes", nil,
		map[string]any{"parent_id": s.RootID(), "name": "taxes", "kind": "dir"})
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Contains(t, body, `"code":"exists"`)
	resp, _ = do(t, ts, http.MethodPost, "/api/v1/nodes", nil,
		map[string]any{"parent_id": s.RootID(), "name": "f.txt", "kind": "file"})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
}

func TestMoveRequiresIfMatch(t *testing.T) {
	ts, s := newTestServer(t, nil)
	f := createFileWithContent(t, ts, s, "/a.txt", "x")

	resp, body := do(t, ts, http.MethodPatch, fmt.Sprintf("/api/v1/nodes/%d", f.ID),
		nil, map[string]any{"new_name": "b.txt"})
	assert.Equal(t, http.StatusPreconditionRequired, resp.StatusCode)
	assert.Contains(t, body, `"code":"precondition_required"`)

	resp, body = do(t, ts, http.MethodPatch, fmt.Sprintf("/api/v1/nodes/%d", f.ID),
		map[string]string{"If-Match": `"999"`}, map[string]any{"new_name": "b.txt"})
	assert.Equal(t, http.StatusPreconditionFailed, resp.StatusCode)
	assert.Contains(t, body, `"code":"stale_revision"`)

	// "-1" is the store's unconditional sentinel; via HTTP it must be a 400,
	// never a precondition bypass.
	resp, body = do(t, ts, http.MethodPatch, fmt.Sprintf("/api/v1/nodes/%d", f.ID),
		map[string]string{"If-Match": `"-1"`}, map[string]any{"new_name": "b.txt"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, body, `"code":"validation"`)

	// Unbalanced or nested quotes are malformed, not a lenient parse of the
	// digits inside.
	for _, bad := range []string{`"3`, `3"`, `"""3"""`, `"`} {
		resp, body = do(t, ts, http.MethodPatch, fmt.Sprintf("/api/v1/nodes/%d", f.ID),
			map[string]string{"If-Match": bad}, map[string]any{"new_name": "b.txt"})
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "If-Match %q", bad)
		assert.Contains(t, body, `"code":"validation"`, "If-Match %q", bad)
	}

	_, etag := etagOf(t, ts, f.ID)
	resp, body = do(t, ts, http.MethodPatch, fmt.Sprintf("/api/v1/nodes/%d", f.ID),
		map[string]string{"If-Match": etag}, map[string]any{"new_name": "b.txt"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var n api.Node
	require.NoError(t, json.Unmarshal([]byte(body), &n))
	assert.Equal(t, "/b.txt", n.Path)

	// Empty patch body → 422.
	_, etag = etagOf(t, ts, f.ID)
	resp, _ = do(t, ts, http.MethodPatch, fmt.Sprintf("/api/v1/nodes/%d", f.ID),
		map[string]string{"If-Match": etag}, map[string]any{})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
}

func TestMovePathAndTrashPath(t *testing.T) {
	ts, s := newTestServer(t, nil)
	_, err := s.Mkdir(t.Context(), s.RootID(), "docs")
	require.NoError(t, err)
	_, err = s.Mkdir(t.Context(), s.RootID(), "filed")
	require.NoError(t, err)
	f := createFileWithContent(t, ts, s, "/a.txt", "x")
	_, err = s.Move(t.Context(), f.ID, s.RootID(), "a.txt", store.UnconditionalRev)
	require.NoError(t, err)
	_, err = s.MovePath(t.Context(), "/a.txt", "/docs")
	require.NoError(t, err)

	resp, body := do(t, ts, http.MethodPost, "/api/v1/path/move", nil,
		map[string]any{"src_path": "/docs/a.txt", "dest_path": "/filed"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var moved api.Node
	require.NoError(t, json.Unmarshal([]byte(body), &moved))
	assert.Equal(t, "/filed/a.txt", moved.Path)

	resp, body = do(t, ts, http.MethodPost, "/api/v1/path/trash", nil,
		map[string]any{"path": "/filed/a.txt"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var trashed api.Node
	require.NoError(t, json.Unmarshal([]byte(body), &trashed))
	assert.Equal(t, moved.ID, trashed.ID)
	assert.NotEmpty(t, trashed.TrashedAt)
	// Pre-trash location, not the post-trash "/a.txt" the store's
	// root-re-parenting would yield.
	assert.Equal(t, "/filed/a.txt", trashed.Path)

	resp, body = do(t, ts, http.MethodPost, "/api/v1/path/move", nil,
		map[string]any{"src_path": "relative", "dest_path": "/x"})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Contains(t, body, `"code":"validation"`)
}

func TestTrashAndRestoreRoundTripHTTP(t *testing.T) {
	ts, s := newTestServer(t, nil)
	_, err := s.Mkdir(t.Context(), s.RootID(), "inbox")
	require.NoError(t, err)
	f := createFileWithContent(t, ts, s, "/doc.txt", "x")
	_, err = s.MovePath(t.Context(), "/doc.txt", "/inbox")
	require.NoError(t, err)

	_, etag := etagOf(t, ts, f.ID)
	resp, body := do(t, ts, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/trash", f.ID),
		map[string]string{"If-Match": etag}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var n api.Node
	require.NoError(t, json.Unmarshal([]byte(body), &n))
	assert.NotEmpty(t, n.TrashedAt)
	// Pre-trash location: the store re-parents trash roots to the vault
	// root, which would misreport this as "/doc.txt".
	assert.Equal(t, "/inbox/doc.txt", n.Path)

	_, etag = etagOf(t, ts, f.ID)
	resp, body = do(t, ts, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/restore", f.ID),
		map[string]string{"If-Match": etag}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	// Fresh variable: trashed_at is omitempty, so the restore response omits
	// the key entirely on success, and unmarshaling into the already-set n
	// from above would silently leave its stale value in place.
	var restored api.Node
	require.NoError(t, json.Unmarshal([]byte(body), &restored))
	assert.Empty(t, restored.TrashedAt)
	assert.Equal(t, "/inbox/doc.txt", restored.Path)
}
