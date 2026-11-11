package processing

import (
	"bytes"
	"context"
	"image/color"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"encoding/json/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/vision"
	"go.kenn.io/docbank/document/vision/visiontest"
	"go.kenn.io/docbank/internal/retrieval"
	"go.kenn.io/docbank/internal/store"
)

func TestImageUnderstandingPublishesEmbedsAndSearchesSourceVersion(t *testing.T) {
	t.Parallel()
	model := strings.Repeat("m", 255)
	var partial atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fields := map[string]string{"visible_text": "SAVE\nNOW\nR and D &amp; partners\n===", "literal_description": "A cartoon dog beside flames.", "possible_meaning": strings.Repeat("ordinary context ", 100) + "denial of a crisis", "uncertainty": "Conversation context is unavailable."}
		if partial.Load() {
			fields = map[string]string{"visible_text": "image", "literal_description": "", "possible_meaning": " ", "uncertainty": ""}
		}
		raw, err := json.Marshal(fields)
		if !assert.NoError(t, err) {
			http.Error(w, "invalid response", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(visiontest.Completion(string(raw)))
	}))
	defer server.Close()
	renderer, err := vision.New(vision.Profile{DescriptorID: vision.UnderstandingProviderID, Origin: server.URL, Model: model, TrustBoundary: document.RenditionTrustOperatorNetwork, RequestTimeout: time.Minute, MaxResponseBytes: 1 << 20}, nil, server.Client())
	require.NoError(t, err)
	embedder := &understandingEmbeddingCapture{descriptor: embeddingWorkerDescriptor(t)}
	reranker := &understandingRerankingCapture{}
	fixture := newPublicationFixture(t)
	stored := workerProcessingProfile(t, renderer.Descriptor())
	var portable, embeddings document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(stored.CanonicalProfile, &portable))
	require.NoError(t, json.Unmarshal(embeddingWorkerProfile(t, embedder.Descriptor()).CanonicalProfile, &embeddings))
	portable.Embeddings = embeddings.Embeddings[:1]
	portable.Embeddings[0].Chunk.Formatter = "rendition-chunk-headings/v1"
	portable.Embeddings[0].MaxInputTokens = 512
	portable.Rendition.TrustBoundary = string(renderer.Descriptor().TrustBoundary)
	portable.Rendition.AdapterContract = vision.UnderstandingAdapterContract
	portable.Rendition.DiscloseFilename = false
	portable.Rendition.MaxUnits = 4
	portable.Rendition.RequestedArtifacts = []document.EvidenceArtifactRole{document.EvidenceArtifactStructured}
	portable.EvidenceLexical.MaxUnitRunes = 5000
	service, err := NewService(t.Context(), ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs, Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Profiles: map[string]ProfileConfig{"images": {Profile: portable, RenditionProvider: renderer, RenditionDisclosure: RuntimeDisclosure{Endpoint: server.URL, Model: model}, EmbeddingProviders: map[string]document.EmbeddingProvider{"chunk": embedder}, Tokenizers: map[string]document.Tokenizer{"chunk": embeddingIntegrationTokenizer{}}, RerankingProvider: reranker, RerankingProfile: retrieval.RerankingProfile{ID: "synthetic-reranker", MaxCandidates: 10, MaxExcerptBytes: 4096}, RerankingDeadline: time.Second, RerankingFailurePolicy: retrieval.ProviderFailureFailClosed, RerankingDisclosure: RuntimeDisclosure{ImmediateProcessor: "synthetic", UltimateProcessor: "synthetic", Endpoint: "https://rerank.example.invalid", Deployment: workerHash("reranker")}}}})
	require.NoError(t, err)
	t.Cleanup(service.Stop)
	written, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(mediatest.JPEG(32, 24, color.White)))
	require.NoError(t, err)
	node, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), "cartoon.jpg", written.Hash, written.Size, "image/jpeg", processingBlobPhysical(t, written))
	require.NoError(t, err)
	selector := Selector{Profile: "images", NodeID: node.ID, ContentVersionID: node.CurrentVersionID}
	plan, err := service.Plan(t.Context(), selector)
	require.NoError(t, err)
	require.Empty(t, plan.RenditionIneligibleReason)
	_, err = service.Start(t.Context(), StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.NoError(t, err)
	view, err := fixture.catalog.ActiveRendition(t.Context(), node.CurrentVersionID, service.profiles["images"].record.Fingerprint)
	require.NoError(t, err)
	assert.Equal(t, written.Hash, view.Build.SourceSHA256)
	assert.Equal(t, document.EvidenceDegradedProvenance, view.Build.Completeness)
	assert.NotEmpty(t, view.Build.ProviderReceipt)
	window, err := service.RenditionTextWindow(t.Context(), RenditionWindowRequest{VaultUID: fixture.catalog.VaultID(), NodeID: node.ID, ContentVersionID: node.CurrentVersionID, AttachmentID: view.Attachment.ID, MaxChars: 16000})
	require.NoError(t, err)
	assert.Contains(t, window.Text, model)
	assert.Contains(t, window.Text, "SAVE\nNOW")
	assert.Contains(t, window.Text, `R and D \&amp\; partners`)
	jobs, err := fixture.catalog.EmbeddingJobsForVersionProfile(t.Context(), node.CurrentVersionID, service.profiles["images"].record.Fingerprint)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	require.Equal(t, "completed", jobs[0].State, "%+v", jobs[0])
	require.NotEmpty(t, embedder.text)
	assert.Contains(t, embedder.text, "denial of a crisis")
	assert.Contains(t, embedder.text, "SAVE\nNOW")
	assert.Contains(t, embedder.text, "R and D &amp; partners")
	for _, input := range embedder.inputs {
		assert.Contains(t, input.Text, "Machine-generated image understanding; unverified; sender intent unknown / ")
		assert.Contains(t, input.Text, input.HeadingPath[1])
	}
	for _, mode := range []string{"lexical", "semantic", "hybrid"} {
		reranker.excerpts = nil
		report, err := service.Search(t.Context(), SearchRequest{Profile: "images", Query: "denial", Mode: mode, Rerank: true, Limit: 10, Fence: SourceFence{VaultUID: fixture.catalog.VaultID(), ContentVersionIDs: []string{node.CurrentVersionID}}})
		require.NoError(t, err, mode)
		require.Len(t, report.Results, 1, mode)
		assert.Equal(t, node.CurrentVersionID, report.Results[0].Document.ContentVersionID)
		require.NotEmpty(t, reranker.excerpts)
		assert.Contains(t, reranker.excerpts[0], "Machine-generated image understanding; unverified; sender intent unknown / ")
		assert.Contains(t, report.Results[0].Excerpt, "Machine-generated image understanding; unverified; sender intent unknown / ")
		assert.Contains(t, report.Results[0].Excerpt, "Possible meaning (interpretation)")
		assert.Contains(t, report.Results[0].Excerpt, "denial")
	}
	for _, mode := range []string{"semantic", "hybrid"} {
		report, err := service.Search(t.Context(), SearchRequest{Profile: "images", Query: "ignoring danger", Mode: mode, Limit: 10, Fence: SourceFence{VaultUID: fixture.catalog.VaultID(), ContentVersionIDs: []string{node.CurrentVersionID}}})
		require.NoError(t, err, mode)
		require.Len(t, report.Results, 1, mode)
		assert.Zero(t, report.Results[0].LexicalRank, mode)
		assert.NotZero(t, report.Results[0].SemanticRank, mode)
		assert.Contains(t, report.Results[0].Excerpt, "Machine-generated image understanding; unverified; sender intent unknown / Possible meaning (interpretation)", mode)
		assert.Contains(t, report.Results[0].Excerpt, "denial of a crisis", mode)
		require.NotEmpty(t, report.Results[0].Evidence, mode)
		assert.Equal(t, node.CurrentVersionID, report.Results[0].Evidence[0].ContentVersionID, mode)
	}
	literal, err := service.Search(t.Context(), SearchRequest{Profile: "images", Query: "partners", Mode: "lexical", Limit: 10, Fence: SourceFence{VaultUID: fixture.catalog.VaultID(), ContentVersionIDs: []string{node.CurrentVersionID}}})
	require.NoError(t, err)
	require.Len(t, literal.Results, 1)
	assert.Contains(t, literal.Results[0].Excerpt, "SAVE\nNOW")
	assert.Contains(t, literal.Results[0].Excerpt, "R and D &amp; partners")
	partial.Store(true)
	startInputs := len(embedder.inputs)
	written, err = fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(mediatest.JPEG(24, 32, color.White)))
	require.NoError(t, err)
	partialNode, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), "partial.jpg", written.Hash, written.Size, "image/jpeg", processingBlobPhysical(t, written))
	require.NoError(t, err)
	selector = Selector{Profile: "images", NodeID: partialNode.ID, ContentVersionID: partialNode.CurrentVersionID}
	plan, err = service.Plan(t.Context(), selector)
	require.NoError(t, err)
	_, err = service.Start(t.Context(), StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.NoError(t, err)
	partialView, err := fixture.catalog.ActiveRendition(t.Context(), partialNode.CurrentVersionID, service.profiles["images"].record.Fingerprint)
	require.NoError(t, err)
	partialWindow, err := service.RenditionTextWindow(t.Context(), RenditionWindowRequest{VaultUID: fixture.catalog.VaultID(), NodeID: partialNode.ID, ContentVersionID: partialNode.CurrentVersionID, AttachmentID: partialView.Attachment.ID, MaxChars: 16000})
	require.NoError(t, err)
	front, body, err := document.ParseRenditionFrontMatterV1([]byte(partialWindow.Text))
	require.NoError(t, err)
	require.Len(t, front.Navigation.Entries, 1)
	textStart := bytes.Index(body, []byte("\n\nimage\n"))
	require.NotEqual(t, -1, textStart)
	assert.Equal(t, textStart+2, front.Navigation.Entries[0].Byte)
	assert.Equal(t, 1+bytes.Count(body[:textStart+2], []byte("\n")), front.Navigation.Entries[0].Line)
	require.Greater(t, len(embedder.inputs), startInputs)
	for _, input := range embedder.inputs[startInputs:] {
		assert.Equal(t, "Visible text (best-effort transcription)", input.HeadingPath[1])
		assert.NotContains(t, input.Text, "No supported content provided")
	}
	report, err := service.Search(t.Context(), SearchRequest{Profile: "images", Query: "supported", Mode: "lexical", Limit: 10, Fence: SourceFence{VaultUID: fixture.catalog.VaultID(), ContentVersionIDs: []string{partialNode.CurrentVersionID}}})
	require.NoError(t, err)
	assert.Empty(t, report.Results)
	report, err = service.Search(t.Context(), SearchRequest{Profile: "images", Query: "endorsement", Mode: "lexical", Limit: 10, Fence: SourceFence{VaultUID: fixture.catalog.VaultID(), ContentVersionIDs: []string{partialNode.CurrentVersionID}}})
	require.NoError(t, err)
	assert.Empty(t, report.Results)
}

