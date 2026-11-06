package processing

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/store"
)

func TestLocalTextMoveReusesHostedRendition(t *testing.T) {
	t.Parallel()
	renderer := newWorkerProvider(t)
	renderer.descriptor.TrustBoundary = document.RenditionTrustHostedProvider
	renderer.descriptor.Fingerprint = ""
	var err error
	renderer.descriptor, err = document.NewRenditionDescriptor(renderer.descriptor)
	require.NoError(t, err)
	fixture, original := newLocalTextBackfillFixture(t, newWorkerProvider(t))
	base := original.Service.profiles["pdf"]
	hosted := base.portable
	hosted.Rendition = new(*hosted.Rendition)
	hosted.Rendition.Descriptor.Fingerprint = renderer.Descriptor().Fingerprint
	hosted.Rendition.TrustBoundary = string(document.RenditionTrustHostedProvider)
	source, err := NewService(t.Context(), ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs, Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Profiles: map[string]ProfileConfig{"pdf": {Profile: hosted, RenditionProvider: renderer, RenditionDisclosure: RuntimeDisclosure{Endpoint: "https://extractor.example.org"}, EmbeddingProviders: base.embedders, Tokenizers: base.tokenizers}}})
	require.NoError(t, err)
	t.Cleanup(source.Stop)
	// Exclude the helper's unrelated, unprocessed source from the live vault.
	version, err := fixture.catalog.ContentVersionByID(t.Context(), fixture.versionID)
	require.NoError(t, err)
	_, _, err = fixture.catalog.Trash(t.Context(), version.NodeID, store.UnconditionalRev)
	require.NoError(t, err)
	written, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(mediatest.PDF()))
	require.NoError(t, err)
	node, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), "indexed.pdf", written.Hash, written.Size, "application/pdf", processingBlobPhysical(t, written))
	require.NoError(t, err)
	selector := Selector{Profile: "pdf", NodeID: node.ID, ContentVersionID: node.CurrentVersionID}
	plan, err := source.Plan(t.Context(), selector)
	require.NoError(t, err)
	_, err = source.Start(t.Context(), StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.NoError(t, err)
	require.Equal(t, 1, renderer.calls)
	old := source.profiles["pdf"]
	before, err := fixture.catalog.ActiveRendition(t.Context(), node.CurrentVersionID, old.record.Fingerprint)
	require.NoError(t, err)
	portable := old.portable
	portable.Embeddings = append(portable.Embeddings[:0:0], portable.Embeddings...)
	portable.Embeddings[0].AuthorizationFingerprint = workerHash("moved-text-transport")
	var moved *Service
	setup := &LocalTextSetup{Service: source, Selected: "pdf", Candidate: func(string, string) (*Service, error) {
		moved, err = NewService(t.Context(), ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs, Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Profiles: map[string]ProfileConfig{"pdf": {Profile: portable, RenditionProvider: renderer, RenditionDisclosure: old.renderDisclosure, EmbeddingProviders: old.embedders, Tokenizers: old.tokenizers}}})
		if err == nil {
			t.Cleanup(moved.Stop)
		}
		return moved, err
	}, Startup: func(context.Context, bool, string) error { return nil }, Save: func(string, string) error { return nil }}
	move, err := setup.Plan(t.Context(), "local")
	require.NoError(t, err)
	_, err = setup.Apply(t.Context(), "local", move.Fingerprint)
	require.NoError(t, err)
	current := moved.profiles["pdf"]
	require.NotEqual(t, old.record.Fingerprint, current.record.Fingerprint)
	after, err := fixture.catalog.ActiveRendition(t.Context(), node.CurrentVersionID, current.record.Fingerprint)
	require.NoError(t, err)
	t.Logf("renderer calls before=1 after=%d; source build=%s; moved build=%s", renderer.calls, before.Build.ID, after.Build.ID)
	require.Equal(t, 1, renderer.calls, "an already-indexed document must reuse its hosted extractor output")
	require.Equal(t, before.Build.ID, after.Build.ID)
}
