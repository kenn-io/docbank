package docbank_test

import (
	"bytes"
	"image/color"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/docbank"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/vision"
	"go.kenn.io/docbank/document/vision/visiontest"
)

func TestEmbeddedPhotoDescriptionBackfillQueuesEligiblePhoto(t *testing.T) {
	t.Parallel()
	described := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		content := "red,blue"
		if !visiontest.IsProbe(body) {
			select {
			case described <- struct{}{}:
			default:
			}
			content = `{"caption":"A white photo.","tags":[],"visible_text":""}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, err = w.Write(visiontest.Completion(content))
		assert.NoError(t, err)
	}))
	defer server.Close()
	provider, err := vision.New(vision.Profile{Origin: server.URL, Model: "synthetic-vision", TrustBoundary: document.RenditionTrustOperatorNetwork, RequestTimeout: time.Minute, MaxResponseBytes: 1 << 20}, nil, server.Client())
	require.NoError(t, err)
	profile := embeddedProcessingProfile(t, provider.Descriptor())
	profile.Rendition.AdapterContract = vision.AdapterContract
	profile.RetentionDisclosure.RetainSanitizedMarkdown = false
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(), Processing: docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{
		"vision": {Profile: profile, RenditionProvider: provider, RenditionDisclosure: docbank.ProcessingRuntimeDisclosure{Endpoint: server.URL}},
	}}})
	require.NoError(t, err)
	defer func() { require.NoError(t, vault.Close()) }()
	receipt, err := vault.Put(t.Context(), "/photo.jpg", bytes.NewReader(mediatest.JPEG(8, 8, color.White)), docbank.PutOptions{MediaType: "image/jpeg"})
	require.NoError(t, err)
	_, err = vault.EnsureVisualPreviewForSize(t.Context(), receipt.Version.ID, docbank.VisualPreviewGrid)
	require.NoError(t, err)
	request := docbank.ProcessingPlanRequest{Selector: docbank.ProcessingSelector{NodeID: receipt.Node.ID, ContentVersionID: receipt.Version.ID, Profile: "vision"}}
	plan, err := vault.PlanProcessing(t.Context(), request)
	require.NoError(t, err)
	grant := document.ProcessingConsentRequest{Principal: "embedded:operator", Scope: "document-processing", ProfileFingerprint: plan.ProfileFingerprint, DisclosureFingerprint: profile.Rendition.DisclosureFingerprint, InputClasses: plan.DisclosedClasses, RetainedArtifactClasses: plan.RetainedClasses}
	_, err = vault.GrantProcessingConsent(t.Context(), grant)
	require.NoError(t, err)
	select {
	case <-described:
	case <-time.After(5 * time.Second):
		t.Fatal("backfill did not queue the eligible photo")
	}
}
