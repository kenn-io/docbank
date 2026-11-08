package processing

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/config"
)

func TestFacePlanRefusesUnsupportedSource(t *testing.T) {
	t.Parallel()
	x := newFaceEmbeddingFixture(t, false)
	written, err := x.f.blobs.WriteDetailedContext(t.Context(), bytes.NewBufferString("synthetic text"))
	require.NoError(t, err)
	node, err := x.f.catalog.CreateFile(t.Context(), x.f.catalog.RootID(), "source.txt", written.Hash, written.Size, "text/plain", processingBlobPhysical(t, written))
	require.NoError(t, err)
	plan, err := x.service.Plan(t.Context(), Selector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: config.LocalFacesProfile})
	require.NoError(t, err)
	require.NotEmpty(t, plan.RenditionIneligibleReason, "a text source must not offer face processing")
}

func TestUnsupportedFaceStartPreservesConsent(t *testing.T) {
	t.Parallel()
	x := newFaceEmbeddingFixture(t, false)
	written, err := x.f.blobs.WriteDetailedContext(t.Context(), bytes.NewBufferString("synthetic text"))
	require.NoError(t, err)
	node, err := x.f.catalog.CreateFile(t.Context(), x.f.catalog.RootID(), "source.txt", written.Hash, written.Size, "text/plain", processingBlobPhysical(t, written))
	require.NoError(t, err)
	selector := Selector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: config.LocalFacesProfile}
	plan, err := x.service.Plan(t.Context(), selector)
	require.NoError(t, err)
	require.True(t, plan.ConsentRequired)
	job, err := x.service.Start(t.Context(), StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.Error(t, err)
	require.Empty(t, job.ID)
	plan, err = x.service.Plan(t.Context(), selector)
	require.NoError(t, err)
	require.True(t, plan.ConsentRequired, "a refused source must preserve vault-wide face consent")
	require.Zero(t, x.calls)
}
