package processing

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/store"
)

func TestWatchEmbeddingsEmbedOnlyAdmittedRenditions(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	renderer := newWorkerProvider(t)
	fixture, original := newLocalTextBackfillFixture(t, renderer)
	base := original.Service.profiles["pdf"]
	embedder := base.embedders["chunk"]
	var bindings document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(embeddingWorkerProfile(t, embedder.Descriptor()).CanonicalProfile, &bindings))
	portable := base.portable
	portable.Embeddings = bindings.Embeddings
	directDescriptor := embedder.Descriptor()
	directDescriptor.ID = "synthetic-direct"
	directDescriptor.Fingerprint = ""
	directDescriptor, err := document.NewEmbeddingDescriptor(directDescriptor)
	require.NoError(t, err)
	portable.Embeddings[1].Descriptor = document.ProviderDescriptorV1{ID: directDescriptor.ID, Fingerprint: directDescriptor.Fingerprint}
	directCalls := 0
	direct := localTextEmbeddingProvider{descriptor: directDescriptor, embed: func(context.Context) error {
		directCalls++
		return nil
	}}
	service, err := NewService(ctx, ServiceConfig{
		Catalog: fixture.catalog, Blobs: fixture.blobs, Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(),
		Profiles: map[string]ProfileConfig{"pdf": {
			Profile: portable, RenditionProvider: renderer, Tokenizers: base.tokenizers,
			EmbeddingProviders: map[string]document.EmbeddingProvider{"chunk": embedder, "direct": direct},
		}},
	})
	require.NoError(t, err)
	t.Cleanup(service.Stop)
	profile := service.profiles["pdf"]
	require.NoError(t, service.grantProfileConsent(ctx, profile, nil))
	lane := service.WatchEmbeddings("pdf")
	require.NotNil(t, lane)

	written, err := fixture.blobs.WriteDetailedContext(ctx, bytes.NewReader(mediatest.PDF()))
	require.NoError(t, err)
	createFile := func(name string) store.Node {
		t.Helper()
		node, err := fixture.catalog.CreateFile(ctx, fixture.catalog.RootID(), name, written.Hash, written.Size, "application/pdf", processingBlobPhysical(t, written))
		require.NoError(t, err)
		return node
	}
	admitted, older := createFile("admitted.pdf"), createFile("older.pdf")
	worker, err := NewRenditionWorker(RenditionWorkerConfig{
		Catalog: fixture.catalog, Blobs: fixture.blobs, Runtime: service.renditions, Gate: service.gate,
		Owner: "synthetic-watch-worker", LeaseDuration: time.Minute, IdleDelay: time.Millisecond,
	})
	require.NoError(t, err)
	require.NoError(t, service.AdmitImportedRendition(ctx, admitted, "pdf"))
	items, err := lane.targets(ctx, "", 10)
	require.NoError(t, err)
	require.Empty(t, items, "a queued rendition is not ready to embed")
	processed, err := worker.RunOne(ctx)
	require.NoError(t, err)
	require.True(t, processed)
	require.Equal(t, 1, renderer.calls)

	items, err = lane.targets(ctx, "", 10)
	require.NoError(t, err)
	require.Equal(t, []store.DocumentSummary{{NodeID: admitted.ID, ContentVersionID: admitted.CurrentVersionID, Name: admitted.Name}}, items)
	require.NoError(t, lane.process(ctx, items[0]))
	require.Equal(t, 1, renderer.calls, "embedding reuses the published rendition")
	require.Zero(t, directCalls)
	hasChunks, err := fixture.catalog.HasEmbeddingHeads(ctx, profile.record.Fingerprint, "chunk")
	require.NoError(t, err)
	require.True(t, hasChunks)
	hasOriginals, err := fixture.catalog.HasEmbeddingHeads(ctx, profile.record.Fingerprint, "direct")
	require.NoError(t, err)
	require.False(t, hasOriginals)
	jobs, err := fixture.catalog.EmbeddingJobsForVersionProfile(ctx, admitted.CurrentVersionID, profile.record.Fingerprint)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	require.Equal(t, "chunk", jobs[0].BindingID)
	items, err = lane.targets(ctx, "", 10)
	require.NoError(t, err)
	require.Empty(t, items, "an absent original-file head does not keep chunk work pending")

	_, err = fixture.catalog.ActiveRendition(ctx, older.CurrentVersionID, profile.record.Fingerprint)
	require.ErrorIs(t, err, store.ErrNotFound)
	jobs, err = fixture.catalog.EmbeddingJobsForVersionProfile(ctx, older.CurrentVersionID, profile.record.Fingerprint)
	require.NoError(t, err)
	require.Empty(t, jobs)

	pending := createFile("pending.pdf")
	require.NoError(t, service.AdmitImportedRendition(ctx, pending, "pdf"))
	_, err = worker.RunOne(ctx)
	require.NoError(t, err)
	items, err = lane.targets(ctx, "", 10)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, pending.CurrentVersionID, items[0].ContentVersionID)
	_, err = service.RevokeConsent(ctx)
	require.NoError(t, err)
	items, err = lane.targets(ctx, "", 10)
	require.NoError(t, err)
	require.Empty(t, items)
}

func TestWatchEmbeddingsRequiresChunkBinding(t *testing.T) {
	t.Parallel()
	service := &Service{profiles: map[string]configuredProfile{
		"direct": {portable: document.ProcessingProfileV1{
			Embeddings: []document.EmbeddingBindingV1{{Name: "direct", InputKind: document.EmbeddingInputOriginalFile}},
		}},
	}}
	require.Nil(t, service.WatchEmbeddings("direct"))
	require.Nil(t, service.WatchEmbeddings("unknown"))
}
