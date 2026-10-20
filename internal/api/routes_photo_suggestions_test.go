package api_test

import (
	"bytes"
	"encoding/json/v2"
	"image/color"
	"net/http"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/sqlite"
)

func TestPhotoSuggestionRoutesValidateResponseAndPassHiddenCookie(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	ts, s := newTestServer(t, nil)
	hash, size, err := s.Blobs.Write(bytes.NewReader(mediatest.JPEG(8, 8, color.White)))
	require.NoError(t, err)
	node, err := s.CreateFile(ctx, s.RootID(), "synthetic.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	db, err := s.SQLiteDriver().Open(s.DBPath, sqlite.OpenOptions{Access: sqlite.ReadWriteExisting, TransactionMode: sqlite.Deferred})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.ExecContext(ctx, `INSERT INTO photo_suggestions(suggestion_id,content_version_id,producer_fingerprint,caption,tags_json,visible_text,created_at) VALUES(?,?,?,?,?,?,'2026-01-01T00:00:00Z')`, uuid.New().String(), node.CurrentVersionID, strings.Repeat("a", 64), "A harbor.", `["coast"]`, "QUAYSIDE")
	require.NoError(t, err)
	prepared, err := s.PreparePhotoAuthored(ctx, store.PhotoSetSelection{AssetIDs: []string{asset.ID}})
	require.NoError(t, err)
	require.Len(t, prepared.Suggestions, 1)
	id := prepared.Suggestions[0].ID
	response, body := do(t, ts, http.MethodPost, "/api/v1/photos/suggestions/"+id+"/response", nil, store.PhotoSuggestionResponse{Response: "accepted"})
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	require.Contains(t, body, `"code":"invalid_photo_suggestion_response"`)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	_, cookie, err := daemonconn.New(ts.URL, testAPIKey).PhotoHidden(ctx, "unlock", "synthetic-passcode", "")
	require.NoError(t, err)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/suggestions/"+id+"/response", map[string]string{"Cookie": cookie}, store.PhotoSuggestionResponse{SourceRevision: prepared.Suggestions[0].SourceRevision, NodeRevision: prepared.Suggestions[0].NodeRevision, Response: "accepted", Caption: true, Tags: []string{"coast"}})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var accepted map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &accepted))
	require.Contains(t, accepted, "authored_receipt")
}
