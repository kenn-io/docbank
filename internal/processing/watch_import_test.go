package processing

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"path"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/config"
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
		WatchProfiles: []string{"pdf"},
		Profiles: map[string]ProfileConfig{"pdf": {
			Profile: portable, RenditionProvider: renderer, Tokenizers: base.tokenizers,
			EmbeddingProviders: map[string]document.EmbeddingProvider{"chunk": embedder, "direct": direct},
		}},
	})
	require.NoError(t, err)
	t.Cleanup(service.Stop)
	profile := service.profiles["pdf"]
	require.NoError(t, service.grantProfileConsent(ctx, profile, nil))
	lane := service.WatchEmbeddings("pdf", []config.WatchConfig{{Name: "inbox", Destination: "/", Profile: "pdf"}})
	require.NotNil(t, lane)

	createFile := func(name string) store.Node {
		t.Helper()
		return importWatched(t, fixture, "inbox", "/", name, mediatest.PDF())
	}
	admitted, older := createFile("admitted.pdf"), createFile("older.pdf")
	service.profiles["pdf-alias"] = profile
	plan, err := service.Plan(ctx, Selector{Profile: "pdf-alias", NodeID: admitted.ID, ContentVersionID: admitted.CurrentVersionID})
	require.NoError(t, err)
	require.Contains(t, plan.Estimate.Scope, "files imported by watches that route to this profile", "profile aliases share the same consent")
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
	before, err := fixture.catalog.ActiveRendition(ctx, admitted.CurrentVersionID, profile.record.Fingerprint)
	require.NoError(t, err)
	admitted, _, err = fixture.catalog.Move(ctx, admitted.ID, fixture.catalog.RootID(), "renamed.pdf", store.UnconditionalRev)
	require.NoError(t, err)
	require.Equal(t, before.Attachment.ContentVersionID, admitted.CurrentVersionID)

	items, err = lane.targets(ctx, "", 10)
	require.NoError(t, err)
	require.Equal(t, []store.DocumentSummary{{NodeID: admitted.ID, ContentVersionID: admitted.CurrentVersionID, Name: admitted.Name}}, items)
	require.NoError(t, lane.process(ctx, items[0]))
	require.Equal(t, 1, renderer.calls, "renaming after publication keeps the published rendition")
	after, err := fixture.catalog.ActiveRendition(ctx, admitted.CurrentVersionID, profile.record.Fingerprint)
	require.NoError(t, err)
	require.Equal(t, before.Attachment, after.Attachment)
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
	require.NoError(t, lane.process(ctx, store.DocumentSummary{NodeID: pending.ID, ContentVersionID: pending.CurrentVersionID, Name: pending.Name}))
	jobs, err = fixture.catalog.EmbeddingJobsForVersionProfile(ctx, pending.CurrentVersionID, profile.record.Fingerprint)
	require.NoError(t, err)
	require.Empty(t, jobs, "revocation after listing fences work")
}

func TestWatchEmbeddingsRequiresChunkBinding(t *testing.T) {
	t.Parallel()
	service := &Service{profiles: map[string]configuredProfile{
		"direct": {portable: document.ProcessingProfileV1{
			Embeddings: []document.EmbeddingBindingV1{{Name: "direct", InputKind: document.EmbeddingInputOriginalFile}},
		}},
	}}
	watches := []config.WatchConfig{{Name: "inbox", Profile: "direct"}, {Name: "mapped", Profiles: config.WatchProfiles{"document": "unknown"}}}
	require.Nil(t, service.WatchEmbeddings("direct", watches))
	require.Nil(t, service.WatchEmbeddings("unknown", watches))
}

