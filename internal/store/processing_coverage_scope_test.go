package store

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

// Catches scope filters being ignored, stale IDs leaking into all_live, and
// rebuilding counts losing their previous serving generation.
func TestProcessingCoverageScopeMatchesExactStates(t *testing.T) {
	t.Parallel()
	s, id, profile, _ := newEmbeddingCatalogFixture(t)
	trashed, err := s.NodeByPath(t.Context(), "/synthetic-source-b.pdf")
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), trashed.ID, trashed.Revision)
	require.NoError(t, err)
	missing, err := s.CreateFile(t.Context(), s.RootID(), "unprocessed.pdf", catalogSourceHash, 20, "application/pdf")
	require.NoError(t, err)
	other, err := s.CreateFile(t.Context(), s.RootID(), "other.txt", catalogSourceHash, 20, "text/plain")
	require.NoError(t, err)
	record := embeddingSetFixture(s, id, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		FencingToken: 1, Key: EmbeddingHeadKey{ContentVersionID: id, BindingID: record.BindingID, InputKind: record.InputKind},
		SetID: record.ID, VectorSpaceID: record.VectorSpace.ID, ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))
	for _, rebuilding := range []bool{false, true} {
		if rebuilding {
			_, err = s.EnqueueEmbeddingJob(t.Context(), embeddingJobTestRequest(t, s, id, profile, "scope-rebuild"))
			require.NoError(t, err)
		}
		exactScope := ProcessingCoverageScope{ContentVersionIDs: []string{id, missing.CurrentVersionID}, ProcessingProfileFingerprint: profile.Fingerprint, Bindings: []ProcessingCoverageBinding{{BindingID: "optional"}}}
		exact, err := s.ProcessingCoverage(t.Context(), exactScope)
		require.NoError(t, err)
		scopedScope := exactScope
		scopedScope.ContentVersionIDs = nil
		scopedScope.Filters = &SearchOptions{MIMEType: "APPLICATION/PDF"}
		scoped, err := s.ProcessingCoverage(t.Context(), scopedScope)
		require.NoError(t, err)
		require.Equal(t, exact.Renditions, scoped.Renditions)
		require.Equal(t, exact.Embeddings, scoped.Embeddings)
		require.Equal(t, "application/pdf", scoped.Filters.MIMEType)
		require.Equal(t, 2, scoped.Renditions.Total)
		require.Equal(t, 1, scoped.Embeddings[0].Unavailable)
		if rebuilding {
			require.Equal(t, 1, scoped.Embeddings[0].Rebuilding)
			require.Equal(t, 1, scoped.Embeddings[0].PreviousGenerationServing)
		} else {
			require.Equal(t, 1, scoped.Embeddings[0].Complete)
		}
		exactScope.ContentVersionIDs = append(exactScope.ContentVersionIDs, "00000000-0000-4000-8000-000000000001")
		stale, err := s.ProcessingCoverage(t.Context(), exactScope)
		require.NoError(t, err)
		require.Equal(t, 3, stale.Renditions.Total)
		require.Equal(t, 1, stale.Renditions.Stale)
		require.Equal(t, 1, stale.Embeddings[0].Stale)
	}
	allLive, err := s.ProcessingCoverage(t.Context(), ProcessingCoverageScope{Filters: &SearchOptions{}, ProcessingProfileFingerprint: profile.Fingerprint, Bindings: []ProcessingCoverageBinding{{BindingID: "optional"}}})
	require.NoError(t, err)
	allExact, err := s.ProcessingCoverage(t.Context(), ProcessingCoverageScope{ContentVersionIDs: []string{id, missing.CurrentVersionID, other.CurrentVersionID}, ProcessingProfileFingerprint: profile.Fingerprint, Bindings: []ProcessingCoverageBinding{{BindingID: "optional"}}})
	require.NoError(t, err)
	require.Equal(t, 3, allLive.Renditions.Total)
	require.Equal(t, allExact.Renditions, allLive.Renditions)
	require.Equal(t, allExact.Embeddings, allLive.Embeddings)
	empty, err := s.ProcessingCoverage(t.Context(), ProcessingCoverageScope{Filters: &SearchOptions{MIMEType: "image/png"}, ProcessingProfileFingerprint: profile.Fingerprint, Bindings: []ProcessingCoverageBinding{{BindingID: "optional"}}})
	require.NoError(t, err)
	require.Equal(t, ProcessingClassCoverage{Name: "rendition", State: "complete"}, empty.Renditions)
	require.Equal(t, ProcessingClassCoverage{Name: "optional", State: "complete"}, empty.Embeddings[0])
}

func TestProcessingCoverageScopeAboveFenceLimit(t *testing.T) {
	t.Parallel()
	s, _, profile, _ := newEmbeddingCatalogFixture(t)
	const total = MaxSearchSourceFenceIDs + 1
	for i := range total {
		node, err := s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("scope-scale-%04d.txt", i), catalogSourceHash, 20, "text/plain")
		require.NoError(t, err)
		_, err = s.EnqueueEmbeddingJob(t.Context(), embeddingJobTestRequest(t, s, node.CurrentVersionID, profile, "scope-large-"+node.CurrentVersionID))
		require.NoError(t, err)
	}
	report, err := s.ProcessingCoverage(t.Context(), ProcessingCoverageScope{Filters: &SearchOptions{MIMEType: "text/plain"}, ProcessingProfileFingerprint: profile.Fingerprint, Bindings: []ProcessingCoverageBinding{{BindingID: "optional"}}})
	require.NoError(t, err)
	require.Equal(t, total, report.Renditions.Total)
	require.Equal(t, total, report.Renditions.Unavailable)
	require.Equal(t, ProcessingClassCoverage{Name: "optional", State: "rebuilding", Total: total, Rebuilding: total}, report.Embeddings[0])
}

// Scope and exact fences must be exclusive regardless of which form is present.
func FuzzProcessingCoverageScopeExclusivity(f *testing.F) {
	for _, input := range []uint8{0, 1, 2, 3, 4, 255} {
		f.Add(input)
	}
	f.Fuzz(func(t *testing.T, input uint8) {
		s := newTestStore(t)
		scope := ProcessingCoverageScope{ProcessingProfileFingerprint: catalogProcessingProfile(t, false).Fingerprint}
		hasIDs, hasFilters := input&1 != 0, input&2 != 0
		if hasIDs {
			scope.ContentVersionIDs = []string{"00000000-0000-4000-8000-000000000001"}
		}
		if hasFilters {
			scope.Filters = &SearchOptions{}
		}
		report, err := s.ProcessingCoverage(t.Context(), scope)
		if hasIDs == hasFilters {
			require.ErrorIs(t, err, ErrInvalidProcessingSourceFence)
			return
		}
		require.NoError(t, err)
		if hasIDs {
			require.Equal(t, 1, report.Renditions.Stale)
		} else {
			require.Equal(t, "complete", report.Renditions.State)
			require.Zero(t, report.Renditions.Total)
		}
	})
}
