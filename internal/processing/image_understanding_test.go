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
	"go.kenn.io/docbank/internal/store"
)

func TestImageUnderstandingPublishesEmbedsAndSearchesSourceVersion(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		raw, err := json.Marshal(map[string]string{"visible_text": "R and D &amp; partners\n===", "literal_description": "A cartoon dog beside flames.", "possible_meaning": strings.Repeat("ordinary context ", 100) + "denial of a crisis", "uncertainty": "Conversation context is unavailable."})
		require.NoError(t, err)
		_, _ = w.Write(visiontest.Completion(string(raw)))
	}))
	defer server.Close()
	renderer, err := vision.New(vision.Profile{DescriptorID: vision.UnderstandingProviderID, Origin: server.URL, Model: "synthetic-vision", TrustBoundary: document.RenditionTrustOperatorNetwork, RequestTimeout: time.Minute, MaxResponseBytes: 1 << 20}, nil, server.Client())
	require.NoError(t, err)
	embedder := &understandingEmbeddingCapture{descriptor: embeddingWorkerDescriptor(t)}
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
	portable.Rendition.RequestedArtifacts = []document.EvidenceArtifactRole{document.EvidenceArtifactMarkdown}
	portable.EvidenceLexical.MaxUnitRunes = 5000
	service, err := NewService(t.Context(), ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs, Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Profiles: map[string]ProfileConfig{"images": {Profile: portable, RenditionProvider: renderer, RenditionDisclosure: RuntimeDisclosure{Endpoint: server.URL, Model: "synthetic-vision"}, EmbeddingProviders: map[string]document.EmbeddingProvider{"chunk": embedder}, Tokenizers: map[string]document.Tokenizer{"chunk": embeddingIntegrationTokenizer{}}}}})
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
	require.NotEmpty(t, embedder.text)
	assert.Contains(t, embedder.text, "denial of a crisis")
	assert.Contains(t, embedder.text, "R and D &amp; partners")
	assert.Contains(t, embedder.headings, []string{"Machine-generated image understanding; unverified; sender intent unknown", "Possible meaning (interpretation)"})
	for _, input := range embedder.inputs {
		assert.Contains(t, input.Text, "Machine-generated image understanding; unverified; sender intent unknown / ")
		assert.Contains(t, input.Text, input.HeadingPath[1])
	}
	jobs, err := fixture.catalog.EmbeddingJobsForVersionProfile(t.Context(), node.CurrentVersionID, service.profiles["images"].record.Fingerprint)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, "completed", jobs[0].State)
	for _, mode := range []string{"lexical", "semantic", "hybrid"} {
		report, err := service.Search(t.Context(), SearchRequest{Profile: "images", Query: "denial", Mode: mode, Limit: 10, Fence: SourceFence{VaultUID: fixture.catalog.VaultID(), ContentVersionIDs: []string{node.CurrentVersionID}}})
		require.NoError(t, err, mode)
		require.Len(t, report.Results, 1, mode)
		assert.Equal(t, node.CurrentVersionID, report.Results[0].Document.ContentVersionID)
		assert.Contains(t, report.Results[0].Excerpt, "Machine-generated image understanding; unverified; sender intent unknown / ")
		if mode != "semantic" {
			assert.Contains(t, report.Results[0].Excerpt, "Possible meaning (interpretation)")
			assert.Contains(t, report.Results[0].Excerpt, "denial")
		}
	}
}

type understandingEmbeddingCapture struct {
	localTextEmbeddingProvider

	text     string
	headings [][]string
	inputs   []document.EmbeddingInput
}

func (p *understandingEmbeddingCapture) Embed(ctx context.Context, inputs []document.EmbeddingInput, auth document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
	for _, input := range inputs {
		p.text += input.Text + "\n"
		p.headings = append(p.headings, input.HeadingPath)
		p.inputs = append(p.inputs, input)
	}
	return p.localTextEmbeddingProvider.Embed(ctx, inputs, auth)
}

func TestImageUnderstandingInvalidRepliesStopAfterThreeAttempts(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(visiontest.Completion(`{}`))
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
	portable.Rendition.RequestedArtifacts = []document.EvidenceArtifactRole{document.EvidenceArtifactMarkdown}
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
	for attempt := range 3 {
		processed, err := worker.RunOne(t.Context())
		require.NoError(t, err)
		require.True(t, processed)
		current, err := fixture.catalog.RenditionJobByID(t.Context(), job.ID)
		require.NoError(t, err)
		assert.Equal(t, attempt+1, current.ProviderAttempts)
		if attempt < 2 {
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
	assert.Equal(t, int32(3), calls.Load())
}
