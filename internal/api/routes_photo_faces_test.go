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
	"go.kenn.io/docbank/internal/store"
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
	response, body = get(t, ts, path, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var listed api.PhotoFacePage
	require.NoError(t, json.Unmarshal([]byte(body), &listed))
	require.Len(t, listed.People, 1)
	var payload struct {
		Assertions []map[string]any `json:"assertions"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &payload))
	require.NotContains(t, payload.Assertions[0], "display_name")

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
	response, body = do(t, ts, http.MethodPost, path, nil, api.PhotoFaceDecisionRequest{AssertionID: uuid.NewString(), Action: "retract", Revision: 1})
	require.Equal(t, http.StatusNotFound, response.StatusCode, body)
	operationID := uuid.NewString()
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/face-decisions/"+receipt.ReceiptID+"/undo?operation_id="+operationID, nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var undone api.PhotoFaceDecisionResult
	require.NoError(t, json.Unmarshal([]byte(body), &undone))
	require.Equal(t, operationID, undone.ReceiptID)
}

func TestPersonAssertionSelectorPageRoute(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	p, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	hash, size, err := s.Blobs.Write(strings.NewReader("synthetic letter"))
	require.NoError(t, err)
	n, err := s.CreateFile(ctx, s.RootID(), "letter.txt", hash, size, "text/plain")
	require.NoError(t, err)
	for _, role := range []string{"author", "recipient"} {
		_, err = s.AssertDocumentPerson(ctx, store.PersonDocumentAssertion{ContentVersionID: n.CurrentVersionID, PersonID: p.PersonID, Role: role, Action: "assert", Revision: 1})
		require.NoError(t, err)
	}
	path := "/api/v1/people/by-id/" + p.PersonID + "?assertion_limit=1"
	response, body := get(t, ts, path, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var first api.PersonDetail
	require.NoError(t, json.Unmarshal([]byte(body), &first))
	require.Len(t, first.Assertions, 1)
	require.NotEmpty(t, first.AssertionsNextCursor)
	response, body = get(t, ts, path+"&assertion_cursor="+first.AssertionsNextCursor, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var second api.PersonDetail
	require.NoError(t, json.Unmarshal([]byte(body), &second))
	require.Len(t, second.Assertions, 1)
	require.NotEqual(t, first.Assertions[0].AssertionID, second.Assertions[0].AssertionID)
}

func TestPhotoFaceBrowserDecisions(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	hash, size, err := s.Blobs.Write(strings.NewReader("synthetic portrait"))
	require.NoError(t, err)
	node, err := s.CreateFile(ctx, s.RootID(), "synthetic-portrait.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	response, body := do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
	var issued struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &issued))
	headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issued.Token}
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)

	path := "/api/v1/photos/versions/" + node.CurrentVersionID + "/faces"
	response, body = do(t, ts, http.MethodPost, path, headers, api.CreatePhotoFaceRequest{PersonID: person.PersonID})
	require.Equal(t, http.StatusForbidden, response.StatusCode, body)
	require.Contains(t, body, "web_session_read_only")
	response, body = get(t, ts, path, headers)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var unchanged api.PhotoFacePage
	require.NoError(t, json.Unmarshal([]byte(body), &unchanged))
	require.Empty(t, unchanged.Assertions)
	faceID := uuid.NewString()
	response, body = do(t, ts, http.MethodPost, path, headers, api.CreatePhotoFaceRequest{FaceID: faceID, Box: &document.FaceBox{X: .98, Y: .98, Width: .01, Height: .01}})
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
	decisions := "/api/v1/photos/versions/" + node.CurrentVersionID + "/face-decisions"
	response, body = do(t, ts, http.MethodPost, decisions, headers, api.PhotoFaceDecisionRequest{Action: "name", FaceID: faceID, PersonName: "Grace", Revision: 1, OperationID: uuid.NewString()})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	for _, action := range []string{"reject", "key", "withdraw_rejection"} {
		response, body = do(t, ts, http.MethodPost, decisions, headers, api.PhotoFaceDecisionRequest{Action: action, FaceID: faceID, PersonID: person.PersonID, Revision: 2, OperationID: uuid.NewString()})
		require.Equal(t, http.StatusForbidden, response.StatusCode, body)
	}
	response, body = do(t, ts, http.MethodPost, decisions, headers, api.PhotoFaceDecisionRequest{Action: "retract", FaceID: faceID, Revision: 2})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, decisions, headers, api.PhotoFaceDecisionRequest{Action: "dismiss", FaceID: faceID, Revision: 3})
	require.Equal(t, http.StatusOK, response.StatusCode, body)

	mark, err := s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID)
	require.NoError(t, err)
	response, body = do(t, ts, http.MethodPost, decisions, headers, api.PhotoFaceDecisionRequest{Action: "retract", AssertionID: mark.AssertionID, Revision: mark.Revision})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
}

func TestPhotoFaceBrowserPeopleHiddenScope(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	hash, size, err := s.Blobs.Write(strings.NewReader("synthetic portrait"))
	require.NoError(t, err)
	node, err := s.CreateFile(ctx, s.RootID(), "hidden.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	hidden, err := s.CreatePerson(ctx, "Hidden Ada", "operator")
	require.NoError(t, err)
	_, err = s.MarkPersonInPhoto(ctx, node.CurrentVersionID, hidden.PersonID)
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	response, body := do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
	var issued struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &issued))
	headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issued.Token}
	response, body = get(t, ts, "/api/v1/people", headers)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var page api.PersonPage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Empty(t, page.Items, "browser sessions apply the Hidden scope")
	lockedHeaders := response.Header
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/hidden/unlock", headers, map[string]string{"passcode": "synthetic-passcode"})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Len(t, response.Cookies(), 1)
	headers["Cookie"] = response.Cookies()[0].String()
	response, body = get(t, ts, "/api/v1/people", headers)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Len(t, page.Items, 1, "the Hidden cookie reaches the store")
	require.Equal(t, hidden.PersonID, page.Items[0].PersonID)
	for _, h := range []http.Header{lockedHeaders, response.Header} {
		require.Equal(t, "no-store", h.Get("Cache-Control"))
		require.Equal(t, "X-Api-Key, Authorization, Cookie, "+api.WebSessionHeader, h.Get("Vary"))
	}
}
