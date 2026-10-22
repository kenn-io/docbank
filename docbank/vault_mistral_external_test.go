package docbank_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	docbank "go.kenn.io/docbank/docbank"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/mistral"
	"go.kenn.io/docbank/document/mistral/mistraltest"
)

func TestEmbeddedMistralProcessingRetainsSearchableRendition(t *testing.T) {
	normalize, err := document.NewNormalizePolicy(25_000_000)
	require.NoError(t, err)
	policy, err := mistral.NewPolicy(mistral.PolicyConfig{Region: mistral.RegionEU, Model: mistral.DefaultModel,
		Retention: mistral.RetentionStandard, Training: mistral.TrainingDefaultOptOut,
		MaxDocumentBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxUnits: 100,
		ExtractHeader: false, ExtractFooter: false, NormalizePolicy: normalize})
	require.NoError(t, err)
	manifest, err := mistraltest.SyntheticManifest(policy, true)
	require.NoError(t, err)
	descriptor, err := mistral.DocumentDescriptor(policy, manifest)
	require.NoError(t, err)
	source := mistraltest.MinimalPDF("synthetic embedded OCR")
	transport := &embeddedMistralTransport{t: t, source: source}
	provider, err := mistral.NewRenditionProvider(mistral.Profile{
		Policy: policy, CapabilityManifest: manifest, Descriptor: descriptor,
		SecretBinding: "mistral", Timeout: time.Second,
	}, transport, &http.Client{Transport: transport})
	require.NoError(t, err)
	profile := embeddedProcessingProfile(t, descriptor)
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
	require.True(t, plan.ConsentRequired)
	require.Zero(t, transport.calls.Load())
	job, err := vault.StartProcessing(t.Context(), docbank.StartProcessingRequest{PlanRequest: request, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.NoError(t, err)
	status, err := vault.ProcessingStatus(t.Context(), docbank.ProcessingStatusRequest{JobID: job.ID})
	require.NoError(t, err)
	require.Equal(t, "completed", status.State, "%+v", status)
	require.EqualValues(t, 1, transport.calls.Load())
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
	t      *testing.T
	source []byte
	calls  atomic.Int32
}

func (transport *embeddedMistralTransport) ResolveSecret(_ context.Context, binding string) (string, error) {
	assert.Equal(transport.t, "mistral", binding)
	return "synthetic-mistral-key", nil
}

func (transport *embeddedMistralTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.calls.Add(1)
	assert.Equal(transport.t, "https://api.eu.mistral.ai/v1/ocr", request.URL.String())
	assert.Equal(transport.t, http.MethodPost, request.Method)
	assert.Equal(transport.t, "Bearer synthetic-mistral-key", request.Header.Get("Authorization"))
	var wire struct {
		Model    string `json:"model"`
		Document struct {
			URL string `json:"document_url"`
		} `json:"document"`
		ExtractHeader bool `json:"extract_header"`
		ExtractFooter bool `json:"extract_footer"`
	}
	if err := json.UnmarshalRead(request.Body, &wire); err != nil {
		return nil, err
	}
	assert.Equal(transport.t, "mistral-ocr-4-1", wire.Model)
	assert.Equal(transport.t, "data:application/pdf;base64,"+base64.StdEncoding.EncodeToString(transport.source), wire.Document.URL)
	assert.False(transport.t, wire.ExtractHeader)
	assert.False(transport.t, wire.ExtractFooter)
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"model":"mistral-ocr-4-1","pages":[{"index":0,"markdown":"# Synthetic needle report"}],"usage_info":{"pages_processed":1}}`)), Request: request}, nil
}
