package production

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfproduction"
)

const (
	productionJobTimeout    = 30 * time.Minute
	productionPageTimeout   = 60 * time.Second
	productionLeaseDuration = 2 * time.Minute
	productionRenewInterval = 30 * time.Second
	productionIdleDelay     = time.Second
)

// LifecycleStore supplies only stored finalized, job, plan, and claim
// authority. Physical blob reads and writes remain in the verified adapters.
type LifecycleStore interface {
	ProductionFinalPublicationStore
	LoadFinalizedProduction(ctx context.Context, setID string, revision int64) (FinalizedProduction, error)
	AdmitProductionJob(ctx context.Context, request JobRequest) (Job, error)
	ReserveProductionJobNumbers(ctx context.Context, job Job, finalized FinalizedProduction) (documentproduction.NumberReservation, error)
	CheckpointProductionRenderPlan(ctx context.Context, job Job, plan RenderPlan) (RenderPlan, error)
	ClaimProductionRenderStage(ctx context.Context, jobID, worker string, lease time.Duration) (JobClaim, error)
	RenewProductionJobClaim(ctx context.Context, claim JobClaim, lease time.Duration) (JobClaim, error)
	LoadProductionRecipeForJob(ctx context.Context, jobID string) (redaction.Recipe, error)
	NextRunnableProductionJob(ctx context.Context) (JobRequest, bool, error)
}

type ProductionPageArchive interface {
	ProductionPageStager
	ProductionPageHandleStore
}

// Worker owns one production job at a time for its vault. The daemon registers
// Run under its supervisor, but no production route submits work until the
// separate lifecycle and retention boundary is ready.
type Worker struct {
	Store         LifecycleStore
	Source        ProductionPDFSourceOpener
	Pages         ProductionPageArchive
	Artifacts     ProductionFinalArtifactStore
	WorkerID      string
	EngineFactory func(redaction.Recipe) (pdfproduction.Engine, error)

	mu            sync.Mutex
	jobTimeout    time.Duration
	pageTimeout   time.Duration
	leaseDuration time.Duration
	renewInterval time.Duration
	idleDelay     time.Duration
}

func (w *Worker) valid() bool {
	return w != nil && w.Store != nil && w.Source != nil && w.Pages != nil && w.Artifacts != nil && w.WorkerID != ""
}

