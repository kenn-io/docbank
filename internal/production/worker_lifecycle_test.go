package production

import (
	"bytes"
	"context"
	"errors"
	"image"
	"strings"
	"testing"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/kit/packstore"
)

type lifecycleStoreFixture struct {
	*storedFixture

	job            Job
	plan           RenderPlan
	request        JobRequest
	checkpointErr  error
	claimErr       error
	claimAttempted chan struct{}
	reserveBlock   bool
	renewErr       error
	renewGate      chan struct{}
	lostResponse   bool
	claims         int
	publications   int
	listed         bool
	failures       int
	lastPermanent  bool
}

func (f *lifecycleStoreFixture) AdmitProductionJob(_ context.Context, request JobRequest) (Job, error) {
	if f.job.ID != "" {
		return f.job, nil
	}
	f.job = Job{ID: request.JobID, OperationID: request.OperationID, SetID: request.SetID,
		Revision: request.Revision, ETag: request.ETag, RevisionSHA256: request.RevisionSHA256,
		PreparedInputSHA256: request.PreparedInputSHA256, State: ProductionJobQueued}
	return f.job, nil
}
func (f *lifecycleStoreFixture) ReserveProductionJobNumbers(ctx context.Context, job Job,
	finalized FinalizedProduction) (documentproduction.NumberReservation, error) {
	f.reserved++
	if f.reserveBlock {
		<-ctx.Done()
		return documentproduction.NumberReservation{}, ctx.Err()
	}
	member := finalized.Authority.Prepared.Members[0].Member
	reservation := documentproduction.NumberReservation{Contract: documentproduction.NumberReservationContractV1,
		Authority: "bates-ledger/v1", ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		OperationID: job.ID, RevisionSHA256: job.RevisionSHA256, State: "reserved",
		Numbers: []documentproduction.AssignedNumber{{MemberID: member.ID, MemberOrdinal: member.Ordinal,
			Page: 1, Text: "SYN000001"}}}
	_, digest, err := documentproduction.CanonicalNumberReservation(reservation)
	reservation.SHA256 = digest
	return reservation, err
}
func (f *lifecycleStoreFixture) CheckpointProductionRenderPlan(_ context.Context, _ Job,
	plan RenderPlan) (RenderPlan, error) {
	if f.checkpointErr != nil {
		return RenderPlan{}, f.checkpointErr
	}
	if f.plan.SHA256 != "" && f.plan.SHA256 != plan.SHA256 {
		return RenderPlan{}, ErrJobConflict
	}
	f.plan = plan
	return plan, nil
}
func (f *lifecycleStoreFixture) ClaimProductionRenderStage(_ context.Context, jobID, worker string,
	lease time.Duration) (JobClaim, error) {
	f.claims++
	if f.claimAttempted != nil {
		close(f.claimAttempted)
	}
	if f.claimErr != nil {
		return JobClaim{}, f.claimErr
	}
	return JobClaim{JobID: jobID, Worker: worker, Token: "synthetic claim", Epoch: int64(f.claims),
		ExpiresAt: time.Now().Add(lease)}, nil
}
func (f *lifecycleStoreFixture) RenewProductionJobClaim(ctx context.Context, claim JobClaim,
	lease time.Duration) (JobClaim, error) {
	if f.renewGate != nil {
		select {
		case <-f.renewGate:
		case <-ctx.Done():
			return JobClaim{}, ctx.Err()
		}
	}
	if f.renewErr != nil {
		return JobClaim{}, f.renewErr
	}
	claim.ExpiresAt = time.Now().Add(lease)
	return claim, nil
}
func (f *lifecycleStoreFixture) LoadProductionJob(_ context.Context, _ string) (Job, error) {
	return f.job, nil
}
func (f *lifecycleStoreFixture) LoadProductionRenderPlan(_ context.Context, _ string) (RenderPlan, error) {
	return f.plan, nil
}
func (f *lifecycleStoreFixture) LoadProductionRecipeForJob(_ context.Context, _ string) (redaction.Recipe, error) {
	return pdfproduction.QualifiedRecipe(), nil
}
func (f *lifecycleStoreFixture) NextRunnableProductionJob(_ context.Context) (JobRequest, bool, error) {
	if f.listed {
		return JobRequest{}, false, nil
	}
	f.listed = true
	return f.request, true, nil
}
func (f *lifecycleStoreFixture) RecordProductionJobFailure(_ context.Context, _ JobRequest, _ JobClaim, permanent bool) error {
	f.failures++
	f.lastPermanent = permanent
	if permanent {
		f.job.State = ProductionJobFailed
	}
	return nil
}

