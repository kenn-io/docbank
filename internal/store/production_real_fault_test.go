package store

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/production"
)

var errSyntheticRendererFailure = errors.New("synthetic renderer failure")

type failSecondProductionPage struct{ rendered int }

func (e *failSecondProductionPage) Render(ctx context.Context, source pdfproduction.Source,
	page redaction.Page, recipe redaction.Recipe) (pdfproduction.Raster, error) {
	e.rendered++
	if e.rendered == 2 {
		return pdfproduction.Raster{}, errSyntheticRendererFailure
	}
	return (restartWhiteEngine{}).Render(ctx, source, page, recipe)
}

func (*failSecondProductionPage) Close() error { return nil }

type cancelAfterProductionPage struct {
	*realRestartFixture

	canceled bool
}

func (a *cancelAfterProductionPage) StageProductionPage(ctx context.Context, claim production.JobClaim,
	job production.Job, plan production.RenderPlan, member documentproduction.PreparedMember,
	page production.RenderPagePlan, candidate production.ProductionPageCandidate) (production.ProductionPageStage, error) {
	stage, err := a.realRestartFixture.StageProductionPage(ctx, claim, job, plan, member, page, candidate)
	if err == nil && !a.canceled {
		a.canceled = true
		err = a.CancelProductionJob(ctx, job.ID)
	}
	return stage, err
}

func realFaultFixture(t *testing.T) (*realRestartFixture, production.JobRequest) {
	t.Helper()
	s, finalized, job := unreservedProductionCheckpointFixture(t)
	f := &realRestartFixture{Store: s, root: filepath.Dir(s.path)}
	var err error
	f.blobs, err = openRestartBlobs(s, f.root)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, f.blobs.Close()) })
	written, err := f.write(t.Context(), bytes.NewReader([]byte("synthetic derived PDF")))
	require.NoError(t, err)
	require.Equal(t, finalized.Authority.Prepared.Members[0].Member.PDFSHA256, written.Hash)
	request := production.JobRequest{JobID: job.ID, OperationID: job.OperationID, SetID: job.SetID,
		Revision: job.Revision, ETag: job.ETag, RevisionSHA256: job.RevisionSHA256,
		PreparedInputSHA256:    job.PreparedInputSHA256,
		NumberingProfileSHA256: finalized.Draft.NumberingRecipeSHA256}
	return f, request
}

func requireUnpublishedProductionFault(t *testing.T, f *realRestartFixture, jobID string) {
	t.Helper()
	stored, err := f.LoadProductionJob(t.Context(), jobID)
	require.NoError(t, err)
	require.NotEqual(t, production.ProductionJobSucceeded, stored.State)
	require.Empty(t, stored.Receipt.SHA256)
	require.Empty(t, stored.Manifest.Artifacts)
	var allocations int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM bates_allocations WHERE operation_id=?`, jobID).Scan(&allocations))
	require.Equal(t, 1, allocations)
}

func TestProductionRealStoreRendererFailureThenTakeover(t *testing.T) {
	f, request := realFaultFixture(t)
	engine := &failSecondProductionPage{}
	firstWorker := f.worker()
	firstWorker.WorkerID = "first-worker"
	firstWorker.EngineFactory = func(redaction.Recipe) (pdfproduction.Engine, error) { return engine, nil }
	_, err := firstWorker.RunJob(t.Context(), request)
	require.ErrorIs(t, err, errSyntheticRendererFailure)
	require.Equal(t, 2, engine.rendered)
	require.Equal(t, 1, f.pageWrites)
	requireUnpublishedProductionFault(t, f, request.JobID)

	var old production.JobClaim
	old.JobID = request.JobID
	require.NoError(t, f.db.QueryRow(`SELECT claim_epoch,claim_token,claim_owner FROM production_jobs WHERE job_id=?`,
		request.JobID).Scan(&old.Epoch, &old.Token, &old.Worker))
	require.Equal(t, "first-worker", old.Worker)
	secondWorker := f.worker()
	secondWorker.WorkerID = "takeover-worker"
	_, err = secondWorker.RunJob(t.Context(), request)
	require.ErrorIs(t, err, production.ErrJobStaleClaim, "a live claim blocks takeover")
	requireUnpublishedProductionFault(t, f, request.JobID)
	_, err = f.db.Exec(`UPDATE production_jobs SET lease_expires_at=? WHERE job_id=?`,
		time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), request.JobID)
	require.NoError(t, err)
	f.reopen(t)
	result, err := secondWorker.RunJob(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, production.ProductionJobSucceeded, result.State)
	require.Equal(t, 2, f.pageWrites, "takeover reuses the first verified page")
	require.NotEmpty(t, result.Receipt.SHA256)
	require.Equal(t, request.JobID, result.Receipt.JobID)
	require.Equal(t, request.RevisionSHA256, result.Receipt.RevisionSHA256)
	require.Equal(t, result.Reservation.SHA256, result.Receipt.NumberReservationSHA256)
	require.Equal(t, result.Manifest.SHA256, result.Receipt.ArtifactManifestSHA256)
	require.Equal(t, productionEndorsementDigest(result.Endorsements), result.Receipt.EndorsementsSHA256)
	plan, err := f.LoadProductionRenderPlan(t.Context(), request.JobID)
	require.NoError(t, err)
	require.Equal(t, plan.LayoutSHA256, result.Receipt.LayoutSHA256)
	var epoch int64
	var owner string
	require.NoError(t, f.db.QueryRow(`SELECT claim_epoch,claim_owner FROM production_jobs WHERE job_id=?`,
		request.JobID).Scan(&epoch, &owner))
	require.Greater(t, epoch, old.Epoch)
	require.Equal(t, "takeover-worker", owner)
	_, err = f.RenewProductionJobClaim(t.Context(), old, time.Minute)
	require.ErrorIs(t, err, production.ErrJobStaleClaim)
	var allocations int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM bates_allocations WHERE operation_id=?`, request.JobID).Scan(&allocations))
	require.Equal(t, 1, allocations)
	f.reopen(t)
	replayed, err := f.worker().RunJob(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, result.Receipt, replayed.Receipt)
	require.Equal(t, result.Reservation, replayed.Reservation)
	changed := request
	changed.NumberingProfileSHA256 = productionHash("different profile")
	_, err = f.worker().RunJob(t.Context(), changed)
	require.ErrorIs(t, err, ErrPackageConflict)
}

func TestProductionRealStoreCancellationAfterPageStage(t *testing.T) {
	f, request := realFaultFixture(t)
	archive := &cancelAfterProductionPage{realRestartFixture: f}
	worker := f.worker()
	worker.Pages = archive
	_, err := worker.RunJob(t.Context(), request)
	require.Error(t, err)
	require.True(t, archive.canceled)
	require.Equal(t, 1, f.pageWrites)
	requireUnpublishedProductionFault(t, f, request.JobID)
	stored, err := f.LoadProductionJob(t.Context(), request.JobID)
	require.NoError(t, err)
	require.Equal(t, production.ProductionJobCanceled, stored.State)
	f.reopen(t)
	_, err = f.worker().RunJob(t.Context(), request)
	require.ErrorIs(t, err, production.ErrJobConflict)
	requireUnpublishedProductionFault(t, f, request.JobID)
}
