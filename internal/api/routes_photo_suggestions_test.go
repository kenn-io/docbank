package api_test

import (
	"bytes"
	"image/color"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/vision"
	"go.kenn.io/docbank/document/vision/visiontest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoSuggestionRoutesPassHiddenCookie(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	ts, s := newTestServer(t, nil)
	hash, size, err := s.Blobs.Write(bytes.NewReader(mediatest.JPEG(8, 8, color.White)))
	require.NoError(t, err)
	node, err := s.CreateFile(ctx, s.RootID(), "synthetic.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		reply := `{"caption":"A harbor.","tags":["coast"],"visible_text":"QUAYSIDE"}`
		if visiontest.IsProbe(body) {
			reply = "red,blue"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(visiontest.Completion(reply))
	}))
	t.Cleanup(endpoint.Close)
	provider, err := vision.New(vision.Profile{Origin: endpoint.URL, Model: "synthetic-vision", TrustBoundary: document.RenditionTrustOperatorNetwork, RequestTimeout: time.Minute, MaxResponseBytes: 1 << 20}, nil, endpoint.Client())
	require.NoError(t, err)
	profile := processingTestProfile(provider.Descriptor())
	profile.Rendition.AdapterContract = vision.AdapterContract
	profile.Rendition.DiscloseFilename = false
	profile.RetentionDisclosure.RetainSanitizedMarkdown = false
	service, err := processing.NewService(processing.ServiceConfig{Catalog: s.Store, Blobs: s.Blobs, Gate: api.NewOperationGate(), SpoolDirectory: t.TempDir(), Lifecycle: ctx,
		Profiles: map[string]processing.ProfileConfig{"vision": {Profile: profile, RenditionProvider: provider, RenditionDisclosure: processing.RuntimeDisclosure{Endpoint: endpoint.URL}}}})
	require.NoError(t, err)
	recipe, err := processing.VisualPreviewRecipeForSize("grid")
	require.NoError(t, err)
	_, err = processing.EnsureVisualPreview(ctx, s.Store, s.Blobs, node.CurrentVersionID, recipe)
	require.NoError(t, err)
	selector := processing.Selector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: "vision"}
	plan, err := service.Plan(ctx, selector)
	require.NoError(t, err)
	_, err = service.Start(ctx, processing.StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.NoError(t, err)
	prepared, err := s.PreparePhotoAuthored(ctx, store.PhotoSetSelection{AssetIDs: []string{asset.ID}})
	require.NoError(t, err)
	require.Len(t, prepared.Suggestions, 1)
	id := prepared.Suggestions[0].ID
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	hidden, err := s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	_, cookie, err := daemonconn.New(ts.URL, testAPIKey).PhotoHidden(ctx, "unlock", "synthetic-passcode", "")
	require.NoError(t, err)
	request := store.PhotoSuggestionResponse{AssetID: prepared.Suggestions[0].AssetID, AssetRevision: hidden.Revision, SourceRevision: prepared.Suggestions[0].SourceRevision, NodeRevision: prepared.Suggestions[0].NodeRevision, Response: "accepted", Caption: true, Tags: []string{"coast"}}
	response, body := do(t, ts, http.MethodPost, "/api/v1/photos/suggestions/"+id+"/response", nil, request)
	require.Equal(t, http.StatusForbidden, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/suggestions/"+id+"/response", map[string]string{"Cookie": cookie}, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
}
