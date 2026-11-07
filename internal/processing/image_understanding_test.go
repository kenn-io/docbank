package processing

import (
	"bytes"
	"context"
	"image/color"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"encoding/json/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/vision"
	"go.kenn.io/docbank/document/vision/visiontest"
)

func TestImageUnderstandingPublishesEmbedsAndSearchesSourceVersion(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(visiontest.Completion(`{"visible_text":"FINE","literal_description":"A cartoon dog beside flames.","possible_meaning":"May joke about denial of a crisis.","uncertainty":"Conversation context is unavailable."}`))
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
	portable.Rendition.TrustBoundary = string(renderer.Descriptor().TrustBoundary)
	portable.Rendition.AdapterContract = vision.UnderstandingAdapterContract
	portable.Rendition.DiscloseFilename = false
	portable.Rendition.RequestedArtifacts = []document.EvidenceArtifactRole{document.EvidenceArtifactMarkdown}
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
	assert.Contains(t, embedder.headings, []string{"Machine-generated image understanding", "Possible meaning (interpretation)"})
	jobs, err := fixture.catalog.EmbeddingJobsForVersionProfile(t.Context(), node.CurrentVersionID, service.profiles["images"].record.Fingerprint)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, "completed", jobs[0].State)
	for _, mode := range []string{"lexical", "semantic", "hybrid"} {
		report, err := service.Search(t.Context(), SearchRequest{Profile: "images", Query: "denial", Mode: mode, Limit: 10, Fence: SourceFence{VaultUID: fixture.catalog.VaultID(), ContentVersionIDs: []string{node.CurrentVersionID}}})
		require.NoError(t, err, mode)
		require.Len(t, report.Results, 1, mode)
		assert.Equal(t, node.CurrentVersionID, report.Results[0].Document.ContentVersionID)
	}
}

type understandingEmbeddingCapture struct {
	localTextEmbeddingProvider

	text     string
	headings [][]string
}

func (p *understandingEmbeddingCapture) Embed(ctx context.Context, inputs []document.EmbeddingInput, auth document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
	for _, input := range inputs {
		p.text += input.Text + "\n"
		p.headings = append(p.headings, input.HeadingPath)
	}
	return p.localTextEmbeddingProvider.Embed(ctx, inputs, auth)
}