func (w *Worker) Run(ctx context.Context) error {
	if !w.valid() || ctx == nil {
		return ErrJobConflict
	}
	for {
		processed, err := w.RunOne(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !errors.Is(err, ErrJobStaleClaim) && !errors.Is(err, context.Canceled) &&
				!errors.Is(err, context.DeadlineExceeded) {
				return err
			}
		}
		if processed && err == nil {
			continue
		}
		delay := w.idleDelay
		if delay <= 0 {
			delay = productionIdleDelay
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// RunOne scans only queued or expired work. The mutex also serializes callers
// that invoke RunJob directly, enforcing one rendering job per vault worker.
func (w *Worker) RunOne(ctx context.Context) (bool, error) {
	if !w.valid() || ctx == nil {
		return false, ErrJobConflict
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	request, found, err := w.Store.NextRunnableProductionJob(ctx)
	if err != nil || !found {
		return false, err
	}
	_, err = w.runJob(ctx, request)
	return true, err
}

// RunJob is the same path used by restart and internal fault tests. A
// successful historical retry reopens every artifact without another claim or
// number reservation.
func (w *Worker) RunJob(ctx context.Context, request JobRequest) (Job, error) {
	if !w.valid() || ctx == nil {
		return Job{}, ErrJobConflict
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.runJob(ctx, request)
}

func (w *Worker) runJob(ctx context.Context, request JobRequest) (Job, error) {
	limit := w.jobTimeout
	if limit <= 0 || limit > productionJobTimeout {
		limit = productionJobTimeout
	}
	jobCtx, stopJob := context.WithTimeout(ctx, limit)
	defer stopJob()
	finalized, err := w.Store.LoadFinalizedProduction(jobCtx, request.SetID, request.Revision)
	if err != nil {
		return Job{}, err
	}
	if finalized.Draft.State != "finalized" || finalized.Draft.SetID != request.SetID ||
		finalized.Draft.Revision != request.Revision || finalized.Draft.ETag != request.ETag ||
		finalized.Authority.Receipt == nil || finalized.Authority.Prepared.SHA256 != request.RevisionSHA256 ||
		finalized.Authority.Receipt.SHA256 != request.PreparedInputSHA256 ||
		documentproduction.ValidatePreparedInputAuthority(finalized.Authority) != nil {
		return Job{}, ErrJobConflict
	}
	job, err := w.Store.AdmitProductionJob(jobCtx, request)
	if err != nil {
		return Job{}, fmt.Errorf("admit production job: %w", err)
	}
	if job.ID != request.JobID || job.SetID != request.SetID || job.Revision != request.Revision ||
		job.RevisionSHA256 != request.RevisionSHA256 || job.PreparedInputSHA256 != request.PreparedInputSHA256 {
		return Job{}, ErrJobConflict
	}
	recipe, err := w.Store.LoadProductionRecipeForJob(jobCtx, job.ID)
	if err != nil {
		return Job{}, err
	}
	recipeRaw, err := canonical.Marshal(recipe)
	if err != nil || hashProductionStageBytes(recipeRaw) != finalized.Authority.Prepared.RecipeSHA256 {
		return Job{}, ErrJobConflict
	}
	if job.State == ProductionJobSucceeded {
		plan, err := w.Store.LoadProductionRenderPlan(jobCtx, job.ID)
		if err != nil {
			return Job{}, err
		}
		return PublishVerifiedProductionJob(jobCtx, w.Store, w.Pages, w.Artifacts,
			JobClaim{JobID: job.ID}, job, finalized, plan, recipe)
	}
	if job.State != ProductionJobQueued && job.State != ProductionJobRunning {
		return Job{}, ErrJobConflict
	}
	reservation, err := w.Store.ReserveProductionJobNumbers(jobCtx, job, finalized)
	if err != nil {
		return Job{}, fmt.Errorf("reserve production numbers: %w", err)
	}
	plan, err := BuildProductionRenderPlan(job, finalized, reservation, recipe)
	if err != nil {
		return Job{}, err
	}
	plan, err = w.Store.CheckpointProductionRenderPlan(jobCtx, job, plan)
	if err != nil {
		return Job{}, fmt.Errorf("checkpoint production render plan: %w", err)
	}
	lease := w.leaseDuration
	if lease <= 0 {
		lease = productionLeaseDuration
	}
	claim, err := w.Store.ClaimProductionRenderStage(jobCtx, job.ID, w.WorkerID, lease)
	if err != nil {
		return Job{}, fmt.Errorf("claim production render stage: %w", err)
	}
	workCtx, stopWork := context.WithCancel(jobCtx)
	renewed := make(chan error, 1)
	renewDone := make(chan struct{})
	interval := w.renewInterval
	if interval <= 0 {
		interval = productionRenewInterval
	}
	go func() {
		defer close(renewDone)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-workCtx.Done():
				return
			case <-ticker.C:
				_, err := w.Store.RenewProductionJobClaim(workCtx, claim, lease)
				if err != nil {
					renewed <- err
					stopWork()
					return
				}
			}
		}
	}()
	defer func() { stopWork(); <-renewDone }()
	claimErr := func(err error) error {
		if jobCtx.Err() != nil {
			return jobCtx.Err()
		}
		select {
		case lost := <-renewed:
			return errors.Join(ErrJobStaleClaim, lost)
		default:
		}
		return err
	}
	engineFactory := w.EngineFactory
	if engineFactory == nil {
		engineFactory = pdfproduction.NewPDFium
	}
	engine, err := engineFactory(recipe)
	if err != nil {
		return Job{}, claimErr(err)
	}
	pageLimit := w.pageTimeout
	if pageLimit <= 0 || pageLimit > productionPageTimeout {
		pageLimit = productionPageTimeout
	}
	renderErr := renderProductionPagesWithTimeout(workCtx, w.Source, w.Pages, engine,
		claim, job, finalized, plan, recipe, pageLimit)
	if err := errors.Join(renderErr, engine.Close()); err != nil {
		return Job{}, claimErr(err)
	}
	if err := claimErr(workCtx.Err()); err != nil {
		return Job{}, err
	}
	result, err := PublishVerifiedProductionJob(workCtx, w.Store, w.Pages, w.Artifacts,
		claim, job, finalized, plan, recipe)
	if err != nil {
		return Job{}, claimErr(err)
	}
	return result, nil
}
