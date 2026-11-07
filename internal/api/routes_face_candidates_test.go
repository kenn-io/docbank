package api_test

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestFaceCandidateBrowserScopeAndDecision(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	hash, size, err := s.Blobs.WriteContext(ctx, strings.NewReader("synthetic photo"))
	require.NoError(t, err)
	node, err := s.CreateFile(ctx, s.RootID(), "portrait.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	f, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{Width: .2, Height: .2}, "")
	require.NoError(t, err)
	second, _, err := s.CreatePhotoFace(ctx, node.CurrentVersionID, uuid.New().String(), document.FaceBox{X: .5, Width: .2, Height: .2}, "")
	require.NoError(t, err)
	candidate, err := store.NewFaceCandidate([]store.FaceSuggestionFace{{PhotoFace: f}, {PhotoFace: second}}, "", "")
	require.NoError(t, err)
	require.NoError(t, s.ReplaceFaceSuggestions(ctx, faceMatchingSignature(t, s.Store), []store.PersonMatchCandidate{candidate}))
	browser := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts)}
	response, body := get(t, ts, "/api/v1/people/face-candidates?limit=24", browser)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var page api.FaceCandidatePage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Len(t, page.Items, 1)
	require.Len(t, page.Items[0].Faces, 2)
	id := page.Items[0].CandidateID
	path := "/api/v1/people/face-candidates/" + id + "/decision"
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	response, body = get(t, ts, "/api/v1/people/face-candidates?limit=24", browser)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Empty(t, page.Items)
	require.Zero(t, page.Total)
	browser["If-Match"] = `"1"`
	response, body = do(t, ts, http.MethodPost, path, browser, api.FaceCandidateDecisionRequest{Action: "new_person", DisplayName: "Ada"})
	require.Equal(t, http.StatusForbidden, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/hidden/unlock", browser, map[string]string{"passcode": "synthetic-passcode"})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	browser["Cookie"] = response.Cookies()[0].String()
	response, body = get(t, ts, "/api/v1/people/face-candidates?limit=24", browser)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Contains(t, body, f.FaceID)
	browser["If-Match"] = `"9"`
	response, body = do(t, ts, http.MethodPost, path, browser, api.FaceCandidateDecisionRequest{Action: "new_person", DisplayName: "Ada"})
	require.Equal(t, http.StatusPreconditionFailed, response.StatusCode, body)
	browser["If-Match"] = `"1"`
	response, body = do(t, ts, http.MethodPost, path, browser, api.FaceCandidateDecisionRequest{Action: "new_person", DisplayName: "Ada"})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, path, browser, api.FaceCandidateDecisionRequest{Action: "reject"})
	require.Equal(t, http.StatusConflict, response.StatusCode, body)
	response, body = get(t, ts, "/api/v1/people/face-candidates?limit=251", browser)
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
}

func faceMatchingSignature(t *testing.T, s *store.Store) string {
	t.Helper()
	snapshot, err := s.FaceMatchingInputs(t.Context())
	require.NoError(t, err)
	return snapshot.Signature
}

func TestFaceCandidateLinkFencesTargetAfterHiddenLocks(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	hash, size, err := s.Blobs.WriteContext(ctx, strings.NewReader("synthetic photo"))
	require.NoError(t, err)
	visible, err := s.CreateFile(ctx, s.RootID(), "visible.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	first, _, err := s.CreatePhotoFace(ctx, visible.CurrentVersionID, uuid.New().String(), document.FaceBox{Width: .2, Height: .2}, "")
	require.NoError(t, err)
	second, _, err := s.CreatePhotoFace(ctx, visible.CurrentVersionID, uuid.New().String(), document.FaceBox{X: .5, Width: .2, Height: .2}, "")
	require.NoError(t, err)
	candidate, err := store.NewFaceCandidate([]store.FaceSuggestionFace{{PhotoFace: first}, {PhotoFace: second}}, "", "")
	require.NoError(t, err)
	require.NoError(t, s.ReplaceFaceSuggestions(ctx, faceMatchingSignature(t, s.Store), []store.PersonMatchCandidate{candidate}))
	person, err := s.CreatePerson(ctx, "Concealed", "operator")
	require.NoError(t, err)
	hidden, err := s.CreateFile(ctx, s.RootID(), "hidden.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	_, err = s.MarkPersonInPhoto(ctx, hidden.CurrentVersionID, person.PersonID, "")
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	asset, err := s.PhotoAssetForNode(ctx, hidden.ID)
	require.NoError(t, err)
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	browser := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts)}
	response, body := do(t, ts, http.MethodPost, "/api/v1/photos/hidden/unlock", browser, map[string]string{"passcode": "synthetic-passcode"})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	browser["Cookie"] = response.Cookies()[0].String()
	response, body = get(t, ts, "/api/v1/people?query=Concealed&photo_summary=true", browser)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Contains(t, body, person.PersonID)
	response, body = get(t, ts, "/api/v1/people/face-candidates", browser)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var page api.FaceCandidatePage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Len(t, page.Items, 1)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/hidden/lock", browser, map[string]string{})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	browser["If-Match"] = `"1"`
	path := "/api/v1/people/face-candidates/" + page.Items[0].CandidateID + "/decision"
	request := api.FaceCandidateDecisionRequest{Action: "link", PersonID: person.PersonID}
	response, body = do(t, ts, http.MethodPost, path, browser, request)
	require.Equal(t, http.StatusNotFound, response.StatusCode, body)
	faces, _, _, err := s.PhotoFacePersonNames(ctx, visible.CurrentVersionID)
	require.NoError(t, err)
	for _, face := range faces {
		require.Empty(t, face.AssertionID)
	}
	response, body = do(t, ts, http.MethodPost, path, map[string]string{"If-Match": `"1"`}, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
}
