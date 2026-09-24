package store

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
)

func TestFindPublishedProductionNumberBindsFrozenSourceAndFinalArtifact(t *testing.T) {
	f, job := publishedRealRetentionFixture(t)
	plan, err := f.LoadProductionRenderPlan(t.Context(), job.ID)
	require.NoError(t, err)
	number := plan.Reservation.Numbers[0]

	match, err := f.FindPublishedProductionNumber(t.Context(), number.Text)
	require.NoError(t, err)
	require.Equal(t, job.ID, match.JobID)
	require.Equal(t, job.SetID, match.SetID)
	require.Equal(t, number.MemberID, match.OccurrenceID)
	require.Equal(t, number.Page, match.Page)
	require.Equal(t, job.Manifest.SHA256, match.ArtifactManifestSHA256)
	require.NotEmpty(t, match.SourceVersionID)
	require.NotEmpty(t, match.ArtifactID)
	require.NotEmpty(t, match.ArtifactSHA256)
	require.NotEmpty(t, match.Volume)
	artifact, err := f.LoadProductionJobArtifact(t.Context(), job.ID, match.ArtifactID)
	require.NoError(t, err)
	require.Equal(t, documentproduction.ArtifactRoleRedactedPDF, artifact.Role)
	require.Equal(t, artifact.Volume, match.Volume)
	require.Equal(t, artifact.SHA256, match.ArtifactSHA256)
	finalized, err := f.LoadFinalizedProduction(t.Context(), job.SetID, job.Revision)
	require.NoError(t, err)
	source := finalized.Authority.Prepared.Members[0].Member
	require.Equal(t, source.SourceVersionID, match.SourceVersionID)
	const newVersion = "76000000-0000-4000-8000-000000000098"
	_, err = f.db.Exec(`INSERT INTO content_versions(version_id,node_id,blob_hash,size,recorded_at,node_revision,introduced_operation_id,transition_kind)
		SELECT ?,node_id,blob_hash,size,?,node_revision+1,?,'content_replace' FROM content_versions WHERE version_id=?`,
		newVersion, nowRFC3339(), "76000000-0000-4000-8000-000000000099", source.SourceVersionID)
	require.NoError(t, err)
	_, err = f.db.Exec(`UPDATE nodes SET current_version_id=?,revision=revision+1 WHERE id=?`, newVersion, source.NodeID)
	require.NoError(t, err)
	matchAfterDrift, err := f.FindPublishedProductionNumber(t.Context(), number.Text)
	require.NoError(t, err)
	require.Equal(t, match, matchAfterDrift, "lookup must keep the published source version after head drift")

	_, err = f.FindPublishedProductionNumber(t.Context(), "NOT-A-PUBLISHED-NUMBER")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestPublishedProductionNumberCandidatesRankAndBoundFallback(t *testing.T) {
	f, job := publishedRealRetentionFixture(t)
	allocation, err := f.ProductionNumberingForJob(t.Context(), job.ID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(allocation.Labels), 2)
	firstLabel := allocation.Labels[0].Label
	prefix := firstLabel[:len(firstLabel)-1]
	require.True(t, strings.HasPrefix(allocation.Labels[1].Label, prefix))

	exact, err := f.FindPublishedProductionNumberCandidates(t.Context(), firstLabel, 25)
	require.NoError(t, err)
	require.Equal(t, "exact", exact.MatchKind)
	require.Len(t, exact.Items, 1)
	require.False(t, exact.Ambiguous)
	require.False(t, exact.Truncated)
	require.Equal(t, firstLabel, exact.Items[0].Label)
	require.Equal(t, job.ID, exact.Items[0].JobID)

	partial, err := f.FindPublishedProductionNumberCandidates(t.Context(), prefix, 1)
	require.NoError(t, err)
	require.Equal(t, "prefix", partial.MatchKind)
	require.Len(t, partial.Items, 1)
	require.True(t, partial.Ambiguous)
	require.True(t, partial.Truncated)
	require.Equal(t, firstLabel, partial.Items[0].Label)
	require.NotEmpty(t, partial.Items[0].ArtifactID)

	substring, err := f.FindPublishedProductionNumberCandidates(t.Context(), prefix[1:], 25)
	require.NoError(t, err)
	require.Equal(t, "substring", substring.MatchKind)
	require.Len(t, substring.Items, 2)
	require.True(t, substring.Ambiguous)
	require.False(t, substring.Truncated)
	require.NotEqual(t, substring.Items[0].OccurrenceID, substring.Items[1].OccurrenceID)

	missing, err := f.FindPublishedProductionNumberCandidates(t.Context(), "UNPUBLISHED-NUMBER", 25)
	require.NoError(t, err)
	require.Equal(t, "none", missing.MatchKind)
	require.Empty(t, missing.Items)
	_, err = f.FindPublishedProductionNumberCandidates(t.Context(), "", 25)
	require.ErrorIs(t, err, ErrInvalidBatesSelector)
	_, err = f.FindPublishedProductionNumberCandidates(t.Context(), prefix, 26)
	require.ErrorIs(t, err, ErrInvalidBatesSelector)
	_, err = f.FindPublishedProductionNumberCandidates(t.Context(), "%", 25)
	require.NoError(t, err, "wildcards are literal text, never a LIKE pattern")
}

func TestPublishedProductionNumberMatchesLedgerPageOrdinal(t *testing.T) {
	number := documentproduction.AssignedNumber{MemberID: "75000000-0000-4000-8000-000000000010",
		MemberOrdinal: 1, Page: 2, Text: "PLAN000002"}
	ledger := BatesPageLabel{Ordinal: 2, OccurrenceID: number.MemberID,
		SourcePage: number.Page, Label: number.Text}
	require.True(t, matchesPublishedProductionNumberLedger(ledger, number, 2),
		"page ordinal advances within a document while member ordinal stays fixed")
	require.False(t, matchesPublishedProductionNumberLedger(ledger, number, 1))
}

func TestFindPublishedProductionNumberRangePagesInLedgerOrder(t *testing.T) {
	f, job := publishedRealRetentionFixture(t)
	allocation, err := f.ProductionNumberingForJob(t.Context(), job.ID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(allocation.Labels), 2)
	first, err := f.FindPublishedProductionNumberRange(t.Context(), allocation.NamespaceID,
		allocation.StartSequence, allocation.EndSequence, 0, 1)
	require.NoError(t, err)
	require.Len(t, first.Items, 1)
	require.Equal(t, allocation.Labels[0].Label, first.Items[0].Label)
	require.Equal(t, allocation.StartSequence, first.NextSequence)
	second, err := f.FindPublishedProductionNumberRange(t.Context(), allocation.NamespaceID,
		allocation.StartSequence, allocation.EndSequence, first.NextSequence, 1)
	require.NoError(t, err)
	require.Len(t, second.Items, 1)
	require.Equal(t, allocation.Labels[1].Label, second.Items[0].Label)
	require.Zero(t, second.NextSequence)
	require.NotEqual(t, first.Items[0].OccurrenceID, second.Items[0].OccurrenceID)
	_, err = f.FindPublishedProductionNumberRange(t.Context(), allocation.NamespaceID,
		allocation.EndSequence, allocation.StartSequence, 0, 1)
	require.ErrorIs(t, err, ErrInvalidBatesSelector)
	_, err = f.FindPublishedProductionNumberRange(t.Context(), allocation.NamespaceID,
		allocation.StartSequence, allocation.EndSequence, 0, 26)
	require.ErrorIs(t, err, ErrInvalidBatesSelector)
}