func TestWatchEmbeddingsProcessesReplacementAfterConsent(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	renderer := newWorkerProvider(t)
	fixture, local := newLocalTextBackfillFixture(t, renderer)
	service := local.Service
	profile := service.profiles["pdf"]
	_, err := service.RevokeConsent(ctx)
	require.NoError(t, err)
	source := mediatest.PDF()
	node := importWatched(t, fixture, "inbox", "/", "watched.pdf", source)
	require.NoError(t, service.AdmitImportedRendition(ctx, node, "pdf"))
	worker, err := NewRenditionWorker(RenditionWorkerConfig{
		Catalog: fixture.catalog, Blobs: fixture.blobs, Runtime: service.renditions, Gate: service.gate,
		Owner: "review-watch-update", LeaseDuration: time.Minute, IdleDelay: time.Millisecond,
	})
	require.NoError(t, err)
	require.NoError(t, service.grantProfileConsent(ctx, profile, nil))
	worked, err := worker.RunOne(ctx)
	require.NoError(t, err)
	require.False(t, worked, "grant does not admit the skipped version")
	watches := []config.WatchConfig{{Name: "inbox", Destination: "/", Profile: "pdf"}}
	lane := service.WatchEmbeddings("pdf", watches)
	items, err := lane.targets(ctx, "", 100)
	require.NoError(t, err)
	require.Empty(t, items)
	updated, err := fixture.blobs.WriteDetailedContext(ctx, bytes.NewReader(append(bytes.Clone(source), '\n')))
	require.NoError(t, err)
	current, _, changed, err := fixture.catalog.SyncWatchedContent(ctx, "inbox", "watched.pdf", updated.Hash, updated.Size, "application/pdf", processingBlobPhysical(t, updated))
	require.NoError(t, err)
	require.True(t, changed)
	require.NotEqual(t, node.CurrentVersionID, current.CurrentVersionID)
	require.NoError(t, service.AdmitImportedRendition(ctx, current, "pdf"))
	items, err = lane.targets(ctx, "", 100)
	require.NoError(t, err)
	require.Empty(t, items)
	worked, err = worker.RunOne(ctx)
	require.NoError(t, err)
	require.True(t, worked)
	items, err = lane.targets(ctx, "", 100)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, current.CurrentVersionID, items[0].ContentVersionID)
	require.NoError(t, lane.process(ctx, items[0]))
	jobs, err := fixture.catalog.EmbeddingJobsForVersionProfile(ctx, current.CurrentVersionID, profile.record.Fingerprint)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	require.Equal(t, "completed", jobs[0].State)
	require.Equal(t, 1, renderer.calls)
	items, err = service.WatchEmbeddings("pdf", watches).targets(ctx, "", 100)
	require.NoError(t, err)
	require.Empty(t, items, "published work is settled in a fresh lane")
	_, err = fixture.catalog.ActiveRendition(ctx, node.CurrentVersionID, profile.record.Fingerprint)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestWatchEmbeddingsEmbedOnlyFilesTheirWatchesImported(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	renderer := newWorkerProvider(t)
	var calls atomic.Int32
	embedder := localTextEmbeddingProvider{descriptor: embeddingWorkerDescriptor(t), embed: func(context.Context) error {
		calls.Add(1)
		return nil
	}}
	fixture, local := newLocalTextBackfillFixture(t, renderer, embedder)
	service := local.Service
	worker, err := NewRenditionWorker(RenditionWorkerConfig{
		Catalog: fixture.catalog, Blobs: fixture.blobs, Runtime: service.renditions, Gate: service.gate,
		Owner: "watch-scope", LeaseDuration: time.Minute, IdleDelay: time.Millisecond,
	})
	require.NoError(t, err)
	// Each file gets a published rendition head for the profile, as a manual
	// build or watch admission would leave it.
	render := func(node store.Node) store.Node {
		t.Helper()
		require.NoError(t, service.AdmitImportedRendition(ctx, node, "pdf"))
		worked, err := worker.RunOne(ctx)
		require.NoError(t, err)
		require.True(t, worked)
		_, err = fixture.catalog.ActiveRendition(ctx, node.CurrentVersionID, service.profiles["pdf"].record.Fingerprint)
		require.NoError(t, err)
		return node
	}
	content := func(label string) []byte { return append(bytes.Clone(mediatest.PDF()), "\n%"+label+"\n"...) }
	manual := func(directory, name string) store.Node {
		t.Helper()
		written, err := fixture.blobs.WriteDetailedContext(ctx, bytes.NewReader(content(name)))
		require.NoError(t, err)
		parent, err := fixture.catalog.MkdirAll(ctx, directory)
		require.NoError(t, err)
		node, err := fixture.catalog.CreateFile(ctx, parent.ID, name, written.Hash, written.Size, "application/pdf", processingBlobPhysical(t, written))
		require.NoError(t, err)
		return node
	}
	// The outer watch routes to pdf; a watch nested inside its destination
	// routes documents elsewhere.
	watches := []config.WatchConfig{
		{Name: "inbox", Destination: "/inbox", Profile: "pdf"},
		{Name: "nested", Destination: "/inbox/nested", Profiles: config.WatchProfiles{"document": "other"}},
	}
	outside := render(manual("/elsewhere", "outside.pdf"))
	uploaded := render(manual("/inbox", "uploaded.pdf"))
	nested := render(importWatched(t, fixture, "nested", "/inbox/nested", "deep.pdf", content("deep")))
	require.Nil(t, service.WatchEmbeddings("pdf", watches[1:]), "a watch that routes elsewhere starts no lane")
	lane := service.WatchEmbeddings("pdf", watches)
	require.NotNil(t, lane)
	items, err := lane.targets(ctx, "", 100)
	require.NoError(t, err)
	require.Empty(t, items, "manual renditions and other watches' files are not watch work")

	watched := render(importWatched(t, fixture, "inbox", "/inbox", "watched.pdf", content("watched")))
	items, err = lane.targets(ctx, "", 100)
	require.NoError(t, err)
	require.Equal(t, []store.DocumentSummary{{NodeID: watched.ID, ContentVersionID: watched.CurrentVersionID, Name: watched.Name}}, items)
	require.Zero(t, calls.Load())
	require.NoError(t, lane.process(ctx, items[0]))
	require.Positive(t, calls.Load(), "the watched file is embedded")
	fingerprint := service.profiles["pdf"].record.Fingerprint
	jobs, err := fixture.catalog.EmbeddingJobsForVersionProfile(ctx, watched.CurrentVersionID, fingerprint)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	for name, node := range map[string]store.Node{"outside every watch": outside, "uploaded into the destination": uploaded, "imported by a nested watch": nested} {
		jobs, err := fixture.catalog.EmbeddingJobsForVersionProfile(ctx, node.CurrentVersionID, fingerprint)
		require.NoError(t, err)
		require.Empty(t, jobs, name)
	}
	items, err = lane.targets(ctx, "", 100)
	require.NoError(t, err)
	require.Empty(t, items)
}

// importWatched records a new source the way the named watch imports it.
func importWatched(t *testing.T, fixture publicationFixture, watch, destination, sourceRef string, content []byte) store.Node {
	t.Helper()
	ctx := t.Context()
	written, err := fixture.blobs.WriteDetailedContext(ctx, bytes.NewReader(content))
	require.NoError(t, err)
	parent, err := fixture.catalog.MkdirAll(ctx, path.Join(destination, path.Dir(sourceRef)))
	require.NoError(t, err)
	run, err := fixture.catalog.BeginIngest(ctx, "watch", watch)
	require.NoError(t, err)
	node, err := fixture.catalog.IngestFileExact(ctx, run, parent.ID, path.Base(sourceRef), written.Hash, written.Size, "application/pdf", sourceRef, "", processingBlobPhysical(t, written))
	require.NoError(t, err)
	return node
}