func TestProductionWorkerJobDeadlineDefersButCallerCancellationDoesNotRecordFailure(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "job deadline", true: "caller cancellation"}[canceled], func(t *testing.T) {
			fixture := &lifecycleStoreFixture{storedFixture: newStoredFixtureWithText(t, strings.Repeat("A", 85)), reserveBlock: true}
			fixture.request = lifecycleRequest(fixture.finalized)
			worker := &Worker{Store: fixture, Source: &syntheticProductionSource{},
				Pages: &syntheticPageArchive{}, Artifacts: &syntheticFinalArtifacts{},
				WorkerID: "synthetic-worker", jobTimeout: 5 * time.Millisecond}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if canceled {
				cancel()
			}
			processed, err := worker.RunOne(ctx)
			require.True(t, processed)
			if canceled {
				require.ErrorIs(t, err, context.Canceled)
				require.Zero(t, fixture.failures)
			} else {
				require.NoError(t, err)
				require.Equal(t, 1, fixture.failures)
				require.False(t, fixture.lastPermanent)
			}
		})
	}
}
func (f *lifecycleStoreFixture) PublishProductionJob(_ context.Context, _ JobClaim, job Job,
	receipt documentproduction.ProductionReceipt, manifest documentproduction.ArtifactManifest,
	endorsements []redaction.Endorsement) (Job, error) {
	f.publications++
	job.State, job.Receipt, job.Manifest, job.Endorsements = ProductionJobSucceeded, receipt, manifest, endorsements
	f.job = job
	if f.lostResponse {
		return Job{}, errors.New("synthetic lost publication response")
	}
	return job, nil
}

type lifecycleFinalArtifacts struct {
	syntheticFinalArtifacts

	archive      *syntheticPageArchive
	failTextOnce bool
}

func (a *lifecycleFinalArtifacts) StageVerifiedProductionText(ctx context.Context, claim JobClaim,
	job Job, member documentproduction.PreparedMember, maxBytes int64) (documentproduction.Artifact, error) {
	if a.failTextOnce {
		a.failTextOnce = false
		return documentproduction.Artifact{}, ErrJobIncomplete
	}
	return a.syntheticFinalArtifacts.StageVerifiedProductionText(ctx, claim, job, member, maxBytes)
}

func (a *lifecycleFinalArtifacts) OpenVerifiedProductionArtifact(ctx context.Context, jobID string,
	artifact documentproduction.Artifact) (packstore.VerifiedReadCloser, int64, error) {
	if artifact.Role == documentproduction.ArtifactRoleRedactedPage {
		stage, err := a.archive.LoadProductionPageStage(ctx, jobID, artifact.MemberID, artifact.Page)
		if err != nil || stage.Artifact != artifact {
			return nil, 0, ErrJobConflict
		}
		return a.archive.OpenStagedProductionPage(ctx, stage)
	}
	return a.syntheticFinalArtifacts.OpenVerifiedProductionArtifact(ctx, jobID, artifact)
}

type waitingProductionSource struct{ started chan struct{} }

func (s waitingProductionSource) OpenPinnedProductionPDF(ctx context.Context, _ Job,
	_ documentproduction.PreparedMember) (PinnedProductionPDF, error) {
	close(s.started)
	<-ctx.Done()
	return PinnedProductionPDF{}, ctx.Err()
}

func syntheticLifecyclePDF(t *testing.T) []byte {
	t.Helper()
	pdf := fpdf.NewCustom(&fpdf.InitType{UnitStr: "pt", Size: fpdf.SizeType{Wd: 612, Ht: 72}})
	pdf.SetFont("Helvetica", "", 12)
	pdf.AddPage()
	pdf.Text(12, 36, "synthetic source")
	var output bytes.Buffer
	require.NoError(t, pdf.Output(&output))
	return output.Bytes()
}

func lifecycleRequest(finalized FinalizedProduction) JobRequest {
	return JobRequest{JobID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
		OperationID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", SetID: finalized.Draft.SetID,
		Revision: finalized.Draft.Revision, ETag: finalized.Draft.ETag,
		RevisionSHA256:         finalized.Authority.Prepared.SHA256,
		PreparedInputSHA256:    finalized.Authority.Receipt.SHA256,
		NumberingProfileSHA256: testHash("synthetic numbering profile")}
}

