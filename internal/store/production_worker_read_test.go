package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/pdfproduction"
)

func TestProductionWorkerQueueResumesOnlyExpiredClaims(t *testing.T) {
	s, finalized, job, _ := finalizedProductionCheckpointFixture(t)
	request, found, err := s.NextRunnableProductionJob(t.Context())
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, job.ID, request.JobID)
	require.Equal(t, finalized.Authority.Prepared.SHA256, request.RevisionSHA256)
	recipe, err := s.LoadProductionRecipeForJob(t.Context(), job.ID)
	require.NoError(t, err)
	require.Equal(t, pdfproduction.QualifiedRecipe(), recipe)

	_, err = s.ClaimProductionJob(t.Context(), job.ID, "synthetic-worker", time.Hour)
	require.NoError(t, err)
	_, found, err = s.NextRunnableProductionJob(t.Context())
	require.NoError(t, err)
	require.False(t, found)
	_, err = s.db.Exec(`UPDATE production_jobs SET lease_expires_at=? WHERE job_id=?`,
		"2020-01-01T00:00:00Z", job.ID)
	require.NoError(t, err)
	replayed, found, err := s.NextRunnableProductionJob(t.Context())
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, request, replayed)
	reopened, err := Open(s.path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	restarted, found, err := reopened.NextRunnableProductionJob(t.Context())
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, request, restarted)
	require.NoError(t, s.CancelProductionJob(t.Context(), job.ID))
	_, found, err = s.NextRunnableProductionJob(t.Context())
	require.NoError(t, err)
	require.False(t, found)
}

func TestProductionWorkerQueueRejectsOversizedStoredRequestBeforeDecode(t *testing.T) {
	s, _, job, _ := finalizedProductionCheckpointFixture(t)
	_, err := s.db.Exec(`DROP TRIGGER production_jobs_immutable_identity`)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE production_jobs SET request_json=zeroblob(?) WHERE job_id=?`,
		maxProductionJobRequestBytes+1, job.ID)
	require.NoError(t, err)
	_, found, err := s.NextRunnableProductionJob(t.Context())
	require.False(t, found)
	require.Error(t, err)
}
