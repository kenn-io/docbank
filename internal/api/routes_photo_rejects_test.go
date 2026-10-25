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
	for _, scope := range []string{"Library", "album", "Hidden"} {
		t.Run(scope, func(t *testing.T) {
			ts, s := newTestServer(t, nil)
			root := s.RootID()
			node, err := s.CreateFile(t.Context(), root, "reject.jpg", testHash("reject"), 10, "image/jpeg")
			require.NoError(t, err)
			asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
			require.NoError(t, err)
			_, err = s.EditPhotoAuthoredBatch(t.Context(), store.PhotoAuthoredBatch{Targets: []store.PhotoAuthoredEditTarget{{AssetID: asset.ID, AssetRevision: asset.Revision, FileID: asset.Files[0].ID, Revision: asset.Files[0].Revision, NodeRevision: 1}}, Patch: store.PhotoAuthoredPatch{Flag: new("reject")}})
			require.NoError(t, err)

			request := api.PhotoRejectsRequest{Query: api.QueryPayload(`{}`)}
			headers := map[string]string{}
			switch scope {
			case "album":
				outside, err := s.CreateFile(t.Context(), root, "retained.jpg", testHash("retained"), 1, "image/jpeg")
				require.NoError(t, err)
				other, err := s.PhotoAssetForNode(t.Context(), outside.ID)
				require.NoError(t, err)
				_, err = s.EditPhotoAuthoredBatch(t.Context(), store.PhotoAuthoredBatch{Targets: []store.PhotoAuthoredEditTarget{{AssetID: other.ID, AssetRevision: other.Revision, FileID: other.Files[0].ID, Revision: other.Files[0].Revision, NodeRevision: 1}}, Patch: store.PhotoAuthoredPatch{Flag: new("reject")}})
				require.NoError(t, err)
				album, err := s.CreatePhotoSet(t.Context(), "Holiday")
				require.NoError(t, err)
				_, err = s.ChangePhotoSetMembers(t.Context(), album.ID, album.Revision, true, store.PhotoSetSelection{AssetIDs: []string{asset.ID}})
				require.NoError(t, err)
				request.Query = api.QueryPayload(`{"filters":{"set_ids":["` + album.ID + `"]},"sort":{"field":"added_time","direction":"desc"}}`)
			case "Hidden":
				require.NoError(t, s.SetupPhotoHidden(t.Context(), "synthetic-passcode"))
				_, err = s.SetPhotoAssetHidden(t.Context(), asset.ID, asset.Revision, true)
				require.NoError(t, err)
				response, body := do(t, ts, http.MethodPost, "/api/v1/photos/hidden/unlock", nil, map[string]string{"passcode": "synthetic-passcode"})
				require.Equal(t, http.StatusOK, response.StatusCode, body)
				cookies := response.Cookies()
				require.Len(t, cookies, 1)
				headers["Cookie"] = cookies[0].Name + "=" + cookies[0].Value
				request.Hidden = true
			}
			response, body := do(t, ts, http.MethodPost, "/api/v1/photos/rejects/preflight", headers, request)
			require.Equal(t, http.StatusOK, response.StatusCode, body)
			var preview api.PhotoRejectsPreflight
			require.NoError(t, json.Unmarshal([]byte(body), &preview))
			require.Len(t, preview.Targets, 1)
			if scope == "album" {
				require.Equal(t, asset.ID, preview.Targets[0].AssetID)
				return
			}
			response, body = do(t, ts, http.MethodPost, "/api/v1/photos/rejects/trash", headers,
				api.MovePhotoRejectsRequest{Hidden: request.Hidden, Targets: preview.Targets})
			require.Equal(t, http.StatusOK, response.StatusCode, body)
			var moved api.PhotoRejectsMoved
			require.NoError(t, json.Unmarshal([]byte(body), &moved))
			require.Equal(t, []string{asset.ID}, moved.Moved)
		})
	}
}

func TestPhotoRejectRouteValidation(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	for _, test := range []struct {
		name string
		body any
	}{
		{"missing targets", map[string]any{}},
		{"empty targets", api.MovePhotoRejectsRequest{Targets: []store.PhotoRejectTarget{}}},
		{"invalid UUID", api.MovePhotoRejectsRequest{Targets: []store.PhotoRejectTarget{{AssetID: "bad", Revision: 1, MemberRevision: 1}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			response, body := do(t, ts, http.MethodPost, "/api/v1/photos/rejects/trash", nil, test.body)
			require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
			require.Contains(t, body, `"code":"validation"`)
		})
	}
}
