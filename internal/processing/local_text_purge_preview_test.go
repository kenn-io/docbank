package processing

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/store"
)

func TestLocalTextPreviewExcludesPurgedDocuments(t *testing.T) {
	t.Parallel()
	renderer := newWorkerProvider(t)
	fixture, original := newLocalTextBackfillFixture(t, renderer)
	source := original.Service
	unused, err := fixture.catalog.ContentVersionByID(t.Context(), fixture.versionID)
	require.NoError(t, err)
	_, _, err = fixture.catalog.Trash(t.Context(), unused.NodeID, store.UnconditionalRev)
	require.NoError(t, err)
	written, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(mediatest.PDF()))
	require.NoError(t, err)
	node, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), "purged.pdf", written.Hash, written.Size, "application/pdf", processingBlobPhysical(t, written))
	require.NoError(t, err)
	selector := Selector{Profile: "pdf", NodeID: node.ID, ContentVersionID: node.CurrentVersionID}
	process, err := source.Plan(t.Context(), selector)
	require.NoError(t, err)
	_, err = source.Start(t.Context(), StartRequest{Selector: selector, PlanFingerprint: process.Fingerprint, Consent: true})
	require.NoError(t, err)
	require.Equal(t, 1, renderer.calls)
	_, err = fixture.catalog.PurgeDerivatives(t.Context(), store.PurgeRequest{ContentVersionIDs: []string{node.CurrentVersionID}})
	require.NoError(t, err)
	current := source.profiles["pdf"]
	var selected string
	setup := &LocalTextSetup{Service: source, Selected: "pdf", Candidate: func(string, string) (*Service, error) {
		candidate, err := NewService(t.Context(), ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs, Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Profiles: map[string]ProfileConfig{"pdf": {Profile: current.portable, RenditionProvider: current.provider, EmbeddingProviders: current.embedders, Tokenizers: current.tokenizers}}})
		if err == nil {
			t.Cleanup(candidate.Stop)
		}
		return candidate, err
	}, Startup: func(context.Context, bool, string) error { return nil }, Save: func(name, _ string) error { selected = name; return nil }}
	plan, err := setup.Plan(t.Context(), "local")
	require.NoError(t, err)
	pending, err := setup.targets(t.Context(), "", 50)
	require.NoError(t, err)
	require.Empty(t, pending, "purged content must stay outside activation")
	_, err = setup.Apply(t.Context(), "local", plan.Fingerprint)
	require.NoError(t, err)
	require.Equal(t, "pdf", selected)
	require.Equal(t, 1, renderer.calls, "activation must leave the purged document alone")
	t.Logf("preview=%q; activation targets=%d; provider calls after rerun=%d", plan.VectorEffect, len(pending), renderer.calls)
	require.Contains(t, plan.VectorEffect, "processes 0 documents before setup returns")
}
