package api_test

import (
	"encoding/json/v2"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/phototest"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/sqlite"
)

func TestPhotoSuggestionRoutes(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	hash, size, err := s.Blobs.Write(strings.NewReader("synthetic JPEG"))
	require.NoError(t, err)
	node, err := s.CreateFile(ctx, s.RootID(), "synthetic.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	db, err := store.DefaultSQLiteDriver().Open(s.DBPath, sqlite.OpenOptions{Access: sqlite.ReadWriteExisting, TransactionMode: sqlite.Deferred})
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	id, err := phototest.SeedSuggestion(ctx, db, node.CurrentVersionID, document.PhotoDescription{Caption: "A harbor.", Tags: []string{"coast"}, VisibleText: "QUAYSIDE"})
	require.NoError(t, err)
	response, body := get(t, ts, "/api/v1/photos/assets/"+asset.ID+"/suggestions", nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Contains(t, body, "QUAYSIDE")
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/suggestions/"+id+"/response", nil, store.PhotoSuggestionResponse{Revision: 1, Response: "accepted", Caption: true})
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	require.Contains(t, body, `"code":"invalid_photo_suggestion_response"`)
	_, err = db.ExecContext(ctx, `UPDATE photo_suggestions SET response='dismissed', revision=2 WHERE suggestion_id=?`, id)
	require.NoError(t, err)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/suggestions/"+id+"/response", nil, store.PhotoSuggestionResponse{Targets: []store.PhotoAuthoredEditTarget{{AssetID: asset.ID, AssetRevision: asset.Revision, FileID: *asset.DisplayFileID, Revision: asset.Files[0].Revision, NodeRevision: node.Revision}}, Revision: 2, Response: "accepted", Caption: true})
	require.Equal(t, http.StatusConflict, response.StatusCode, body)
	_, err = db.ExecContext(ctx, `UPDATE photo_suggestions SET response='pending', revision=1 WHERE suggestion_id=?`, id)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	_, cookie, err := daemonconn.New(ts.URL, testAPIKey).PhotoHidden(ctx, "unlock", "synthetic-passcode", "")
	require.NoError(t, err)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/suggestions/"+id+"/response", map[string]string{"Cookie": cookie}, store.PhotoSuggestionResponse{Targets: []store.PhotoAuthoredEditTarget{{AssetID: asset.ID, AssetRevision: asset.Revision + 1, FileID: *asset.DisplayFileID, Revision: asset.Files[0].Revision, NodeRevision: node.Revision}}, Revision: 1, Response: "accepted", Caption: true, Tags: []string{"coast"}})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var accepted map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &accepted))
	require.ElementsMatch(t, []string{"$schema", "suggestion_id", "content_version_id", "producer_fingerprint", "kind", "target", "value", "response", "revision", "receipt_id", "created_at", "authored_receipt_id", "authored_receipt"}, slices.Collect(maps.Keys(accepted)))
}

func TestPhotoSuggestionMatchSurvivesMapProjection(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	hash, size, err := s.Blobs.Write(strings.NewReader("synthetic JPEG"))
	require.NoError(t, err)
	node, err := s.CreateFile(ctx, s.RootID(), "synthetic.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	db, err := store.DefaultSQLiteDriver().Open(s.DBPath, sqlite.OpenOptions{Access: sqlite.ReadWriteExisting, TransactionMode: sqlite.Deferred})
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	_, err = phototest.SeedSuggestion(ctx, db, node.CurrentVersionID, document.PhotoDescription{Caption: "A harbor.", Tags: []string{}, VisibleText: "QUAYSIDE"})
	require.NoError(t, err)
	for _, projection := range []string{"", "map"} {
		response, body := do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, api.PhotoBrowseRequest{Query: api.QueryPayload(`{"text":"QUAYSIDE"}`), Projection: projection})
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		var page struct {
			Items []struct {
				AssetID             string `json:"asset_id"`
				MatchedAISuggestion bool   `json:"matched_ai_suggestion"`
			} `json:"items"`
			MapItems []struct {
				AssetID             string `json:"asset_id"`
				MatchedAISuggestion bool   `json:"matched_ai_suggestion"`
			} `json:"map_items"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &page))
		rows := page.Items
		if projection == "map" {
			rows = page.MapItems
		}
		require.Len(t, rows, 1)
		require.Equal(t, asset.ID, rows[0].AssetID)
		require.True(t, rows[0].MatchedAISuggestion, "projection %q must identify a suggestion-only match", projection)
	}
}
