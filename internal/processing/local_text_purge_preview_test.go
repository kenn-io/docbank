package processing

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/store"
)

func TestLocalTextPurgePreviewTracksSelectedProfile(t *testing.T) {
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
	setup := newLocalTextMoveSetup(t, source, &selected)
	plan, err := setup.Plan(t.Context(), "local")
	require.NoError(t, err)
	require.NotEqual(t, current.record.Fingerprint, plan.ProfileFingerprint)
	require.Contains(t, plan.VectorEffect, "processes 1 documents before setup returns")
	_, err = setup.Apply(t.Context(), "local", plan.Fingerprint)
	require.NoError(t, err)
	require.Equal(t, "pdf", selected)
	require.Equal(t, 2, renderer.calls, "the moved profile has its own purge scope")
	setup.Service, err = setup.Candidate("local", "pdf")
	require.NoError(t, err)
	_, err = fixture.catalog.PurgeDerivatives(t.Context(), store.PurgeRequest{ContentVersionIDs: []string{node.CurrentVersionID}})
	require.NoError(t, err)
	plan, err = setup.Plan(t.Context(), "local")
	require.NoError(t, err)
	require.Contains(t, plan.VectorEffect, "processes 0 documents before setup returns")
	_, err = setup.Apply(t.Context(), "local", plan.Fingerprint)
	require.NoError(t, err)
	require.Equal(t, 2, renderer.calls, "rerunning the selected profile must respect its purge")
}
