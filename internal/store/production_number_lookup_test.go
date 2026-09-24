package store

import (
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

func TestPublishedProductionNumberMatchesLedgerPageOrdinal(t *testing.T) {
	number := documentproduction.AssignedNumber{MemberID: "75000000-0000-4000-8000-000000000010",
		MemberOrdinal: 1, Page: 2, Text: "PLAN000002"}
	ledger := BatesPageLabel{Ordinal: 2, OccurrenceID: number.MemberID,
		SourcePage: number.Page, Label: number.Text}
	require.True(t, matchesPublishedProductionNumberLedger(ledger, number, 2),
		"page ordinal advances within a document while member ordinal stays fixed")
	require.False(t, matchesPublishedProductionNumberLedger(ledger, number, 1))
}
