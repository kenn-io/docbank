package store_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestEmbeddedProductionNumberLookupUsesPublishedArtifactAuthority(t *testing.T) {
	metadata, root, job, _ := store.PublishedProductionPackageHTTPFixture(t)
	plan, err := metadata.LoadProductionRenderPlan(t.Context(), job.ID)
	require.NoError(t, err)
	allocation, err := metadata.ProductionNumberingForJob(t.Context(), job.ID)
	require.NoError(t, err)
	label := plan.Reservation.Numbers[0].Text
	stored, err := metadata.FindPublishedProductionNumber(t.Context(), label)
	require.NoError(t, err)
	require.NoError(t, metadata.Close())
	embedded, err := docbank.New(t.Context(), docbank.Config{Root: root})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, embedded.Close()) })
	exact, err := embedded.ProductionNumbers(t.Context(), docbank.ProductionNumberQuery{Label: label})
	require.NoError(t, err)
	require.Len(t, exact.Items, 1)
	require.Equal(t, api.ProductionNumberReference{
		Label: stored.Label, JobID: stored.JobID, SetID: stored.SetID, Revision: stored.Revision,
		ProductionReceiptSHA256: stored.ProductionReceiptSHA256,
		ArtifactManifestSHA256:  stored.ArtifactManifestSHA256,
		SourceVersionID:         stored.SourceVersionID, OccurrenceID: stored.OccurrenceID, Page: stored.Page,
		ArtifactID: stored.ArtifactID, ArtifactSHA256: stored.ArtifactSHA256,
		ArtifactPath: stored.ArtifactPath, Volume: stored.Volume,
	}, exact.Items[0])
	ranged, err := embedded.ProductionNumbers(t.Context(), docbank.ProductionNumberQuery{
		NamespaceID: allocation.NamespaceID, StartSequence: allocation.StartSequence,
		EndSequence: allocation.EndSequence, Limit: 1,
	})
	require.NoError(t, err)
	require.Len(t, ranged.Items, 1)
	require.Equal(t, label, ranged.Items[0].Label)
	require.Equal(t, allocation.StartSequence, ranged.NextSequence)
	continued, err := embedded.ProductionNumbers(t.Context(), docbank.ProductionNumberQuery{
		NamespaceID: allocation.NamespaceID, StartSequence: allocation.StartSequence,
		EndSequence: allocation.EndSequence, AfterSequence: ranged.NextSequence, Limit: 1,
	})
	require.NoError(t, err)
	require.Len(t, continued.Items, 1)
	require.Equal(t, plan.Reservation.Numbers[1].Text, continued.Items[0].Label)
	prefix := label[:len(label)-1]
	candidates, err := embedded.ProductionNumberCandidates(t.Context(), prefix, 1)
	require.NoError(t, err)
	require.Equal(t, "prefix", candidates.MatchKind)
	require.Len(t, candidates.Items, 1)
	require.True(t, candidates.Ambiguous)
	require.True(t, candidates.Truncated)
	_, err = embedded.ProductionNumbers(t.Context(), docbank.ProductionNumberQuery{Label: label,
		NamespaceID: allocation.NamespaceID})
	require.ErrorIs(t, err, store.ErrInvalidBatesSelector)
	_, err = embedded.ProductionNumberCandidates(t.Context(), strings.Repeat("x", 257), 1)
	require.ErrorIs(t, err, store.ErrInvalidBatesSelector)
	separate, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, separate.Close()) })
	_, err = separate.ProductionNumbers(t.Context(), docbank.ProductionNumberQuery{Label: label})
	require.ErrorIs(t, err, store.ErrNotFound)
}
