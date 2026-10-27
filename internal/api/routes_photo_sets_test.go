package api_test

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoAlbumRoutesAndClient(t *testing.T) {
	t.Parallel()
	ts, vault := newTestServer(t, nil)
	ctx := t.Context()
	hash, size, err := vault.Blobs.Write(strings.NewReader("synthetic photo"))
	require.NoError(t, err)
	node, err := vault.CreateFile(ctx, vault.RootID(), "photo.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	asset, err := vault.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	c := daemonconn.New(ts.URL, testAPIKey)
	album, err := c.CreatePhotoAlbum(ctx, "Holiday")
	require.NoError(t, err)
	require.Equal(t, int64(1), album.Revision)
	resp, body := do(t, ts, http.MethodPost, "/api/v1/photos/albums", nil, map[string]string{"name": "   "})
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	require.Equal(t, "invalid_photo_album", decodeProblem(t, body).Code)
	_, err = c.CreatePhotoAlbum(ctx, "   ")
	require.ErrorIs(t, err, store.ErrInvalidPhotoAlbum)
	path := "/api/v1/photos/albums/" + album.ID
	resp, body = do(t, ts, http.MethodPut, path, nil, map[string]string{"name": "Trip"})
	require.Equal(t, http.StatusPreconditionRequired, resp.StatusCode, body)
	album, err = c.UpdatePhotoAlbum(ctx, album.ID, album.Revision, api.UpdatePhotoAlbumRequest{Name: new("Trip"), Starred: new(true)})
	require.NoError(t, err)
	resp, body = do(t, ts, http.MethodPut, path+"/cover", map[string]string{"If-Match": `"2"`}, map[string]string{"asset_id": asset.ID})
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	require.Equal(t, "invalid_photo_album", decodeProblem(t, body).Code)
	resp, body = do(t, ts, http.MethodPost, path+"/members/add", map[string]string{"If-Match": `"1"`}, map[string]any{"asset_ids": []string{asset.ID}})
	require.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPost, path+"/members/add", map[string]string{"If-Match": `"2"`}, map[string]any{})
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	album, err = c.AddPhotoAlbumMembers(ctx, album.ID, album.Revision, api.PhotoAlbumMembersRequest{AssetIDs: []string{asset.ID}})
	require.NoError(t, err)
	album, err = c.SetPhotoAlbumCover(ctx, album.ID, album.Revision, &asset.ID)
	require.NoError(t, err)
	require.Equal(t, asset.ID, *album.CoverAssetID)
	summary, err := c.PhotoAlbum(ctx, album.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), summary.MemberCount)
	page, err := c.BrowsePhotoAlbum(ctx, api.PhotoBrowseRequest{Query: api.QueryPayload(`{"filters":{"set_ids":["` + album.ID + `"]},"sort":{"field":"added_time","direction":"desc"}}`)})
	require.NoError(t, err)
	require.Equal(t, int64(1), page.Total)
	require.Equal(t, asset.ID, page.Items[0].AssetID)
	_, err = c.DuplicatePhotoAlbum(ctx, album.ID, album.Revision, "Copy")
	require.NoError(t, err)
	albums, err := c.PhotoAlbums(ctx)
	require.NoError(t, err)
	require.Len(t, albums, 2)
	q := api.QueryPayload(`{"filters":{"set_ids":["` + album.ID + `"]},"sort":{"field":"added_time","direction":"desc"}}`)
	album, err = c.RemovePhotoAlbumMembers(ctx, album.ID, album.Revision, api.PhotoAlbumMembersRequest{Query: &q})
	require.NoError(t, err)
	album, err = c.DeletePhotoAlbum(ctx, album.ID, album.Revision)
	require.NoError(t, err)
	require.NotNil(t, album.DeletedAt)

	saved, err := vault.CreateSavedQuery(ctx, "Frozen search", "", store.SavedQueryKindQuery,
		[]byte(`{"filters":{"kinds":["photo","video"]},"sort":{"field":"name","direction":"asc"}}`))
	require.NoError(t, err)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/saved-queries/"+saved.ID+"/runs",
		map[string]string{"If-Match": `"1"`}, map[string]any{"photos": true})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var result api.SavedQueryRunResult
	require.NoError(t, json.Unmarshal([]byte(body), &result))
	resp, body = do(t, ts, http.MethodPost, "/api/v1/photos/albums", nil,
		map[string]string{"name": "Frozen", "snapshot_id": result.Snapshot.SnapshotID, "member_hash": result.Snapshot.MemberHash})
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var frozen api.PhotoAlbum
	require.NoError(t, json.Unmarshal([]byte(body), &frozen))
	require.NotEmpty(t, frozen.ID)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/photos/albums", nil,
		map[string]string{"name": "Invalid frozen album", "snapshot_id": result.Snapshot.SnapshotID})
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	require.Equal(t, "invalid_photo_album", decodeProblem(t, body).Code)
	require.Contains(t, decodeProblem(t, body).Detail, "member_hash is required")
	resp, body = do(t, ts, http.MethodPost, "/api/v1/photos/albums", nil,
		map[string]string{"name": "Missing snapshot", "member_hash": result.Snapshot.MemberHash})
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	require.Equal(t, "invalid_photo_album", decodeProblem(t, body).Code)
	require.Contains(t, decodeProblem(t, body).Detail, "snapshot_id is required")
	_, _, err = vault.Trash(ctx, node.ID, node.Revision)
	require.NoError(t, err)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/photos/albums", nil,
		map[string]string{"name": "Photo left Library", "snapshot_id": result.Snapshot.SnapshotID, "member_hash": result.Snapshot.MemberHash})
	require.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, body)
	require.Equal(t, "photo_snapshot_member_not_in_library", decodeProblem(t, body).Code)
}

func TestSavedPhotoRun(t *testing.T) {
	t.Parallel()
	ts, vault, _ := photoImageHTTPFixture(t, newProcessingTestEmbeddingProvider(t), []string{"sunset.jpg"})
	saved, err := vault.CreateSavedQuery(t.Context(), "Meaning", "", store.SavedQueryKindQuery, []byte(`{"mode":"semantic","text":"sunset","sort":{"field":"relevance","direction":"desc"}}`))
	require.NoError(t, err)
	resp, body := do(t, ts, http.MethodPost, "/api/v1/saved-queries/"+saved.ID+"/runs", map[string]string{"If-Match": `"1"`}, api.SavedQueryRunRequest{Photos: true, PageSize: 50})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var result api.SavedQueryRunResult
	require.NoError(t, json.Unmarshal([]byte(body), &result))
	require.Equal(t, "semantic", result.Snapshot.RequestedMode)
	require.NotEmpty(t, result.Run.RunID)
	require.Equal(t, "semantic", result.Snapshot.ActualMode)
	require.Empty(t, result.Snapshot.FallbackReason)
}
