package docbank_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	docbank "go.kenn.io/docbank/docbank"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/mistral"
	"go.kenn.io/docbank/document/mistral/mistraltest"
	"go.kenn.io/docbank/internal/config"
)

func TestEmbeddedMistralProcessingRetainsSearchableRendition(t *testing.T) {
	configured := config.RenditionProfileConfig{MaxDocumentBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxUnits: 100,
		Runtime: &config.RenditionRuntimeConfig{Endpoint: "https://api.eu.mistral.ai"}}
	policy, err := configured.MistralPolicy()
	require.NoError(t, err)
	manifest, err := mistraltest.SyntheticManifest(policy, true)
	require.NoError(t, err)
	descriptor, err := mistral.DocumentDescriptor(policy, manifest)
	require.NoError(t, err)
	source := mistraltest.MinimalPDF("synthetic embedded OCR")
	transport := &embeddedMistralTransport{t: t}
	provider, err := mistral.NewRenditionProvider(mistral.Profile{
		Policy: policy, CapabilityManifest: manifest, Descriptor: descriptor,
		SecretBinding: "mistral", Timeout: time.Second,
	}, transport, &http.Client{Transport: transport})
	require.NoError(t, err)
	profile := embeddedProcessingProfile(t, descriptor)
	profile.Rendition.Name = "mistral"
	profile.Rendition.DisclosureFingerprint = mistral.DisclosureFingerprint(descriptor, "https://api.eu.mistral.ai", profile.Rendition.DeploymentFingerprint)
	profile.Rendition.AdapterContract = mistral.DocumentAdapterContract
	profile.Rendition.CredentialBinding = "credential:mistral"
	profile.Rendition.MaxUnits = 100
	profile.Rendition.RequestedArtifacts = []document.EvidenceArtifactRole{document.EvidenceArtifactMarkdown}
	profile.RetentionDisclosure.RetainProviderMarkdown = true
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(),
		Processing: docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{
			"mistral": {Profile: profile, RenditionProvider: provider,
				RenditionDisclosure: docbank.ProcessingRuntimeDisclosure{Endpoint: "https://api.eu.mistral.ai", Model: mistral.DefaultModel}},
		}}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	receipt, err := vault.Put(t.Context(), "/report.pdf", bytes.NewReader(source), docbank.PutOptions{MediaType: "application/pdf"})
	require.NoError(t, err)
	selector := docbank.ProcessingSelector{NodeID: receipt.Node.ID, ContentVersionID: receipt.Version.ID, Profile: "mistral"}
	request := docbank.ProcessingPlanRequest{Selector: selector}
	plan, err := vault.PlanProcessing(t.Context(), request)
	require.NoError(t, err)
	job, err := vault.StartProcessing(t.Context(), docbank.StartProcessingRequest{PlanRequest: request, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.NoError(t, err)
	status, err := vault.ProcessingStatus(t.Context(), docbank.ProcessingStatusRequest{JobID: job.ID})
	require.NoError(t, err)
	require.Equal(t, "completed", status.State, "%+v", status)
	rendition, err := vault.Rendition(t.Context(), docbank.RenditionRequest{Selector: selector})
	require.NoError(t, err)
	body, err := io.ReadAll(rendition.Reader)
	require.NoError(t, err)
	require.NoError(t, rendition.Reader.Close())
	require.Contains(t, string(body), "Synthetic needle report")
	report, err := vault.SearchDocuments(t.Context(), docbank.DocumentSearchRequest{
		Query: "needle", Mode: docbank.DocumentSearchLexical, Profile: "mistral",
		Fence: docbank.DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: []string{receipt.Version.ID}},
	})
	require.NoError(t, err)
	require.Len(t, report.Results, 1)
	require.Equal(t, receipt.Version.ID, report.Results[0].ContentVersionID)
}

type embeddedMistralTransport struct {
	t *testing.T
}

func (transport *embeddedMistralTransport) ResolveSecret(_ context.Context, binding string) (string, error) {
	assert.Equal(transport.t, "mistral", binding)
	return "synthetic-mistral-key", nil
}

func (transport *embeddedMistralTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	var wire struct {
		ExtractHeader bool `json:"extract_header"`
		ExtractFooter bool `json:"extract_footer"`
	}
	if err := json.UnmarshalRead(request.Body, &wire); err != nil {
		return nil, err
	}
	assert.False(transport.t, wire.ExtractHeader)
	assert.False(transport.t, wire.ExtractFooter)
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"model":"mistral-ocr-4-1","pages":[{"index":0,"markdown":"# Synthetic needle report"}],"usage_info":{"pages_processed":1}}`)), Request: request}, nil
}
