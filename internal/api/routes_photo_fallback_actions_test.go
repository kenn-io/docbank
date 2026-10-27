package api_test

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestReviewFallbackAlbumKeepsCapturedMembership(t *testing.T) {
	t.Parallel()
	ts, vault := newTestServer(t, nil)
	ctx := t.Context()
	hash, size, err := vault.Blobs.Write(strings.NewReader("synthetic photo"))
	require.NoError(t, err)
	_, err = vault.CreateFile(ctx, vault.RootID(), "sunset-first.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	request := api.PhotoBrowseRequest{Query: api.QueryPayload(`{"mode":"semantic","text":"sunset"}`)}
	response, body := do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var page api.PhotoBrowsePage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.NotEmpty(t, page.SnapshotID)
	require.Equal(t, "lexical", page.ActualMode)
	require.Len(t, page.Items, 1)
	_, err = vault.CreateFile(ctx, vault.RootID(), "sunset-later.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	request.SnapshotID = page.SnapshotID
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var reopened api.PhotoBrowsePage
	require.NoError(t, json.Unmarshal([]byte(body), &reopened))
	require.Len(t, reopened.Items, 1)
	client := daemonconn.New(ts.URL, testAPIKey)
	album, err := client.CreatePhotoAlbum(ctx, "Captured sunset")
	require.NoError(t, err)
	_, err = client.AddPhotoAlbumMembers(ctx, album.ID, album.Revision, api.PhotoAlbumMembersRequest{AssetIDs: []string{reopened.Items[0].AssetID}})
	require.NoError(t, err)
	summary, err := client.PhotoAlbum(ctx, album.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), summary.MemberCount)
}
