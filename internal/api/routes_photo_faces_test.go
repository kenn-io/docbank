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
	previewPath := "/api/v1/photos/people/" + person.PersonID + "/face-previews"

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
	browserHeaders := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts)}
	response, body = get(t, ts, previewPath, browserHeaders)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var previews store.PersonFacePreviewPage
	require.NoError(t, json.Unmarshal([]byte(body), &previews))
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	require.Len(t, previews.Items, 1)

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
	response, body = get(t, ts, previewPath+"?hidden=true", nil)
	require.Equal(t, http.StatusForbidden, response.StatusCode, body)
	response, body = get(t, ts, path, headers)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var unlocked api.PhotoFacePage
	require.NoError(t, json.Unmarshal([]byte(body), &unlocked))
	require.Equal(t, []api.PhotoFace{*created.Face}, unlocked.Faces)
	require.Equal(t, []api.PhotoPersonAssertion{*created.Assertion}, unlocked.Assertions)
	response, body = get(t, ts, previewPath+"?hidden=true", headers)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &previews))
	require.Equal(t, []store.PersonFacePreview{{AssetID: asset.ID, ContentVersionID: node.CurrentVersionID, Box: &box}}, previews.Items)
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
	headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts)}
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)

	faceID := uuid.NewString()
	_, _, err = s.CreatePhotoFace(ctx, node.CurrentVersionID, faceID, document.FaceBox{X: .98, Y: .98, Width: .01, Height: .01}, "")
	require.NoError(t, err)
	decisions := "/api/v1/photos/versions/" + node.CurrentVersionID + "/face-decisions"
	response, body := do(t, ts, http.MethodPost, decisions, headers, api.PhotoFaceDecisionRequest{Action: "name", FaceID: faceID, PersonName: "Grace", Revision: 1, OperationID: uuid.NewString()})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	_, err = s.DecidePhotoFace(ctx, node.CurrentVersionID, store.PhotoFaceDecisionRequest{Action: "reject", FaceID: faceID, PersonID: person.PersonID, Revision: 2})
	require.NoError(t, err)
	for _, action := range []string{"reject", "key", "withdraw_rejection"} {
		response, body = do(t, ts, http.MethodPost, decisions, headers, api.PhotoFaceDecisionRequest{Action: action, FaceID: faceID, PersonID: person.PersonID, Revision: 3, OperationID: uuid.NewString(), PersonRevision: 1})
		require.Equal(t, http.StatusForbidden, response.StatusCode, body)
	}
	response, body = do(t, ts, http.MethodPost, decisions, headers, api.PhotoFaceDecisionRequest{Action: "retract", FaceID: faceID, Revision: 3})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, decisions, headers, api.PhotoFaceDecisionRequest{Action: "dismiss", FaceID: faceID, Revision: 4})
	require.Equal(t, http.StatusOK, response.StatusCode, body)

	marks := "/api/v1/photos/versions/" + node.CurrentVersionID + "/faces"
	response, body = do(t, ts, http.MethodPost, marks, headers, api.CreatePhotoFaceRequest{PersonName: new("Grace"), FaceID: uuid.NewString(), Box: &document.FaceBox{Width: .2, Height: .2}})
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	require.Contains(t, body, "invalid_photo_face")
	response, body = do(t, ts, http.MethodPost, marks, headers, api.CreatePhotoFaceRequest{PersonID: uuid.NewString(), PersonName: new("Grace")})
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
	var namedMark api.PhotoFaceResult
	require.NoError(t, json.Unmarshal([]byte(body), &namedMark))
	require.NotNil(t, namedMark.Assertion)
	require.True(t, namedMark.Assertion.PhotoMark)
	response, body = do(t, ts, http.MethodPost, marks, headers, api.CreatePhotoFaceRequest{PersonID: person.PersonID})
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
	var mark api.PhotoFaceResult
	require.NoError(t, json.Unmarshal([]byte(body), &mark))
	require.Nil(t, mark.Face)
	require.NotNil(t, mark.Assertion)
	require.True(t, mark.Assertion.PhotoMark)
	require.Equal(t, person.PersonID, mark.Assertion.PersonID)
	response, body = do(t, ts, http.MethodPost, decisions, headers, api.PhotoFaceDecisionRequest{Action: "retract", AssertionID: mark.Assertion.AssertionID, Revision: mark.Assertion.Revision})
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
	_, err = s.MarkPersonInPhoto(ctx, node.CurrentVersionID, hidden.PersonID, "")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts)}
	response, body := get(t, ts, "/api/v1/people", headers)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var page api.PersonPage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Empty(t, page.Items, "browser sessions apply the Hidden scope")
	// Browser writes apply the same scope; API-key writes keep theirs.
	headers["If-Match"] = `"1"`
	response, body = do(t, ts, http.MethodPatch, "/api/v1/people/by-id/"+hidden.PersonID, headers, api.RenamePersonRequest{DisplayName: "Locked rename"})
	require.Equal(t, http.StatusNotFound, response.StatusCode, body)
	delete(headers, "If-Match")
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/hidden/unlock", headers, map[string]string{"passcode": "synthetic-passcode"})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Len(t, response.Cookies(), 1)
	headers["Cookie"] = response.Cookies()[0].String()
	response, body = get(t, ts, "/api/v1/people", headers)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Len(t, page.Items, 1, "the Hidden cookie reaches the store")
	require.Equal(t, hidden.PersonID, page.Items[0].PersonID)
	headers["If-Match"] = `"1"`
	response, body = do(t, ts, http.MethodPatch, "/api/v1/people/by-id/"+hidden.PersonID, headers, api.RenamePersonRequest{DisplayName: "Unlocked rename"})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
}