func TestProductionWorkerPublishesVerifiedPagesPDFAndTextThenReplays(t *testing.T) {
	pdf := syntheticLifecyclePDF(t)
	fixture := &lifecycleStoreFixture{storedFixture: newStoredFixtureWithSource(t, strings.Repeat("A", 85), pdf),
		lostResponse: true}
	fixture.request = lifecycleRequest(fixture.finalized)
	archive := &syntheticPageArchive{}
	artifacts := &lifecycleFinalArtifacts{archive: archive}
	source := &syntheticProductionSource{data: pdf}
	worker := &Worker{Store: fixture, Source: source, Pages: archive,
		Artifacts: artifacts, WorkerID: "synthetic-worker"}
	result, err := worker.RunJob(t.Context(), fixture.request)
	require.NoError(t, err)
	require.Equal(t, ProductionJobSucceeded, result.State)
	require.Len(t, archive.pages, 1)
	require.Equal(t, 1, fixture.reserved)
	require.Equal(t, 1, fixture.claims)
	require.Equal(t, 1, fixture.publications)
	require.Equal(t, 1, artifacts.writes)
	require.Equal(t, 1, artifacts.textWrites)
	require.Equal(t, 1, source.opens)
	require.Len(t, result.Manifest.Artifacts, 3)

	restarted := &Worker{Store: fixture, Source: source, Pages: archive,
		Artifacts: artifacts, WorkerID: "restarted-worker"}
	replayed, err := restarted.RunJob(t.Context(), fixture.request)
	require.NoError(t, err)
	require.Equal(t, result.Receipt, replayed.Receipt)
	require.Equal(t, 1, fixture.reserved, "historical replay cannot reserve another number")
	require.Equal(t, 1, fixture.claims, "historical replay cannot claim or rewrite")
	require.Equal(t, 1, fixture.publications)
	require.Equal(t, 1, source.opens)
}

func TestProductionWorkerRetriesSamePlanAfterFinalTextFailure(t *testing.T) {
	pdf := syntheticLifecyclePDF(t)
	fixture := &lifecycleStoreFixture{storedFixture: newStoredFixtureWithSource(t, strings.Repeat("A", 85), pdf)}
	request := lifecycleRequest(fixture.finalized)
	archive := &syntheticPageArchive{}
	artifacts := &lifecycleFinalArtifacts{archive: archive, failTextOnce: true}
	worker := &Worker{Store: fixture, Source: &syntheticProductionSource{data: pdf}, Pages: archive,
		Artifacts: artifacts, WorkerID: "synthetic-worker"}
	_, err := worker.RunJob(t.Context(), request)
	require.ErrorIs(t, err, ErrJobIncomplete)
	require.Len(t, archive.pages, 1)
	require.Equal(t, 1, artifacts.writes, "the verified PDF was staged before the fault")
	require.Zero(t, fixture.publications, "a page and PDF cannot be partial success")
	firstPlan := fixture.plan

	result, err := worker.RunJob(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, ProductionJobSucceeded, result.State)
	require.Equal(t, firstPlan, fixture.plan)
	require.Equal(t, firstPlan.Reservation.SHA256, result.Receipt.NumberReservationSHA256)
	require.Equal(t, 1, fixture.publications)
}

func TestProductionWorkerRefusesLiveTakeoverBeforeRendering(t *testing.T) {
	fixture := &lifecycleStoreFixture{storedFixture: newStoredFixtureWithText(t, strings.Repeat("A", 85)),
		claimErr: ErrJobStaleClaim}
	request := lifecycleRequest(fixture.finalized)
	source := &syntheticProductionSource{}
	worker := &Worker{Store: fixture, Source: source, Pages: &syntheticPageArchive{},
		Artifacts: &syntheticFinalArtifacts{}, WorkerID: "synthetic-worker"}
	_, err := worker.RunJob(t.Context(), request)
	require.ErrorIs(t, err, ErrJobStaleClaim)
	require.Equal(t, 1, fixture.reserved)
	require.NotEmpty(t, fixture.plan.SHA256)
	require.Equal(t, 1, fixture.claims)
	require.Zero(t, source.opens)
	require.Zero(t, fixture.publications)
}

