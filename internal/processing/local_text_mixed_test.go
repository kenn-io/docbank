package processing

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
)

func TestLocalTextMixedRecipeFinishesIneligibleImage(t *testing.T) {
	t.Parallel()
	fixture, original := newLocalTextBackfillFixture(t, newWorkerProvider(t))
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
	defer service.Stop()
	written, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(mediatest.PDF()))
	require.NoError(t, err)
	node, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), "indexed.pdf", written.Hash, written.Size, "application/pdf", processingBlobPhysical(t, written))
	require.NoError(t, err)
	selector := Selector{Profile: "mixed", NodeID: node.ID, ContentVersionID: node.CurrentVersionID}
	plan, err := service.Plan(t.Context(), selector)
	require.NoError(t, err)
	_, err = service.Start(t.Context(), StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.NoError(t, err)
	setup := &LocalTextSetup{Service: service, Selected: "mixed"}
	items, err := setup.targets(t.Context(), "", 50)
	require.NoError(t, err)
	for _, item := range items {
		require.NotEqual(t, node.CurrentVersionID, item.ContentVersionID, "the completed text and ineligible image must finish this document")
	}
}
