package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/production"
)

// A deterministic render failure must leave durable terminal state, so the
// supervisor can select the next queued job after a real store restart.
func TestProductionWorkerPersistsRenderFailureAndAdvancesQueue(t *testing.T) {
	s, finalized, first := unreservedProductionCheckpointFixture(t)
	f := &realRestartFixture{Store: s}
	request := production.JobRequest{JobID: first.ID, OperationID: first.OperationID,
		SetID: first.SetID, Revision: first.Revision, ETag: first.ETag,
		RevisionSHA256: first.RevisionSHA256, PreparedInputSHA256: first.PreparedInputSHA256,
		NumberingProfileSHA256: finalized.Draft.NumberingRecipeSHA256}
	second := request
	second.JobID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	second.OperationID = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	_, err := s.AdmitProductionJob(t.Context(), second)
	require.NoError(t, err)
	worker := f.worker()
	engineCalls := 0
	worker.EngineFactory = func(redaction.Recipe) (pdfproduction.Engine, error) {
		engineCalls++
		return nil, production.ErrJobConflict
	}
	processed, err := worker.RunOne(t.Context())
	require.Equal(t, 1, engineCalls)
	require.NoError(t, err)
	require.True(t, processed)
	stored, err := s.LoadProductionJob(t.Context(), first.ID)
	require.NoError(t, err)
	require.Equal(t, production.ProductionJobFailed, stored.State)
	require.Empty(t, stored.Receipt.SHA256)

	reopened, err := Open(s.path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	next, found, err := reopened.NextRunnableProductionJob(t.Context())
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, second.JobID, next.JobID)
}

func TestProductionSupervisorContinuesToLaterStoredJob(t *testing.T) {
	s, finalized, first := unreservedProductionCheckpointFixture(t)
	request := production.JobRequest{JobID: first.ID, OperationID: first.OperationID,
		SetID: first.SetID, Revision: first.Revision, ETag: first.ETag,
		RevisionSHA256: first.RevisionSHA256, PreparedInputSHA256: first.PreparedInputSHA256,
		NumberingProfileSHA256: finalized.Draft.NumberingRecipeSHA256}
	second := request
	second.JobID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	second.OperationID = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	_, err := s.AdmitProductionJob(t.Context(), second)
	require.NoError(t, err)
	f := &realRestartFixture{Store: s}
	worker := f.worker()
	worker.EngineFactory = func(redaction.Recipe) (pdfproduction.Engine, error) {
		return nil, production.ErrJobConflict
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	t.Cleanup(cancel)
	require.Eventually(t, func() bool {
		var epoch int64
		if err := s.db.QueryRow(`SELECT claim_epoch FROM production_jobs WHERE job_id=?`, second.JobID).Scan(&epoch); err != nil {
			return false
		}
		return epoch > 0
	}, 5*time.Second, 10*time.Millisecond, "later queued job was not attempted")
	select {
	case err := <-done:
		t.Fatalf("vault worker stopped after a job failure: %v", err)
	default:
	}
	firstStored, err := s.LoadProductionJob(t.Context(), first.ID)
	require.NoError(t, err)
	require.Equal(t, production.ProductionJobFailed, firstStored.State)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestProductionFailureRetriesAreDeferredAndBoundedAcrossRestart(t *testing.T) {
	s, request := productionJobFixture(t)
	_, err := s.AdmitProductionJob(t.Context(), request)
	require.NoError(t, err)
	for attempt := 1; attempt <= 3; attempt++ {
		require.NoError(t, s.RecordProductionJobFailure(t.Context(), request, production.JobClaim{}, false))
		job, err := s.LoadProductionJob(t.Context(), request.JobID)
		require.NoError(t, err)
		if attempt < 3 {
			require.Equal(t, production.ProductionJobQueued, job.State)
		} else {
			require.Equal(t, production.ProductionJobFailed, job.State)
		}
		var epoch int64
		require.NoError(t, s.db.QueryRow(`SELECT claim_epoch FROM production_jobs WHERE job_id=?`, request.JobID).Scan(&epoch))
		require.EqualValues(t, attempt, epoch)
		reopened, err := Open(s.path)
		require.NoError(t, err)
		_, found, err := reopened.NextRunnableProductionJob(t.Context())
		require.NoError(t, err)
		require.False(t, found, "failed or deferred job must not hot-loop after restart")
		require.NoError(t, reopened.Close())
		if attempt < 3 {
			_, err := s.db.Exec(`UPDATE production_jobs SET lease_expires_at=? WHERE job_id=?`,
				time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), request.JobID)
			require.NoError(t, err)
		}
	}
}

func TestProductionFailureCannotOverrideCanceledOrStaleClaims(t *testing.T) {
	s, request := productionJobFixture(t)
	job, err := s.AdmitProductionJob(t.Context(), request)
	require.NoError(t, err)
	claim, err := s.ClaimProductionJob(t.Context(), job.ID, "synthetic-worker", time.Minute)
	require.NoError(t, err)
	stale := claim
	stale.Token = "wrong token"
	require.ErrorIs(t, s.RecordProductionJobFailure(t.Context(), request, stale, true), production.ErrJobStaleClaim)
	require.NoError(t, s.CancelProductionJob(t.Context(), job.ID))
	require.ErrorIs(t, s.RecordProductionJobFailure(t.Context(), request, claim, true), production.ErrJobStaleClaim)
	stored, err := s.LoadProductionJob(t.Context(), job.ID)
	require.NoError(t, err)
	require.Equal(t, production.ProductionJobCanceled, stored.State)
	require.Empty(t, stored.Receipt.SHA256)
}

func TestProductionFailureFencesFormerClaimBeforeRetry(t *testing.T) {
	s, request := productionJobFixture(t)
	job, err := s.AdmitProductionJob(t.Context(), request)
	require.NoError(t, err)
	claim, err := s.ClaimProductionJob(t.Context(), job.ID, "synthetic-worker", time.Minute)
	require.NoError(t, err)
	require.NoError(t, s.RecordProductionJobFailure(t.Context(), request, claim, false))
	_, err = s.RenewProductionJobClaim(t.Context(), claim, time.Minute)
	require.ErrorIs(t, err, production.ErrJobStaleClaim)
	_, err = s.ClaimProductionJob(t.Context(), job.ID, "synthetic-worker", time.Minute)
	require.ErrorIs(t, err, production.ErrJobStaleClaim)
	stored, err := s.LoadProductionJob(t.Context(), job.ID)
	require.NoError(t, err)
	require.Equal(t, production.ProductionJobQueued, stored.State)
	require.Empty(t, stored.Receipt.SHA256)
}
