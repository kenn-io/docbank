package store

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/production"
)

func TestPublishedProductionPackageInputsSurviveSourceHeadChange(t *testing.T) {
	s, claim, job, receipt, manifest, endorsements := stagedProductionPublicationFixture(t)
	finalized, err := s.LoadFinalizedProduction(t.Context(), job.SetID, job.Revision)
	require.NoError(t, err)
	_, err = s.PublishProductionJob(t.Context(), claim, job, receipt, manifest, endorsements)
	require.NoError(t, err)
	before, err := s.LoadProductionPackageInputs(t.Context(), job.ID)
	require.NoError(t, err)
	require.Equal(t, production.ProductionJobSucceeded, before.Job.State)
	require.Len(t, before.Members, 2)
	require.NotEqual(t, before.Members[0].ID, before.Members[1].ID)
	require.Equal(t, before.Members[0].FamilyID, before.Members[1].FamilyID)
	require.Equal(t, int64(1), before.Members[0].Ordinal)
	require.Equal(t, int64(2), before.Members[1].Ordinal)
	require.Equal(t, receipt.NumberReservationSHA256, before.Reservation.SHA256)
	projection, err := production.PlanPackageProjection(before.Job, before.Reservation, before.Members,
		"export-dat-opt-images-v1", production.PackageLimits{MaxVolumeBytes: 1 << 30, MaxVolumeDocuments: 100})
	require.NoError(t, err)
	require.Len(t, projection.Manifest.Documents, 2)

	member := finalized.Authority.Prepared.Members[0].Member
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		const newVersion = "76000000-0000-4000-8000-000000000004"
		_, err := tx.ExecContext(t.Context(), `INSERT INTO content_versions(version_id,node_id,blob_hash,size,recorded_at,node_revision,introduced_operation_id,transition_kind)
			SELECT ?,node_id,blob_hash,size,?,node_revision+1,?,'content_replace' FROM content_versions WHERE version_id=?`,
			newVersion, nowRFC3339(), "76000000-0000-4000-8000-000000000005", member.SourceVersionID)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(t.Context(), `UPDATE nodes SET current_version_id=?,revision=revision+1 WHERE id=?`, newVersion, member.NodeID)
		return err
	}))
	_, err = s.LoadFinalizedProduction(t.Context(), job.SetID, job.Revision)
	require.ErrorIs(t, err, ErrInvalidProduction)
	after, err := s.LoadProductionPackageInputs(t.Context(), job.ID)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestPublishedProductionPackageInputsRejectTamperedFrozenAuthority(t *testing.T) {
	s, claim, job, receipt, manifest, endorsements := stagedProductionPublicationFixture(t)
	_, err := s.PublishProductionJob(t.Context(), claim, job, receipt, manifest, endorsements)
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `DROP TRIGGER production_finalized_revisions_immutable_update`)
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `UPDATE production_finalized_revisions SET prepared_input_json='{}' WHERE set_id=? AND revision=?`, job.SetID, job.Revision)
	require.NoError(t, err)
	_, err = s.LoadProductionPackageInputs(t.Context(), job.ID)
	require.ErrorIs(t, err, production.ErrJobConflict)
}
