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
	// A caller may build consent from the plan's classes or grant the reviewed plan; both must authorize the work.
	for name, grant := range map[string]func(t *testing.T, vault *docbank.Vault, request docbank.ProcessingPlanRequest, plan docbank.ProcessingPlan, disclosure string){
		"from the plan's classes": func(t *testing.T, vault *docbank.Vault, _ docbank.ProcessingPlanRequest, plan docbank.ProcessingPlan, disclosure string) {
			_, err := vault.GrantProcessingConsent(t.Context(), document.ProcessingConsentRequest{Principal: "embedded:operator", Scope: "document-processing", ProfileFingerprint: plan.ProfileFingerprint, DisclosureFingerprint: disclosure, InputClasses: plan.DisclosedClasses, RetainedArtifactClasses: plan.RetainedClasses})
			require.NoError(t, err)
		},
		"from the reviewed plan": func(t *testing.T, vault *docbank.Vault, request docbank.ProcessingPlanRequest, plan docbank.ProcessingPlan, _ string) {
			_, err := vault.GrantProcessingPlanConsent(t.Context(), docbank.ProcessingConsentGrantRequest{PlanRequest: request, PlanFingerprint: plan.Fingerprint})
			require.NoError(t, err)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testEmbeddedPhotoDescription(t, grant)
		})
	}
}

func testEmbeddedPhotoDescription(t *testing.T, grant func(*testing.T, *docbank.Vault, docbank.ProcessingPlanRequest, docbank.ProcessingPlan, string)) {
	t.Helper()
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
	grant(t, vault, request, plan, profile.Rendition.DisclosureFingerprint)
	select {
	case <-described:
	case <-time.After(5 * time.Second):
		t.Fatal("backfill did not queue the eligible photo")
	}
}
