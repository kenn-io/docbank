package processing

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

func TestFaceProfileRequiresDedicatedSetup(t *testing.T) {
	t.Parallel()
	x := newFaceEmbeddingFixture(t, false)
	selector := x.f.selector(config.LocalFacesProfile)
	plan, err := x.service.Plan(t.Context(), selector)
	require.ErrorIs(t, err, ErrProfileNotConfigured)
	_, err = x.service.Start(t.Context(), StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.ErrorIs(t, err, ErrProfileNotConfigured)
	_, err = x.service.GrantConsent(t.Context(), ConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
	require.ErrorIs(t, err, ErrProfileNotConfigured)
	_, err = x.service.Search(t.Context(), SearchRequest{Profile: config.LocalFacesProfile, Query: "Ada", Mode: "lexical", Limit: 24, AllLive: &store.SearchOptions{}})
	require.ErrorIs(t, err, ErrProfileNotConfigured)
	fence := SourceFence{VaultUID: x.f.catalog.VaultID(), ContentVersionIDs: []string{x.f.node.CurrentVersionID}}
	_, err = x.service.Coverage(t.Context(), config.LocalFacesProfile, fence)
	require.ErrorIs(t, err, ErrProfileNotConfigured)
	_, err = x.service.Search(t.Context(), SearchRequest{Profile: config.LocalFacesProfile, Query: "Ada", Mode: "lexical", Limit: 24, Fence: fence})
	require.ErrorIs(t, err, ErrProfileNotConfigured)
	for _, profile := range x.service.Profiles() {
		assert.NotEqual(t, config.LocalFacesProfile, profile.Name, "file processing must only list per-file profiles")
	}
}

func TestFaceJobStatusRequiresDedicatedSetup(t *testing.T) {
	t.Parallel()
	x := newFaceEmbeddingFixture(t, false)
	ctx := t.Context()
	x.enable(t)
	x.worker.catalog = &countedReconcileCatalog{Store: x.f.catalog, pauseClaims: true}
	_, err := x.worker.ScanOnce(ctx)
	require.NoError(t, err)
	now := time.Now()
	claim, _, found, err := x.f.catalog.ClaimNextEmbeddingWork(ctx, "status-observer", now, time.Minute, x.worker.descriptorFingerprints)
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, x.f.catalog.ReleaseEmbeddingWork(ctx, claim, now))
	job, err := x.f.catalog.EmbeddingJobByID(ctx, claim.AttemptID)
	require.NoError(t, err)
	require.Equal(t, "queued", job.State)
	_, err = x.service.Status(ctx, claim.AttemptID)
	require.ErrorIs(t, err, ErrProfileNotConfigured)
}
