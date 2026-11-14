package api_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"image/color"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/localface"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/embedclient"
)

func seedFaceCandidate(t *testing.T, s *store.Store, node store.Node) {
	t.Helper()
	first, _, err := s.CreatePhotoFace(t.Context(), node.CurrentVersionID, uuid.New().String(), document.FaceBox{Width: .2, Height: .2}, "")
	require.NoError(t, err)
	second, _, err := s.CreatePhotoFace(t.Context(), node.CurrentVersionID, uuid.New().String(), document.FaceBox{X: .5, Width: .2, Height: .2}, "")
	require.NoError(t, err)
	candidate, err := store.NewFaceCandidate([]store.FaceSuggestionFace{{PhotoFace: first}, {PhotoFace: second}}, "", "")
	require.NoError(t, err)
	snapshot, err := s.FaceMatchingInputs(t.Context())
	require.NoError(t, err)
	require.NoError(t, s.ReplaceFaceSuggestions(t.Context(), snapshot, []store.PersonMatchCandidate{candidate}))
}

func TestFaceCandidateBrowserScopeAndDecision(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	hash, size, err := s.Blobs.WriteContext(ctx, strings.NewReader("synthetic photo"))
	require.NoError(t, err)
	node, err := s.CreateFile(ctx, s.RootID(), "portrait.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	seedFaceCandidate(t, s.Store, node)
	browser := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts)}
	response, body := get(t, ts, "/api/v1/people/face-candidates?limit=24", browser)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var page api.FaceCandidatePage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Len(t, page.Items, 1)
	require.Len(t, page.Items[0].Faces, 2)
	id := page.Items[0].CandidateID
	path := "/api/v1/people/face-candidates/" + id + "/decision"
	browser["If-Match"] = `"1"`
	oversize := make([]string, 49)
	for i := range oversize {
		oversize[i] = uuid.New().String()
	}
	response, body = do(t, ts, http.MethodPost, path, browser, api.FaceCandidateDecisionRequest{Action: "new_person", DisplayName: "Ada", FaceIDs: oversize})
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	require.Contains(t, body, "expected array length <= 48")
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	browser["If-Match"] = `"1"`
	response, body = do(t, ts, http.MethodPost, path, browser, api.FaceCandidateDecisionRequest{Action: "new_person", DisplayName: "Ada"})
	require.Equal(t, http.StatusForbidden, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/hidden/unlock", browser, map[string]string{"passcode": "synthetic-passcode"})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	browser["Cookie"] = response.Cookies()[0].String()
	response, body = get(t, ts, "/api/v1/people/face-candidates", browser)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Len(t, page.Items, 1)
	require.Equal(t, "no-store", response.Header.Get("Cache-Control"))
	require.Equal(t, "X-Api-Key, Authorization, Cookie, "+api.WebSessionHeader, response.Header.Get("Vary"))
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

func TestFaceCandidateLinkHonorsHiddenPeopleScope(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	hash, size, err := s.Blobs.WriteContext(ctx, strings.NewReader("synthetic photo"))
	require.NoError(t, err)
	visible, err := s.CreateFile(ctx, s.RootID(), "visible.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	seedFaceCandidate(t, s.Store, visible)
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
	response, body := get(t, ts, "/api/v1/people/face-candidates", browser)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var page api.FaceCandidatePage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Len(t, page.Items, 1)
	browser["If-Match"] = `"1"`
	path := "/api/v1/people/face-candidates/" + page.Items[0].CandidateID + "/decision"
	request := api.FaceCandidateDecisionRequest{Action: "link", PersonID: person.PersonID}
	response, body = do(t, ts, http.MethodPost, path, browser, request)
	require.Equal(t, http.StatusNotFound, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, path, map[string]string{"If-Match": `"1"`}, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
}

func TestFaceCandidateBrowserWithholdsUnreadableReferenceMatch(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	inference := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request embedclient.FaceRequest
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &request)) {
			return
		}
		reply := embedclient.FaceResponse{Model: embedclient.FaceModel, Revision: embedclient.FaceRevision, Recipe: embedclient.FaceRecipe}
		for i := range request.Boxes {
			values := make([]float32, 512)
			values[0] = 1
			reply.Data = append(reply.Data, embedclient.FaceResult{Index: i, Embedding: values})
		}
		assert.NoError(t, json.MarshalWrite(w, reply))
	}))
	inference.Start()
	profile, descriptor, err := config.LocalFaces()
	require.NoError(t, err)
	provider, err := localface.New(descriptor, func(context.Context) (string, error) { return inference.URL, nil })
	require.NoError(t, err)
	var service *processing.Service
	ts, s := newTestServer(t, func(deps *api.Deps) {
		service, err = processing.NewService(ctx, processing.ServiceConfig{Catalog: deps.Store, Blobs: deps.Blobs, Gate: api.NewOperationGate(), Lifecycle: ctx, SpoolDirectory: t.TempDir(), Profiles: map[string]processing.ProfileConfig{config.LocalFacesProfile: {Profile: profile, EmbeddingProviders: map[string]document.EmbeddingProvider{config.LocalFacesProfile: provider}}}})
		require.NoError(t, err)
		t.Cleanup(service.Stop)
	})
	person, err := s.CreatePerson(ctx, "Ada", "operator")
	require.NoError(t, err)
	var nodes []store.Node
	for i, name := range []string{"visible-reference.jpg", "hidden-reference.jpg", "target.jpg", "unnamed.jpg"} {
		hash, size, err := s.Blobs.WriteContext(ctx, bytes.NewReader(mediatest.JPEG(24, 24, color.Gray{Y: uint8(i * 50)})))
		require.NoError(t, err)
		node, err := s.CreateFile(ctx, s.RootID(), name, hash, size, "image/jpeg")
		require.NoError(t, err)
		recipe, err := processing.VisualPreviewRecipeForSize("grid")
		require.NoError(t, err)
		_, err = processing.EnsureVisualPreview(ctx, s.Store, s.Blobs, node.CurrentVersionID, recipe)
		require.NoError(t, err)
		nodes = append(nodes, node)
	}
	_, err = s.MarkPersonInPhoto(ctx, nodes[0].CurrentVersionID, person.PersonID, "")
	require.NoError(t, err)
	_, _, err = s.CreatePhotoFace(ctx, nodes[1].CurrentVersionID, uuid.New().String(), document.FaceBox{Width: .2, Height: .2}, person.PersonID)
	require.NoError(t, err)
	face, _, err := s.CreatePhotoFace(ctx, nodes[2].CurrentVersionID, uuid.New().String(), document.FaceBox{Width: .2, Height: .2}, "")
	require.NoError(t, err)
	setup := processing.FaceSetup{Service: service, Startup: func(context.Context) error { return nil }}
	plan, err := setup.Plan(ctx)
	require.NoError(t, err)
	_, err = setup.Apply(ctx, plan.Fingerprint)
	require.NoError(t, err)
	worker, err := processing.NewEmbeddingWorker(processing.EmbeddingWorkerConfig{Catalog: s.Store, Authority: s.Store, Blobs: s.Blobs, GenerationBlobs: s.Blobs, Runtime: service.EmbeddingRuntimes(), Gate: api.NewOperationGate(), Owner: "browser-face-test", RetryLimit: 1, RetryBaseDelay: time.Millisecond, MaxRetryDelay: time.Second, AttemptLifetime: time.Minute, MaxRows: 100, MaxDimensions: 512, MaxVectorBlobBytes: 1 << 20, LeaseDuration: time.Minute, IdleDelay: time.Millisecond, DescriptorFingerprints: []string{descriptor.Fingerprint}})
	require.NoError(t, err)
	_, err = worker.ScanOnce(ctx)
	require.NoError(t, err)
	candidate, err := store.NewFaceCandidate([]store.FaceSuggestionFace{{PhotoFace: face}}, person.PersonID, person.DisplayName)
	require.NoError(t, err)
	snapshot, err := s.FaceMatchingInputs(ctx)
	require.NoError(t, err)
	require.NoError(t, s.ReplaceFaceSuggestions(ctx, snapshot, []store.PersonMatchCandidate{candidate}))
	seedFaceCandidate(t, s.Store, nodes[3])
	retained, _, err := s.FaceCandidates(ctx, s.Blobs, 24, 0)
	require.NoError(t, err)
	var candidateID string
	for _, c := range retained {
		if c.SuggestedPersonID == person.PersonID {
			candidateID = c.CandidateID
		}
	}
	require.NotEmpty(t, candidateID)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	asset, err := s.PhotoAssetForNode(ctx, nodes[1].ID)
	require.NoError(t, err)
	asset, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	browser := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts)}
	response, body := get(t, ts, "/api/v1/people/face-candidates", browser)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var page api.FaceCandidatePage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.EqualValues(t, 1, page.Total)
	require.Len(t, page.Items, 1)
	require.Empty(t, page.Items[0].SuggestedPersonID)
	browser["If-Match"] = `"1"`
	response, body = do(t, ts, http.MethodPost, "/api/v1/people/face-candidates/"+candidateID+"/decision", browser, api.FaceCandidateDecisionRequest{Action: "reject"})
	require.Equal(t, http.StatusNotFound, response.StatusCode, body)
	// Keep Hidden locked and preserve a Hidden photo while restoring the reference.
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	_, err = s.SetPhotoAssetHidden(store.WithPhotoHiddenToken(ctx, token), asset.ID, asset.Revision, false)
	require.NoError(t, err)
	spare, err := s.PhotoAssetForNode(ctx, nodes[0].ID)
	require.NoError(t, err)
	_, err = s.SetPhotoAssetHidden(ctx, spare.ID, spare.Revision, true)
	require.NoError(t, err)
	response, body = get(t, ts, "/api/v1/people/face-candidates", browser)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.EqualValues(t, 2, page.Total, "the browser route must pass the reader for current visible matches")
	response, body = do(t, ts, http.MethodPost, "/api/v1/people/face-candidates/"+candidateID+"/decision", browser, api.FaceCandidateDecisionRequest{Action: "link", PersonID: person.PersonID})
	require.Equal(t, http.StatusOK, response.StatusCode, body, "the decision route must pass the reader too")
}
