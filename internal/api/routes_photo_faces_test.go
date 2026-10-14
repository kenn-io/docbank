package api_test

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
)

func TestPhotoFaceRoutes(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	hash, size, err := s.Blobs.Write(strings.NewReader("synthetic jpeg"))
	require.NoError(t, err)
	node, err := s.CreateFile(ctx, s.RootID(), "portrait.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	box := document.FaceBox{X: 0.1, Y: 0.2, Width: 0.3, Height: 0.4}
	faceID := uuid.NewString()
	path := "/api/v1/photos/versions/" + node.CurrentVersionID + "/faces"

	response, body := do(t, ts, http.MethodPost, path, nil, api.CreatePhotoFaceRequest{FaceID: faceID, Box: &box, PersonID: person.PersonID})
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
	require.NotContains(t, body, "receipt_id")
	var created api.PhotoFaceResult
	require.NoError(t, json.Unmarshal([]byte(body), &created))
	require.NotNil(t, created.Face)
	require.NotNil(t, created.Assertion)
	require.Equal(t, faceID, created.Face.FaceID)
	require.Equal(t, person.PersonID, created.Assertion.PersonID)

	conflictBox := box
	conflictBox.X = 0.5
	response, body = do(t, ts, http.MethodPost, path, nil, api.CreatePhotoFaceRequest{FaceID: faceID, Box: &conflictBox, PersonID: person.PersonID})
	require.Equal(t, http.StatusConflict, response.StatusCode, body)
	require.Contains(t, body, "face ID is already in use with different face details")
	for _, invalid := range []any{
		map[string]any{},
		map[string]any{"face_id": uuid.NewString(), "box": map[string]float64{"x": 0.9, "y": 0, "width": 0.2, "height": 0.1}},
		map[string]any{"box": box},
	} {
		response, body = do(t, ts, http.MethodPost, path, nil, invalid)
		require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	}

	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	response, body = get(t, ts, path, nil)
	require.Equal(t, http.StatusForbidden, response.StatusCode, body)
	require.NotContains(t, body, faceID)
	require.Equal(t, "no-store", response.Header.Get("Cache-Control"))

	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/hidden/unlock", nil, map[string]string{"passcode": "synthetic-passcode"})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	cookies := response.Cookies()
	require.Len(t, cookies, 1)
	headers := map[string]string{"Cookie": cookies[0].Name + "=" + cookies[0].Value}
	response, body = get(t, ts, path, headers)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var unlocked api.PhotoFacePage
	require.NoError(t, json.Unmarshal([]byte(body), &unlocked))
	require.Equal(t, []api.PhotoFace{*created.Face}, unlocked.Faces)
	require.Equal(t, []api.PhotoPersonAssertion{*created.Assertion}, unlocked.Assertions)
}

func TestPhotoFaceDecisionRoutes(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	hash, size, err := s.Blobs.Write(strings.NewReader("synthetic portrait"))
	require.NoError(t, err)
	node, err := s.CreateFile(ctx, s.RootID(), "portrait.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	p, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	face, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.NewString(), document.FaceBox{Width: .2, Height: .2}, "")
	require.NoError(t, err)
	path := "/api/v1/photos/versions/" + node.CurrentVersionID + "/face-decisions"
	response, body := do(t, ts, http.MethodPost, path, nil, api.PhotoFaceDecisionRequest{FaceID: face.FaceID, PersonID: p.PersonID, Action: "name", Revision: 1})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var receipt api.PhotoFaceDecisionResult
	require.NoError(t, json.Unmarshal([]byte(body), &receipt))
	require.NotEmpty(t, receipt.ReceiptID)
	response, body = do(t, ts, http.MethodPost, path, nil, api.PhotoFaceDecisionRequest{FaceID: face.FaceID, Action: "dismiss", Revision: 1})
	require.Equal(t, http.StatusPreconditionFailed, response.StatusCode, body)
	require.Contains(t, body, "stale_revision")
	response, body = get(t, ts, "/api/v1/people/by-id/"+p.PersonID, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var detail api.PersonDetail
	require.NoError(t, json.Unmarshal([]byte(body), &detail))
	require.Len(t, detail.Assertions, 1)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/face-decisions/"+receipt.ReceiptID+"/undo", nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/face-decisions/"+receipt.ReceiptID+"/undo", nil, nil)
	require.Equal(t, http.StatusPreconditionFailed, response.StatusCode, body)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	response, body = do(t, ts, http.MethodPost, path, nil, api.PhotoFaceDecisionRequest{FaceID: face.FaceID, Action: "dismiss", Revision: 3})
	require.Equal(t, http.StatusForbidden, response.StatusCode, body)
	require.NotContains(t, body, face.FaceID)
}