func TestProductionSupervisorWorkerKeepsPollingAfterLostClaim(t *testing.T) {
	fixture := &lifecycleStoreFixture{storedFixture: newStoredFixtureWithText(t, strings.Repeat("A", 85)),
		claimErr: ErrJobStaleClaim, claimAttempted: make(chan struct{})}
	fixture.request = lifecycleRequest(fixture.finalized)
	worker := &Worker{Store: fixture, Source: &syntheticProductionSource{},
		Pages: &syntheticPageArchive{}, Artifacts: &syntheticFinalArtifacts{},
		WorkerID: "synthetic-worker", idleDelay: time.Millisecond}
	ctx, cancel := context.WithCancel(t.Context())
	out := make(chan error, 1)
	go func() { out <- worker.Run(ctx) }()
	select {
	case <-fixture.claimAttempted:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("worker did not attempt the claim")
	}
	select {
	case err := <-out:
		cancel()
		t.Fatalf("worker stopped after one stale claim: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	require.ErrorIs(t, <-out, context.Canceled)
	require.Zero(t, fixture.publications)
}

func TestProductionWorkerJobTimeoutStopsBeforeClaim(t *testing.T) {
	fixture := &lifecycleStoreFixture{storedFixture: newStoredFixtureWithText(t, strings.Repeat("A", 85)),
		reserveBlock: true}
	fixture.request = lifecycleRequest(fixture.finalized)
	worker := &Worker{Store: fixture, Source: &syntheticProductionSource{},
		Pages: &syntheticPageArchive{}, Artifacts: &syntheticFinalArtifacts{},
		WorkerID: "synthetic-worker", jobTimeout: 5 * time.Millisecond}
	_, err := worker.RunJob(t.Context(), fixture.request)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, 1, fixture.reserved)
	require.Zero(t, fixture.claims)
	require.Zero(t, fixture.publications)
}

func TestProductionWorkerLeaseLossAndCancellationCannotPublish(t *testing.T) {
	for _, scenario := range []string{"lease_loss", "cancellation"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := &lifecycleStoreFixture{storedFixture: newStoredFixtureWithText(t, strings.Repeat("A", 85))}
			fixture.request = lifecycleRequest(fixture.finalized)
			if scenario == "lease_loss" {
				fixture.renewErr = ErrJobStaleClaim
				fixture.renewGate = make(chan struct{})
			}
			source := waitingProductionSource{started: make(chan struct{})}
			archive := &syntheticPageArchive{}
			worker := &Worker{Store: fixture, Source: source, Pages: archive,
				Artifacts: &syntheticFinalArtifacts{}, WorkerID: "synthetic-worker",
				EngineFactory: func(redaction.Recipe) (pdfproduction.Engine, error) {
					return waitingProductionEngine{}, nil
				}, renewInterval: time.Millisecond}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			out := make(chan error, 1)
			go func() { _, err := worker.RunJob(ctx, fixture.request); out <- err }()
			select {
			case <-source.started:
			case <-time.After(5 * time.Second):
				t.Fatal("production source opener was not reached")
			}
			if scenario == "cancellation" {
				cancel()
			} else {
				close(fixture.renewGate)
			}
			select {
			case err := <-out:
				if scenario == "lease_loss" {
					require.ErrorIs(t, err, ErrJobStaleClaim)
				} else {
					require.ErrorIs(t, err, context.Canceled)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("production worker did not stop")
			}
			require.Equal(t, 1, fixture.reserved)
			require.Equal(t, 1, fixture.claims)
			require.Empty(t, archive.pages)
			require.Zero(t, fixture.publications)
		})
	}
}

type waitingProductionEngine struct{}

func (waitingProductionEngine) Render(ctx context.Context, _ pdfproduction.Source, _ redaction.Page,
	_ redaction.Recipe) (pdfproduction.Raster, error) {
	<-ctx.Done()
	return pdfproduction.Raster{}, ctx.Err()
}
func (waitingProductionEngine) Close() error { return nil }

type whiteProductionEngine struct{}

func (whiteProductionEngine) Render(ctx context.Context, _ pdfproduction.Source, page redaction.Page,
	recipe redaction.Recipe) (pdfproduction.Raster, error) {
	if err := ctx.Err(); err != nil {
		return pdfproduction.Raster{}, err
	}
	width := int((page.Width*int64(recipe.DPI) + 9_999) / 10_000)
	height := int((page.Height*int64(recipe.DPI) + 9_999) / 10_000)
	return pdfproduction.Raster{Pixels: image.NewNRGBA(image.Rect(0, 0, width, height)),
		Page: page, DPI: recipe.DPI}, nil
}

