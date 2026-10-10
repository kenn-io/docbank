package api_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoRejectRoutes(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	node, err := s.CreateFile(t.Context(), s.RootID(), "reject.jpg", testHash("reject"), 10, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	_, err = s.EditPhotoAuthored(t.Context(), []store.PhotoAuthoredTarget{{FileID: asset.Files[0].ID, Revision: 1, Patch: store.PhotoAuthoredPatch{Flag: new("reject")}}})
	require.NoError(t, err)
	request := api.PhotoRejectsRequest{Query: api.QueryPayload(`{}`)}
	response, body := do(t, ts, http.MethodPost, "/api/v1/photos/rejects/preflight", nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var preview api.PhotoRejectsPreflight
	require.NoError(t, json.Unmarshal([]byte(body), &preview))
	require.Equal(t, 1, preview.Photos)
	require.Equal(t, 1, preview.Movable)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/rejects/trash", nil, request)
	require.Equal(t, http.StatusPreconditionFailed, response.StatusCode, body)
	request.Digest = preview.Digest
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/rejects/trash", nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	request.Query = api.QueryPayload(`{"v":2}`)
	response, _ = do(t, ts, http.MethodPost, "/api/v1/photos/rejects/preflight", nil, request)
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode)
	for _, invalid := range []api.PhotoRejectsRequest{
		{Query: api.QueryPayload(`{}`), Digest: "fake", Coverage: api.WorkspaceQueryCoverage{Configuration: "bad"}},
		{Query: api.QueryPayload(`{"syntax":"advanced","text":"camera:("}`), Digest: "fake"},
	} {
		response, body = do(t, ts, http.MethodPost, "/api/v1/photos/rejects/trash", nil, invalid)
		require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	}
}
