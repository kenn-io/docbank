package voyage_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	json "encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/go-pdf/fpdf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	docbank "go.kenn.io/docbank"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/docling"
	"go.kenn.io/docbank/document/voyage"
)

// Exercise real Docling rendition decoding, retained input preparation,
// Voyage document/query transport, vector publication and hybrid retrieval.
func TestVoyageHybridSearchRetainedDoclingMarkdown(t *testing.T) {
	const passage = "Solar panel maintenance requires an annual inspection."
	doclingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var response any
		switch r.URL.Path {
		case "/v1/convert/file/async":
			upload, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
			if !assert.NoError(t, err) || !assert.LessOrEqual(t, len(upload), 1<<20) {
				http.Error(w, "invalid synthetic upload", http.StatusBadRequest)
				return
			}
			assert.NotEmpty(t, upload)
			response = map[string]any{"task_id": "synthetic-task", "task_type": "convert", "task_status": "pending"}
		case "/v1/status/poll/synthetic-task":
			response = map[string]any{"task_id": "synthetic-task", "task_type": "convert", "task_status": "success"}
		case "/v1/result/synthetic-task":
			response = map[string]any{"status": "success", "processing_time": 0.01, "errors": []string{}, "document": map[string]any{
				"filename": "document.pdf", "md_content": "# Solar maintenance\n\n" + passage + "\n",
				"json_content": map[string]any{"schema_name": "DoclingDocument", "version": "1.7.0", "origin": map[string]any{"filename": "document.pdf"},
					"pages": map[string]any{"1": map[string]any{}}, "texts": []map[string]any{{"text": passage, "prov": []map[string]any{{"page_no": 1}}}}}, "html_content": "", "text_content": ""}}
		default:
			http.NotFound(w, r)
			return
		}
		assert.NoError(t, json.MarshalWrite(w, response))
	}))
	t.Cleanup(doclingServer.Close)
	renditionDescriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID: "docling.serve-v1", ContractVersion: document.RenditionProviderContractVersion, PolicyFingerprint: syntheticTextHash("docling-policy"),
		TrustBoundary: document.RenditionTrustOperatorNetwork, SupportedFormats: []document.RenditionFormatCapability{{MediaFamily: "pdf", MediaType: "application/pdf", InputKind: document.RenditionInputOriginalFile}},
		ReturnsMarkdown: true, ReturnsStructured: true, ArtifactRoles: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured}})
	require.NoError(t, err)
	renditionProvider, err := docling.New(docling.Profile{Origin: doclingServer.URL, Descriptor: renditionDescriptor}, nil, doclingServer.Client())
	require.NoError(t, err)
	var documentCalls, queryCalls atomic.Int32
	endpoint, egress, resolver := voyageFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request voyageTextTestRequest
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &request, json.RejectUnknownMembers(true))) {
			http.Error(w, "invalid synthetic request", http.StatusBadRequest)
			return
		}
		inputs := request.Input
		if request.InputType == "document" {
			documentCalls.Add(1)
			assert.Contains(t, inputs[0], "Solar")
		} else {
			queryCalls.Add(1)
			assert.Equal(t, "query", request.InputType)
		}
		data := make([]map[string]any, len(inputs))
		for i := range inputs {
			data[i] = map[string]any{"object": "embedding", "index": i, "embedding": unitEmbedding(1), "text": inputs[i]}
		}
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(w, map[string]any{"object": "list", "model": "voyage-4-large", "data": data}))
	}))
	embeddingProfile := voyageTextProfile(t, voyage.EmbeddingModeText)
	embeddingProfile.Endpoint, embeddingProfile.EgressPolicy = endpoint, egress
	embeddingProfile.MaxInputBytes = 1 << 20
	embeddingProfile.DeploymentEpoch = "synthetic-retained-v1"
	embeddingProfile.Descriptor.Model = "voyage-4-large"
	embeddingProfile.Descriptor.ModelRevision = embeddingProfile.DeploymentEpoch
	embeddingProfile.Descriptor.SupportsTextQuery = true
	embeddingProfile = refingerprintVoyageProfile(t, embeddingProfile)
	embeddingProvider, err := newVoyageEmbeddingTestProvider(t, embeddingProfile, embeddingSecrets{"credential:voyage": "synthetic-secret"}, resolver)
	require.NoError(t, err)
	descriptor := embeddingProvider.Descriptor()
	profile := document.ProcessingProfileV1{ContractVersion: document.ProcessingProfileContractV1,
		Rendition: &document.RenditionBindingV1{AdapterContract: "docbank-docling/v1", AuthorizationFingerprint: syntheticTextHash("rendition-auth"), CredentialBinding: "credential:none",
			DeploymentFingerprint: syntheticTextHash("rendition-deployment"), Descriptor: document.ProviderDescriptorV1{ID: renditionDescriptor.ID, Fingerprint: renditionDescriptor.Fingerprint},
			DisclosureFingerprint: syntheticTextHash("rendition-disclosure"), MaxDocumentBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxUnits: 1000, Name: "docling",
			RequestedArtifacts: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured}, TrustBoundary: string(renditionDescriptor.TrustBoundary), UploadOptionsFingerprint: syntheticTextHash("upload")},
		EvidenceLexical: document.EvidenceLexicalPolicyV1{CompletenessFingerprint: syntheticTextHash("complete"), LexicalSegmenterFingerprint: syntheticTextHash("segments"), MaxDocumentChars: 1000000,
			MaxSegmentRunes: 1000, MaxUnitRunes: 100000, NormalizedEvidenceContract: document.NormalizedEvidenceContractV1, NormalizerFingerprint: syntheticTextHash("normalizer"),
			RenditionContract: document.RenditionContractV1, SanitizerFingerprint: syntheticTextHash("sanitizer"), SourceEvidenceContract: document.SourceEvidenceContractV1},
		RetentionDisclosure: document.RetentionDisclosurePolicyV1{AttachmentPolicyFingerprint: syntheticTextHash("attachment"), ConsentFingerprint: syntheticTextHash("consent"), RetainSanitizedMarkdown: true, TrustBoundary: string(renditionDescriptor.TrustBoundary)},
		Retrieval:           document.RetrievalPolicyV1{LexicalLimit: 100, VectorLimit: 100},
		Embeddings: []document.EmbeddingBindingV1{{Activation: document.EmbeddingRequired, AuthorizationFingerprint: syntheticTextHash("embedding-auth"), CompatibilityID: descriptor.CompatibilityID,
			CredentialBinding: "credential:voyage", Descriptor: document.ProviderDescriptorV1{ID: descriptor.ID, Fingerprint: descriptor.Fingerprint}, Dimensions: descriptor.Dimension,
			DisclosureFingerprint: syntheticTextHash("embedding-disclosure"), DocumentFormatter: descriptor.DocumentFormatter, InputKind: document.EmbeddingInputRenditionChunk,
			MaxInputTokens: 1024, MaxBatchItems: 8, MaxInputBytes: 1 << 20, MaxResponseBytes: 1 << 20, Metric: descriptor.Metric, ModelInput: descriptor.ModelInput, Model: descriptor.Model, Name: "voyage-text",
			Normalization: descriptor.Normalization, QueryFormatter: descriptor.QueryFormatter, ScalarEncoding: descriptor.ScalarEncoding, TrustBoundary: string(descriptor.TrustBoundary),
			Chunk: &document.EmbeddingChunkPolicyV1{ContextFingerprint: syntheticTextHash("chunk-context"), Formatter: "rendition-chunk/v1", MaxTokens: 512, OverlapTokens: 8, Tokenizer: "synthetic-runes", TokenizerRevision: "v1", TruncationPolicy: document.TruncationPolicyReject}}}}
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(), Processing: docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{
		"voyage-text": {Profile: profile, RenditionProvider: renditionProvider,
			RenditionDisclosure:  docbank.ProcessingRuntimeDisclosure{ImmediateProcessor: "docbank-docling/v1", UltimateProcessor: renditionDescriptor.ID, Endpoint: doclingServer.URL, Deployment: "synthetic-docling-v1"},
			EmbeddingDisclosures: map[string]docbank.ProcessingRuntimeDisclosure{"voyage-text": {ImmediateProcessor: "docbank-voyage-embeddings/v1", UltimateProcessor: descriptor.ID, Endpoint: endpoint, Deployment: embeddingProfile.DeploymentEpoch, Model: descriptor.Model, ModelRevision: descriptor.ModelRevision}},
			EmbeddingProviders:   map[string]document.EmbeddingProvider{"voyage-text": embeddingProvider}, Tokenizers: map[string]document.Tokenizer{"voyage-text": voyageRuneTokenizer{}}}}}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.AddPage()
	pdf.SetFont("Arial", "", 12)
	pdf.Cell(40, 10, "Synthetic source to convert.")
	var source bytes.Buffer
	require.NoError(t, pdf.Output(&source))
	receipt, err := vault.Put(t.Context(), "/synthetic-report.pdf", bytes.NewReader(source.Bytes()), docbank.PutOptions{MediaType: "application/pdf"})
	require.NoError(t, err)
	request := docbank.ProcessingPlanRequest{Selector: docbank.ProcessingSelector{NodeID: receipt.Node.ID, ContentVersionID: receipt.Version.ID, Profile: "voyage-text"}}
	plan, err := vault.PlanProcessing(t.Context(), request)
	require.NoError(t, err)
	_, err = vault.StartProcessing(t.Context(), docbank.StartProcessingRequest{PlanRequest: request, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.NoError(t, err)
	report, err := vault.SearchDocuments(t.Context(), docbank.DocumentSearchRequest{Query: "maintenance", Mode: docbank.DocumentSearchHybrid, Profile: "voyage-text", BindingID: "voyage-text",
		Fence: docbank.DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: []string{receipt.Version.ID}}})
	require.NoError(t, err)
	require.Len(t, report.Results, 1)
	assert.Equal(t, receipt.Version.ID, report.Results[0].ContentVersionID)
	assert.Positive(t, report.Results[0].LexicalRank)
	assert.Positive(t, report.Results[0].SemanticRank)
	assert.Positive(t, documentCalls.Load())
	assert.EqualValues(t, 1, queryCalls.Load())
	assert.Equal(t, docbank.DocumentSearchHybrid, report.ActualMode)
	assert.Contains(t, report.Results[0].Excerpt, "maintenance")
	retained, err := vault.Rendition(t.Context(), docbank.RenditionRequest{Selector: request.Selector, MaxBytes: 1 << 20})
	require.NoError(t, err)
	markdown, err := io.ReadAll(retained.Reader)
	require.NoError(t, err)
	require.NoError(t, retained.Reader.Close())
	assert.Contains(t, string(markdown), "Solar panel maintenance requires an annual inspection")
}

func syntheticTextHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

type voyageRuneTokenizer struct{}

func (voyageRuneTokenizer) Identity() document.TokenizerIdentity {
	return document.TokenizerIdentity{Name: "synthetic-runes", Revision: "v1"}
}
func (voyageRuneTokenizer) PrefixTokenCountsMonotonic() bool { return true }
func (voyageRuneTokenizer) Tokenize(text string, limit int) ([]document.TokenBoundary, error) {
	if len([]rune(text)) > limit {
		return nil, document.ErrTokenizerLimit
	}
	var result []document.TokenBoundary
	for i, r := range text {
		result = append(result, document.TokenBoundary{Start: i, End: i + len(string(r))})
	}
	return result, nil
}