func (whiteProductionEngine) Close() error { return nil }

type renewalSignalStore struct {
	*lifecycleStoreFixture

	renewed chan struct{}
}

func (s *renewalSignalStore) RenewProductionJobClaim(ctx context.Context, claim JobClaim,
	lease time.Duration) (JobClaim, error) {
	next, err := s.lifecycleStoreFixture.RenewProductionJobClaim(ctx, claim, lease)
	if err == nil {
		select {
		case s.renewed <- struct{}{}:
		default:
		}
	}
	return next, err
}

type fenceStageArchive struct {
	*syntheticPageArchive

	onStage     func(context.Context, JobClaim) error
	stageClaim  JobClaim
	stageCtxErr error
	stageCalls  int
}

func (a *fenceStageArchive) StageProductionPage(ctx context.Context, claim JobClaim, job Job,
	plan RenderPlan, prepared documentproduction.PreparedMember, page RenderPagePlan,
	candidate ProductionPageCandidate) (ProductionPageStage, error) {
	a.stageCalls++
	a.stageClaim = claim
	if a.onStage != nil {
		if err := a.onStage(ctx, claim); err != nil {
			a.stageCtxErr = ctx.Err()
			return ProductionPageStage{}, err
		}
	}
	a.stageCtxErr = ctx.Err()
	return a.syntheticPageArchive.StageProductionPage(ctx, claim, job, plan, prepared, page, candidate)
}

func TestProductionWorkerStagesWithLiveContextAfterRenewal(t *testing.T) {
	pdf := syntheticLifecyclePDF(t)
	fixture := &lifecycleStoreFixture{storedFixture: newStoredFixtureWithSource(t, strings.Repeat("A", 85), pdf)}
	request := lifecycleRequest(fixture.finalized)
	store := &renewalSignalStore{lifecycleStoreFixture: fixture, renewed: make(chan struct{}, 1)}
	archive := &fenceStageArchive{syntheticPageArchive: &syntheticPageArchive{}}
	archive.onStage = func(ctx context.Context, claim JobClaim) error {
		select {
		case <-store.renewed:
		case <-ctx.Done():
			return ctx.Err()
		}
		if ctx.Err() != nil || claim.JobID != request.JobID || claim.Worker != "synthetic-worker" ||
			claim.Token == "" || claim.Epoch != 1 {
			return ErrJobConflict
		}
		return ErrJobIncomplete // Stop after observing the fenced staging boundary.
	}
	worker := &Worker{Store: store, Source: &syntheticProductionSource{data: pdf}, Pages: archive,
		Artifacts: &syntheticFinalArtifacts{}, WorkerID: "synthetic-worker",
		EngineFactory: func(redaction.Recipe) (pdfproduction.Engine, error) { return whiteProductionEngine{}, nil },
		renewInterval: time.Millisecond}
	_, err := worker.RunJob(t.Context(), request)
	require.ErrorIs(t, err, ErrJobIncomplete)
	require.Equal(t, 1, archive.stageCalls)
	require.NoError(t, archive.stageCtxErr)
	require.Equal(t, 1, fixture.claims)
	require.Zero(t, fixture.publications)
}

func TestProductionWorkerRejectsStagingAfterClaimLossOrCallerCancellation(t *testing.T) {
	for _, scenario := range []string{"lease_loss", "caller_cancellation"} {
		t.Run(scenario, func(t *testing.T) {
			pdf := syntheticLifecyclePDF(t)
			fixture := &lifecycleStoreFixture{storedFixture: newStoredFixtureWithSource(t, strings.Repeat("A", 85), pdf)}
			request := lifecycleRequest(fixture.finalized)
			started := make(chan struct{})
			archive := &fenceStageArchive{syntheticPageArchive: &syntheticPageArchive{}}
			archive.onStage = func(ctx context.Context, _ JobClaim) error {
				close(started)
				<-ctx.Done()
				return ErrJobStaleClaim
			}
			if scenario == "lease_loss" {
				fixture.renewErr = ErrJobStaleClaim
				fixture.renewGate = make(chan struct{})
			}
			worker := &Worker{Store: fixture, Source: &syntheticProductionSource{data: pdf}, Pages: archive,
				Artifacts: &syntheticFinalArtifacts{}, WorkerID: "synthetic-worker",
				EngineFactory: func(redaction.Recipe) (pdfproduction.Engine, error) { return whiteProductionEngine{}, nil },
				renewInterval: time.Millisecond}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			out := make(chan error, 1)
			go func() { _, err := worker.RunJob(ctx, request); out <- err }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("production page staging was not reached")
			}
			if scenario == "lease_loss" {
				close(fixture.renewGate)
			} else {
				cancel()
			}
			select {
			case err := <-out:
				if scenario == "lease_loss" {
					require.ErrorIs(t, err, ErrJobStaleClaim)
				} else {
					require.ErrorIs(t, err, context.Canceled)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("production staging did not stop")
			}
			require.ErrorIs(t, archive.stageCtxErr, context.Canceled)
			require.Equal(t, 1, archive.stageCalls)
			require.Empty(t, archive.pages)
			require.Zero(t, fixture.publications)
		})
	}
}