func TestPeoplePageProjectionAndCursorBinding(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	ada, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	zoe, err := s.CreatePerson(ctx, "Zoe", "operator")
	require.NoError(t, err)
	// The People page lists only people in photos; Zoe's is newer.
	for _, person := range []store.Person{ada, zoe} {
		hash, size, err := s.Blobs.Write(strings.NewReader("synthetic photo of " + person.DisplayName))
		require.NoError(t, err)
		node, err := s.CreateFile(ctx, s.RootID(), person.DisplayName+".jpg", hash, size, "image/jpeg")
		require.NoError(t, err)
		_, err = s.MarkPersonInPhoto(ctx, node.CurrentVersionID, person.PersonID, "")
		require.NoError(t, err)
	}
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts)}
	path := "/api/v1/people?limit=1&photo_summary=true&recent_first=true"
	response, body := get(t, ts, path, headers)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var first api.PersonPage
	require.NoError(t, json.Unmarshal([]byte(body), &first))
	require.Len(t, first.Items, 1)
	require.Equal(t, zoe.PersonID, first.Items[0].PersonID)
	require.NotNil(t, first.Items[0].PhotoSummary)
	response, body = get(t, ts, path+"&cursor="+first.NextCursor, headers)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var second api.PersonPage
	require.NoError(t, json.Unmarshal([]byte(body), &second))
	require.Len(t, second.Items, 1)
	require.Equal(t, ada.PersonID, second.Items[0].PersonID)
	for _, changed := range []struct {
		query  string
		unlock bool
	}{
		{query: "photo_summary=false&recent_first=true"},
		{query: "photo_summary=true&recent_first=false"},
		{query: "photo_summary=true&recent_first=true&query=Z"},
		{query: "photo_summary=true&recent_first=true", unlock: true},
	} {
		if changed.unlock {
			unlocked, body := do(t, ts, http.MethodPost, "/api/v1/photos/hidden/unlock", headers, map[string]string{"passcode": "synthetic-passcode"})
			require.Equal(t, http.StatusOK, unlocked.StatusCode, body)
			require.Len(t, unlocked.Cookies(), 1)
			headers["Cookie"] = unlocked.Cookies()[0].String()
		}
		response, body = get(t, ts, "/api/v1/people?limit=1&"+changed.query+"&cursor="+first.NextCursor, headers)
		require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
		require.Equal(t, "people_cursor_expired", decodeProblem(t, body).Code)
	}
	response, body = get(t, ts, "/api/v1/people", headers)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.NotContains(t, body, "photo_summary")
	require.NoError(t, json.Unmarshal([]byte(body), &second))
	require.Equal(t, ada.PersonID, second.Items[0].PersonID)
}

// Browser merge conflicts identify the exact face version to review.
func TestBrowserMergeConflictNamesFaceVersion(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	hash, size, err := s.Blobs.Write(strings.NewReader("synthetic merge portrait"))
	require.NoError(t, err)
	node, err := s.CreateFile(ctx, s.RootID(), "merge-portrait.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	ada, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	grace, err := s.CreatePerson(ctx, "Grace", "operator")
	require.NoError(t, err)
	face, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.NewString(), document.FaceBox{Width: .2, Height: .2}, ada.PersonID)
	require.NoError(t, err)
	_, err = s.DecidePhotoFace(ctx, node.CurrentVersionID, store.PhotoFaceDecisionRequest{Action: "reject", FaceID: face.FaceID, PersonID: grace.PersonID, Revision: face.Revision})
	require.NoError(t, err)
	headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts), "If-Match": `"1"`}
	merge := api.MergePersonRequest{AbsorbedPersonID: grace.PersonID, AbsorbedRevision: 1, OperationID: uuid.NewString()}
	response, body := do(t, ts, http.MethodPost, "/api/v1/people/by-id/"+ada.PersonID+"/merge", headers, merge)
	require.Equal(t, http.StatusConflict, response.StatusCode, body)
	problem := decodeProblem(t, body)
	require.Equal(t, []string{face.FaceID}, problem.FaceIDs)
	require.Equal(t, []string{node.CurrentVersionID}, problem.FaceVersionIDs)
}
