package processing

import (
	"bytes"
	"context"
	"image/color"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/store"
)

func TestLocalTextMixedRecipeFinishesTerminalPreview(t *testing.T) {
	t.Parallel()
	renderer := newWorkerProvider(t)
	renderer.descriptor.SupportedFormats = []document.RenditionFormatCapability{{MediaFamily: "image", MediaType: "image/jpeg", InputKind: document.RenditionInputOriginalFile}}
	renderer.descriptor.Fingerprint = ""
	var err error
	renderer.descriptor, err = document.NewRenditionDescriptor(renderer.descriptor)
	require.NoError(t, err)
	fixture, setup := newLocalTextMixedFixture(t, localTextImageRenderer{renderer})
	service := setup.Service
	written, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(mediatest.JPEG(32, 24, color.White)))
	require.NoError(t, err)
	run, err := fixture.catalog.BeginIngest(t.Context(), "cli", "Synthetic terminal preview")
	require.NoError(t, err)
	node, err := fixture.catalog.IngestFileExact(t.Context(), run, fixture.catalog.RootID(), "indexed.jpg", written.Hash, written.Size, "image/jpeg", "indexed.jpg", "", processingBlobPhysical(t, written))
	require.NoError(t, err)
	recipe, _ := VisualPreviewRecipeForSize("grid")
	canonical, _, err := document.MarshalVisualPreviewV1(document.VisualPreviewV1{ContractVersion: document.VisualPreviewContractV1, SourceSHA256: written.Hash, Recipe: recipe, State: document.VisualPreviewFailed, Failure: &document.VisualPreviewFailureV1{Code: "decode_failed", Detail: "Synthetic terminal decoder outcome"}})
	require.NoError(t, err)
	_, err = fixture.catalog.PublishVisualPreviewGeneration(t.Context(), node.CurrentVersionID, canonical, nil)
	require.NoError(t, err)
	require.NoError(t, service.grantProfileConsent(t.Context(), service.profiles["mixed"], nil))
	item := store.DocumentSummary{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Name: node.Name}
	require.NoError(t, setup.process(t.Context(), item))
	jobs, err := fixture.catalog.EmbeddingJobsForVersionProfile(t.Context(), node.CurrentVersionID, service.profiles["mixed"].record.Fingerprint)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	require.Equal(t, "completed", jobs[0].State)
	require.Equal(t, "chunk", jobs[0].BindingID)
	items, err := setup.targets(t.Context(), "", 50)
	require.NoError(t, err)
	require.NotContains(t, items, store.DocumentSummary{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Name: node.Name})
}

type localTextImageRenderer struct{ *workerProvider }

func (p localTextImageRenderer) Render(ctx context.Context, upload document.AuthorizedUpload, auth document.RenditionAuthorization) (document.RenditionResult, error) {
	r, err := p.workerProvider.Render(ctx, upload, auth)
	r.Evidence.Family = "image"
	return r, err
}

func newLocalTextMixedFixture(t *testing.T, renderer document.RenditionProvider) (publicationFixture, *LocalTextSetup) {
	t.Helper()
	fixture, original := newLocalTextBackfillFixture(t, renderer)
	source := original.Service.profiles["pdf"]
	descriptor := embeddingWorkerDescriptor(t)
	descriptor.InputKinds = []document.EmbeddingInputKind{document.EmbeddingInputImage}
	descriptor, err := document.NewEmbeddingDescriptor(descriptor)
	require.NoError(t, err)
	image := source.portable.Embeddings[0]
	image.Name, image.InputKind = "photo", document.EmbeddingInputImage
	image.Descriptor.Fingerprint = descriptor.Fingerprint
	image.Chunk, image.MaxInputTokens = nil, 0
	portable := source.portable
	portable.Embeddings = append([]document.EmbeddingBindingV1{source.portable.Embeddings[0]}, image)
	service, err := NewService(t.Context(), ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
		Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Profiles: map[string]ProfileConfig{"mixed": {
			Profile: portable, RenditionProvider: source.provider,
			EmbeddingProviders: map[string]document.EmbeddingProvider{"chunk": source.embedders["chunk"], "photo": &localTextEmbeddingProvider{descriptor: descriptor}},
			Tokenizers:         source.tokenizers,
		}}})
	require.NoError(t, err)
	t.Cleanup(service.Stop)
	return fixture, &LocalTextSetup{Service: service, Selected: "mixed"}
}