func TestProductionWorkerCheckpointsBeforeClaimAndRendering(t *testing.T) {
	fixture := &lifecycleStoreFixture{storedFixture: newStoredFixtureWithText(t, strings.Repeat("A", 85)),
		checkpointErr: ErrJobConflict}
	finalized := fixture.finalized
	fixture.request = JobRequest{JobID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
		OperationID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", SetID: finalized.Draft.SetID,
		Revision: finalized.Draft.Revision, ETag: finalized.Draft.ETag,
		RevisionSHA256:         finalized.Authority.Prepared.SHA256,
		PreparedInputSHA256:    finalized.Authority.Receipt.SHA256,
		NumberingProfileSHA256: testHash("synthetic numbering profile")}
	worker := Worker{Store: fixture, Source: &syntheticProductionSource{},
		Pages: &syntheticPageArchive{}, Artifacts: &syntheticFinalArtifacts{}, WorkerID: "synthetic-worker"}
	_, err := worker.RunJob(t.Context(), fixture.request)
	require.ErrorIs(t, err, ErrJobConflict, "err=%v", err)
	require.Equal(t, 1, fixture.reserved)
	require.Zero(t, fixture.claims)
	require.Zero(t, fixture.publications)
}

func TestBuildProductionRenderPlanUsesSealedPageAuthority(t *testing.T) {
	fixture := newStoredFixtureWithText(t, strings.Repeat("A", 85))
	finalized := fixture.finalized
	job := Job{ID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", SetID: finalized.Draft.SetID,
		Revision: finalized.Draft.Revision, ETag: finalized.Draft.ETag,
		RevisionSHA256:      finalized.Authority.Prepared.SHA256,
		PreparedInputSHA256: finalized.Authority.Receipt.SHA256}
	member := finalized.Authority.Prepared.Members[0].Member
	reservation := documentproduction.NumberReservation{Contract: documentproduction.NumberReservationContractV1,
		Authority: "bates-ledger/v1", ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		OperationID: job.ID, RevisionSHA256: job.RevisionSHA256, State: "reserved",
		Numbers: []documentproduction.AssignedNumber{{MemberID: member.ID, MemberOrdinal: member.Ordinal,
			Page: 1, Text: "SYN000001"}}}
	_, digest, err := documentproduction.CanonicalNumberReservation(reservation)
	require.NoError(t, err)
	reservation.SHA256 = digest
	require.NoError(t, documentproduction.ValidatePreparedInputAuthority(finalized.Authority))
	_, err = PlanEndorsementPages(member.ID, finalized.Authority.Prepared.Members[0].Resolved.Pages,
		finalized.Authority.Prepared.Members[0].Resolved, reservation.Numbers, pdfproduction.QualifiedRecipe())
	require.NoError(t, err)
	plan, err := BuildProductionRenderPlan(job, finalized, reservation, pdfproduction.QualifiedRecipe())
	require.NoError(t, err)
	require.Equal(t, job.ID, plan.JobID)
	require.Equal(t, reservation.SHA256, plan.Reservation.SHA256)
	require.Len(t, plan.Pages, 1)
	require.Equal(t, finalized.Authority.Prepared.Members[0].Member.ID, plan.Pages[0].MemberID)
	require.Equal(t, finalized.Authority.Prepared.Members[0].ResolvedSHA256, plan.Pages[0].ResolvedSHA256)
	require.NotEmpty(t, plan.LayoutSHA256)
	require.NotEmpty(t, plan.EndorsementsSHA256)
}
