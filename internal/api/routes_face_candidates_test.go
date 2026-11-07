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
