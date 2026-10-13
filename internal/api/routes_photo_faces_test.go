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
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoFaceRoutesAndDaemonClient(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	hash, size, err := s.Blobs.Write(strings.NewReader("synthetic jpeg"))
	require.NoError(t, err)
	node, err := s.CreateFile(ctx, s.RootID(), "portrait.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	c := daemonconn.New(ts.URL, testAPIKey)
	box := document.FaceBox{X: 0.1, Y: 0.2, Width: 0.3, Height: 0.4}
	created, err := c.CreatePhotoFace(ctx, node.CurrentVersionID, api.CreatePhotoFaceRequest{FaceID: uuid.New().String(), Box: &box, PersonID: person.PersonID})
	require.NoError(t, err)
	require.NotNil(t, created.Face)
	raw, err := json.Marshal(created)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "receipt_id")
	page, err := c.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, []api.PhotoFace{*created.Face}, page.Faces)
	require.Equal(t, []api.PhotoPersonAssertion{*created.Assertion}, page.Assertions)
	require.Equal(t, person.PersonID, created.Assertion.PersonID)
	request := api.CreatePhotoFaceRequest{FaceID: created.Face.FaceID, Box: &box, PersonID: person.PersonID}
	replayed, err := c.CreatePhotoFace(ctx, node.CurrentVersionID, request)
	require.NoError(t, err)
	require.Equal(t, created, replayed)
	request.PersonID = ""
	replayed, err = c.CreatePhotoFace(ctx, node.CurrentVersionID, request)
	require.NoError(t, err)
	require.Equal(t, created, replayed)
	conflictBox := box
	conflictBox.X = 0.5
	request.Box = &conflictBox
	_, err = c.CreatePhotoFace(ctx, node.CurrentVersionID, request)
	require.ErrorIs(t, err, store.ErrExists)
	marked, err := c.CreatePhotoFace(ctx, node.CurrentVersionID, api.CreatePhotoFaceRequest{PersonID: person.PersonID})
	require.NoError(t, err)
	require.Nil(t, marked.Face)
	require.Equal(t, created.Assertion, marked.Assertion)
	other, err := s.CreatePerson(ctx, "Grace", "operator")
	require.NoError(t, err)
	whole, err := c.CreatePhotoFace(ctx, node.CurrentVersionID, api.CreatePhotoFaceRequest{PersonID: other.PersonID})
	require.NoError(t, err)
	require.Nil(t, whole.Face)
	require.Equal(t, other.PersonID, whole.Assertion.PersonID)
	page, err = c.PhotoFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, page.Faces, 1)
	require.ElementsMatch(t, []api.PhotoPersonAssertion{*created.Assertion, *whole.Assertion}, page.Assertions)
	path := "/api/v1/photos/versions/" + node.CurrentVersionID + "/faces"
	response, body := do(t, ts, http.MethodPost, path, nil, map[string]any{})
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, path, nil, map[string]any{"box": map[string]float64{"x": 0.9, "y": 0, "width": 0.2, "height": 0.1}})
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, path, nil, map[string]any{"box": box})
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	response, body = get(t, ts, path, nil)
	require.Equal(t, http.StatusForbidden, response.StatusCode, body)
	require.NotContains(t, body, created.Face.FaceID)
	require.Equal(t, "no-store", response.Header.Get("Cache-Control"))
	_, cookie, err := c.PhotoHidden(ctx, "unlock", "synthetic-passcode", "")
	require.NoError(t, err)
	page, err = c.PhotoFaces(ctx, node.CurrentVersionID, cookie)
	require.NoError(t, err)
	require.Len(t, page.Faces, 1)
	response, body = get(t, ts, path, map[string]string{"Cookie": cookie})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var restored api.PhotoFacePage
	require.NoError(t, json.Unmarshal([]byte(body), &restored))
	require.Len(t, restored.Faces, 1)
	_, _, err = c.PhotoHidden(ctx, "lock", "", cookie)
	require.NoError(t, err)
	_, err = c.PhotoFaces(ctx, node.CurrentVersionID, cookie)
	require.Error(t, err)
}