type understandingEmbeddingCapture struct {
	localTextEmbeddingProvider

	text   string
	inputs []document.EmbeddingInput
}

type understandingRerankingCapture struct{ excerpts []string }

func (p *understandingRerankingCapture) Rerank(_ context.Context, request retrieval.RerankingRequest) ([]retrieval.RerankScore, error) {
	scores := make([]retrieval.RerankScore, len(request.Candidates))
	for i, candidate := range request.Candidates {
		p.excerpts = append(p.excerpts, candidate.Excerpt)
		scores[i] = retrieval.RerankScore{Document: candidate.Document, Score: 1}
	}
	return scores, nil
}

func (p *understandingEmbeddingCapture) Embed(ctx context.Context, inputs []document.EmbeddingInput, auth document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
	for _, input := range inputs {
		p.text += input.Text + "\n"
		p.inputs = append(p.inputs, input)
	}
	result, err := p.localTextEmbeddingProvider.Embed(ctx, inputs, auth)
	if err != nil {
		return result, err
	}
	for i, input := range inputs {
		if input.Role != document.EmbeddingRoleQuery && !strings.Contains(input.Text, "denial of a crisis") {
			result.Vectors[i].Values = []float32{0, 1}
		}
	}
	return result, nil
}

func TestImageUnderstandingInvalidRepliesStopAfterAllowedAttempts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		raw      string
		attempts int
	}{
		{"missing fields", `{}`, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(visiontest.Completion(tc.raw))
			}))
			defer server.Close()
			renderer, err := vision.New(vision.Profile{DescriptorID: vision.UnderstandingProviderID, Origin: server.URL, Model: "synthetic-vision", TrustBoundary: document.RenditionTrustOperatorNetwork, RequestTimeout: time.Minute, MaxResponseBytes: 1 << 20}, nil, server.Client())
			require.NoError(t, err)
			fixture := newPublicationFixture(t)
			var portable document.ProcessingProfileV1
			require.NoError(t, json.Unmarshal(workerProcessingProfile(t, renderer.Descriptor()).CanonicalProfile, &portable))
			portable.Rendition.TrustBoundary = string(renderer.Descriptor().TrustBoundary)
			portable.Rendition.AdapterContract = vision.UnderstandingAdapterContract
			portable.Rendition.DiscloseFilename = false
			portable.Rendition.MaxUnits = 4
			portable.Rendition.RequestedArtifacts = []document.EvidenceArtifactRole{document.EvidenceArtifactStructured}
			service, err := NewService(t.Context(), ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs, Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Profiles: map[string]ProfileConfig{"images": {Profile: portable, RenditionProvider: renderer, RenditionDisclosure: RuntimeDisclosure{Endpoint: server.URL, Model: "synthetic-vision"}}}})
			require.NoError(t, err)
			t.Cleanup(service.Stop)
			written, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(mediatest.JPEG(32, 24, color.White)))
			require.NoError(t, err)
			node, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), "invalid-reply.jpg", written.Hash, written.Size, "image/jpeg", processingBlobPhysical(t, written))
			require.NoError(t, err)
			_, version, profile, err := service.resolve(t.Context(), Selector{Profile: "images", NodeID: node.ID, ContentVersionID: node.CurrentVersionID})
			require.NoError(t, err)
			require.NoError(t, service.grantProfileConsent(t.Context(), profile, nil))
			job, _, err := service.enqueueRendition(t.Context(), node, version, profile, service.renditionConsentRequest(profile), "", false)
			require.NoError(t, err)
			// Advance only retry scheduling; provider authorization keeps real time.
			var retrySkew atomic.Int64
			worker, err := NewRenditionWorker(RenditionWorkerConfig{Catalog: fixture.catalog, Blobs: fixture.blobs, Runtime: service.renditions, Gate: newWorkerTestGate(), Owner: "image-invalid-reply-worker", LeaseDuration: time.Minute, IdleDelay: time.Millisecond, Clock: func() time.Time { return time.Now().UTC().Add(time.Duration(retrySkew.Load())) }})
			require.NoError(t, err)
			for attempt := range tc.attempts {
				processed, err := worker.RunOne(t.Context())
				require.NoError(t, err)
				require.True(t, processed)
				current, err := fixture.catalog.RenditionJobByID(t.Context(), job.ID)
				require.NoError(t, err)
				assert.Equal(t, attempt+1, current.ProviderAttempts)
				if attempt < tc.attempts-1 {
					assert.Equal(t, store.RenditionJobRetryWait, current.State)
				} else {
					assert.Equal(t, store.RenditionJobFailed, current.State)
					assert.Equal(t, store.RenditionFailureTerminal, current.FailureCode)
				}
				retrySkew.Add(int64(time.Minute))
			}
			processed, err := worker.RunOne(t.Context())
			require.NoError(t, err)
			assert.False(t, processed, "a terminal job must not submit a fourth request")
		})
	}
}
