package processing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

// RenditionRuntime prepares the local sealed upload and exact provider
// authorization for a claimed immutable work item. Prepare must not perform
// provider egress; the worker's consent fence is deliberately later.
type RenditionRuntime interface {
	Prepare(
		ctx context.Context, work store.RenditionJobWork, now time.Time,
	) (RenditionExecution, error)
}

// RenditionResumeRuntime reconstructs only a provider client for an already
// sealed durable operation. It must not reopen or read source bytes.
type RenditionResumeRuntime interface {
	ResumeProvider(
		ctx context.Context, work store.RenditionJobWork,
		snapshot document.RenditionExecutionSnapshotV1,
	) (document.RenditionProvider, error)
}

// ErrRenditionRuntimeUnavailable means no process-local adapter is registered
// for the immutable descriptor. It is safe to retry because Prepare performs
// no provider egress.
var ErrRenditionRuntimeUnavailable = errors.New("rendition runtime is unavailable")

// RenditionRuntimeRegistry resolves immutable descriptor fingerprints without
// adding provider enumerations or provider-specific state to SQLite.
type RenditionRuntimeRegistry struct {
	mu       sync.RWMutex
	runtimes map[string]RenditionRuntime
}

// NewRenditionRuntimeRegistry returns an empty process-local registry.
func NewRenditionRuntimeRegistry() *RenditionRuntimeRegistry {
	return &RenditionRuntimeRegistry{runtimes: make(map[string]RenditionRuntime)}
}

// Ready reports whether at least one executable provider adapter is bound.
// A daemon must not claim restored work while the registry is empty.
func (registry *RenditionRuntimeRegistry) Ready() bool {
	if registry == nil {
		return false
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	return len(registry.runtimes) != 0
}

// Register binds one descriptor fingerprint once for this daemon lifecycle.
func (registry *RenditionRuntimeRegistry) Register(
	descriptorFingerprint string, runtime RenditionRuntime,
) error {
	if registry == nil || len(descriptorFingerprint) != sha256.Size*2 ||
		renditionInterfaceNil(runtime) {
		return errors.New("rendition runtime registration is invalid")
	}
	if _, err := hex.DecodeString(descriptorFingerprint); err != nil ||
		descriptorFingerprint != string(bytes.ToLower([]byte(descriptorFingerprint))) {
		return errors.New("rendition runtime descriptor fingerprint is invalid")
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, exists := registry.runtimes[descriptorFingerprint]; exists {
		return errors.New("rendition runtime descriptor is already registered")
	}
	registry.runtimes[descriptorFingerprint] = runtime
	return nil
}

// Prepare dispatches to the exact registered descriptor runtime.
func (registry *RenditionRuntimeRegistry) Prepare(
	ctx context.Context, work store.RenditionJobWork, now time.Time,
) (RenditionExecution, error) {
	if registry == nil {
		return RenditionExecution{}, ErrRenditionRuntimeUnavailable
	}
	var profile document.ProcessingProfileV1
	if err := json.Unmarshal(
		work.Profile.CanonicalProfile, &profile, json.RejectUnknownMembers(true)); err != nil ||
		profile.Rendition == nil {
		return RenditionExecution{}, errors.New("rendition runtime profile is invalid")
	}
	registry.mu.RLock()
	runtime := registry.runtimes[profile.Rendition.Descriptor.Fingerprint]
	registry.mu.RUnlock()
	if runtime == nil {
		return RenditionExecution{}, ErrRenditionRuntimeUnavailable
	}
	return runtime.Prepare(ctx, work, now)
}

// ResumeProvider dispatches without calling Prepare or reopening the source.
func (registry *RenditionRuntimeRegistry) ResumeProvider(
	ctx context.Context, work store.RenditionJobWork,
	snapshot document.RenditionExecutionSnapshotV1,
) (document.RenditionProvider, error) {
	if registry == nil {
		return nil, ErrRenditionRuntimeUnavailable
	}
	registry.mu.RLock()
	runtime := registry.runtimes[snapshot.Identity.Authorization.DescriptorFingerprint]
	registry.mu.RUnlock()
	resumable, ok := runtime.(RenditionResumeRuntime)
	if !ok || renditionInterfaceNil(resumable) {
		return nil, ErrRenditionRuntimeUnavailable
	}
	return resumable.ResumeProvider(ctx, work, snapshot)
}

// RenditionExecution contains transient provider inputs plus the executable
// normalization policy pinned by the processing profile.
type RenditionExecution struct {
	Provider        document.RenditionProvider
	Upload          document.AuthorizedUpload
	Authorization   document.RenditionAuthorization
	EvidencePolicy  document.EvidencePolicy
	RenditionPolicy document.RenditionPolicy
}

// RenditionMutationGate is the daemon-wide admission boundary shared by HTTP
// mutations, backup/restore, maintenance, and background processing.
type RenditionMutationGate interface {
	MutateContext(ctx context.Context, fn func() error) error
}

type renditionWorkerCatalog interface {
	ClaimNextRenditionJob(ctx context.Context, owner string, at time.Time, lease time.Duration) (
		store.RenditionJobClaim, bool, error)
	RenditionJobWorkByClaim(ctx context.Context, claim store.RenditionJobClaim, at time.Time) (
		store.RenditionJobWork, error)
	RenewRenditionJobClaim(ctx context.Context, claim store.RenditionJobClaim,
		at time.Time, lease time.Duration) (
		store.RenditionJobClaim, error)
	BeginRenditionProvider(ctx context.Context, claim store.RenditionJobClaim, waiterID string,
		at time.Time, snapshots ...document.RenditionExecutionSnapshotV1) (
		store.ProviderOperationAuthorization, error)
	CheckpointRenditionProvider(ctx context.Context, claim store.RenditionJobClaim,
		handle string, at time.Time) error
	MarkRenditionJobRetry(ctx context.Context, claim store.RenditionJobClaim,
		failure store.RenditionFailureCode, availableAt, at time.Time) error
	MarkRenditionJobOperatorRequired(ctx context.Context, claim store.RenditionJobClaim,
		at time.Time) error
	MarkRenditionJobFailed(ctx context.Context, claim store.RenditionJobClaim,
		failure store.RenditionFailureCode, at time.Time) error
	RecordRenditionBlob(ctx context.Context, hash string, size int64,
		physical store.BlobPhysical) error
	StageRenditionJobBuild(ctx context.Context, claim store.RenditionJobClaim,
		build store.RenditionBuildRecord, at time.Time) error
	StageRenditionJobGeneration(ctx context.Context, claim store.RenditionJobClaim,
		generationID string, at time.Time) (
		store.LexicalGeneration, error)
	PublishRenditionJob(ctx context.Context, claim store.RenditionJobClaim, at time.Time) (
		store.RenditionJobPublication, error)
	RenditionJobErrorRetryable(err error) bool
}

type targetedRenditionWorkerCatalog interface {
	ClaimRenditionJob(ctx context.Context, jobID, owner string, at time.Time,
		lease time.Duration) (store.RenditionJobClaim, error)
}

// RenditionWorkerConfig binds the provider-neutral state machine to one vault.
type RenditionWorkerConfig struct {
	Catalog       renditionWorkerCatalog
	Blobs         renditionBlobWriter
	Runtime       RenditionRuntime
	Gate          RenditionMutationGate
	Owner         string
	LeaseDuration time.Duration
	IdleDelay     time.Duration
	Clock         func() time.Time
}

// RenditionWorker claims, resumes, validates, stages, and publishes shared
// rendition builds without exposing provider data through status or logs.
type RenditionWorker struct {
	catalog       renditionWorkerCatalog
	blobs         renditionBlobWriter
	runtime       RenditionRuntime
	gate          RenditionMutationGate
	owner         string
	leaseDuration time.Duration
	idleDelay     time.Duration
	clock         func() time.Time
}

type renditionWorkerFatalError struct{ cause error }
type renditionWorkerRetryableError struct{ cause error }

var errRenditionLeaseStopped = errors.New("rendition lease heartbeat stopped")

func (err *renditionWorkerFatalError) Error() string     { return err.cause.Error() }
func (err *renditionWorkerFatalError) Unwrap() error     { return err.cause }
func (err *renditionWorkerRetryableError) Error() string { return err.cause.Error() }
func (err *renditionWorkerRetryableError) Unwrap() error { return err.cause }

func renditionWorkerFatal(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*renditionWorkerFatalError](err); ok {
		return err
	}
	return &renditionWorkerFatalError{cause: err}
}

func isRenditionWorkerFatal(err error) bool {
	var fatal *renditionWorkerFatalError
	return errors.As(err, &fatal)
}

func isRenditionWorkerRetryable(err error) bool {
	var retryable *renditionWorkerRetryableError
	return errors.As(err, &retryable)
}

// NewRenditionWorker validates the complete durable worker boundary.
func NewRenditionWorker(config RenditionWorkerConfig) (*RenditionWorker, error) {
	if renditionInterfaceNil(config.Catalog) || renditionInterfaceNil(config.Blobs) ||
		renditionInterfaceNil(config.Runtime) || renditionInterfaceNil(config.Gate) {
		return nil, errors.New("rendition worker requires catalog, blob store, runtime, and operation gate")
	}
	if config.Owner == "" || len(config.Owner) > 128 {
		return nil, errors.New("rendition worker owner is invalid")
	}
	if config.LeaseDuration < 3*time.Second || config.LeaseDuration > time.Hour {
		return nil, errors.New("rendition worker lease must be between three seconds and one hour")
	}
	if config.IdleDelay <= 0 || config.IdleDelay > time.Minute {
		return nil, errors.New("rendition worker idle delay must be positive and at most one minute")
	}
	if config.Clock == nil {
		config.Clock = func() time.Time { return time.Now().UTC() }
	}
	return &RenditionWorker{
		catalog: config.Catalog, blobs: config.Blobs, runtime: config.Runtime,
		gate:  config.Gate,
		owner: config.Owner, leaseDuration: config.LeaseDuration,
		idleDelay: config.IdleDelay, clock: config.Clock,
	}, nil
}

// Run processes durable jobs until cancellation. A quiet queue is expected,
// so the supervisor entry remains running rather than completing spuriously.
func (worker *RenditionWorker) Run(ctx context.Context) error {
	if worker == nil {
		return errors.New("rendition worker is nil")
	}
	for {
		processed, err := worker.RunOne(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, store.ErrRenditionJobFenced) {
				continue
			}
			if isRenditionWorkerRetryable(err) {
				if err := waitRenditionWorker(ctx, min(worker.idleDelay, 250*time.Millisecond)); err != nil {
					return err
				}
				continue
			}
			if isRenditionWorkerFatal(err) {
				return err
			}
			return renditionWorkerFatal(err)
		}
		if processed {
			continue
		}
		if err := waitRenditionWorker(ctx, worker.idleDelay); err != nil {
			return err
		}
	}
}

// RunOne processes at most one ready shared build.
func (worker *RenditionWorker) RunOne(ctx context.Context) (
	processed bool, retErr error,
) {
	if worker == nil {
		return false, errors.New("rendition worker is nil")
	}
	err := worker.gate.MutateContext(ctx, func() error {
		var runErr error
		processed, runErr = worker.runOneUnderGate(ctx)
		return runErr
	})
	if err != nil && ctx.Err() == nil && !errors.Is(err, store.ErrRenditionJobFenced) &&
		!isRenditionWorkerFatal(err) && !isRenditionWorkerRetryable(err) {
		err = renditionWorkerFatal(err)
	}
	return processed, err
}

// RunJob claims and processes one exact ready shared build. It is used by
// request/response surfaces that must not consume unrelated queued work.
func (worker *RenditionWorker) RunJob(ctx context.Context, jobID string) (
	processed bool, retErr error,
) {
	if worker == nil {
		return false, errors.New("rendition worker is nil")
	}
	target, ok := worker.catalog.(targetedRenditionWorkerCatalog)
	if !ok {
		return false, errors.New("rendition catalog does not support targeted claims")
	}
	err := worker.gate.MutateContext(ctx, func() error {
		claim, claimErr := target.ClaimRenditionJob(ctx, jobID, worker.owner,
			worker.clock().UTC(), worker.leaseDuration)
		if claimErr != nil {
			return claimErr
		}
		processed = true
		_, runErr := worker.runClaimUnderGate(ctx, claim)
		return runErr
	})
	if err != nil && ctx.Err() == nil && !errors.Is(err, store.ErrRenditionJobFenced) &&
		!isRenditionWorkerFatal(err) && !isRenditionWorkerRetryable(err) {
		err = renditionWorkerFatal(err)
	}
	return processed, err
}

// runOneUnderGate keeps the operation order gate -> blob coordinator ->
// physical write -> catalog stage/publication for the complete claimed unit.
// The mutation gate is shared-weight: retaining it across retryable in-claim
// catalog faults excludes maintenance without blocking ordinary mutations.
func (worker *RenditionWorker) runOneUnderGate(ctx context.Context) (
	processed bool, retErr error,
) {
	now := worker.clock().UTC()
	var claim store.RenditionJobClaim
	var found bool
	err := worker.catalogOnce(ctx, func() error {
		var claimErr error
		claim, found, claimErr = worker.catalog.ClaimNextRenditionJob(
			ctx, worker.owner, now, worker.leaseDuration)
		return claimErr
	})
	if err != nil || !found {
		return found, err
	}
	return worker.runClaimUnderGate(ctx, claim)
}

func (worker *RenditionWorker) runClaimUnderGate(ctx context.Context,
	claim store.RenditionJobClaim,
) (processed bool, retErr error) {
	leaseCtx, stopLease := worker.keepLease(ctx, claim)
	defer func() {
		leaseErr := stopLease()
		if retErr == nil && errors.Is(leaseErr, store.ErrRenditionJobFenced) {
			return
		}
		retErr = errors.Join(retErr, leaseErr)
	}()
	ctx = leaseCtx
	var work store.RenditionJobWork
	err := worker.retryCatalog(ctx, func() error {
		var workErr error
		work, workErr = worker.catalog.RenditionJobWorkByClaim(
			ctx, claim, worker.clock().UTC())
		return workErr
	})
	if err != nil {
		return true, worker.classifyAuthorityError(ctx, claim, err)
	}
	claim.Phase = work.Job.Phase

	switch work.Job.Phase {
	case store.RenditionPhaseBuildStaged:
		return true, worker.stageGenerationAndPublish(ctx, claim)
	case store.RenditionPhaseGenerationStaged:
		return true, worker.stageGenerationAndPublish(ctx, claim)
	case store.RenditionPhaseQueued, store.RenditionPhaseProvider:
	default:
		return true, renditionWorkerFatal(fmt.Errorf(
			"rendition worker claimed unsupported phase %q", work.Job.Phase))
	}

	var execution RenditionExecution
	var snapshot document.RenditionExecutionSnapshotV1
	var durableResume atomic.Bool
	resuming := claim.ResumeHandle != ""
	if resuming {
		if work.ExecutionSnapshot == nil {
			return true, worker.markOperatorRequired(
				ctx, claim, worker.clock().UTC())
		}
		snapshot = *work.ExecutionSnapshot
		if err := validateRenditionExecutionSnapshot(work, snapshot); err != nil {
			return true, worker.markFailed(
				ctx, claim, store.RenditionFailureTerminal, worker.clock().UTC())
		}
		resumeRuntime, ok := worker.runtime.(RenditionResumeRuntime)
		if !ok || renditionInterfaceNil(resumeRuntime) {
			return true, worker.markSafeTransient(ctx, claim)
		}
		provider, err := resumeRuntime.ResumeProvider(ctx, work, snapshot)
		if err != nil || renditionInterfaceNil(provider) {
			return true, worker.markSafeTransient(ctx, claim)
		}
		evidencePolicy, renditionPolicy, err := snapshot.Policies()
		if err != nil {
			return true, worker.markFailed(
				ctx, claim, store.RenditionFailureTerminal, worker.clock().UTC())
		}
		execution = RenditionExecution{
			Provider: provider, Authorization: snapshot.Authorization,
			EvidencePolicy: evidencePolicy, RenditionPolicy: renditionPolicy,
		}
		durableResume.Store(true)
		if err := worker.retryCatalog(ctx, func() error {
			_, beginErr := worker.catalog.BeginRenditionProvider(
				ctx, claim, work.Waiter.ID, worker.clock().UTC())
			return beginErr
		}); err != nil {
			return true, worker.classifyAuthorityError(ctx, claim, err)
		}
	} else {
		var err error
		execution, err = worker.runtime.Prepare(ctx, work, worker.clock().UTC())
		if err != nil {
			return true, worker.markSafeTransient(ctx, claim)
		}
		if renditionInterfaceNil(execution.Upload) || renditionInterfaceNil(execution.Provider) {
			if !renditionInterfaceNil(execution.Upload) {
				_ = execution.Upload.Close()
			}
			return true, worker.markSafeTransient(ctx, claim)
		}
		defer func() { _ = execution.Upload.Close() }()
		if err := validateRenditionExecution(work, execution); err != nil {
			return true, worker.markFailed(
				ctx, claim, store.RenditionFailureTerminal, worker.clock().UTC())
		}
		snapshot, err = document.SealRenditionExecutionAt(
			worker.clock().UTC(), execution.Provider, execution.Upload, execution.Authorization,
			execution.EvidencePolicy, execution.RenditionPolicy)
		if err != nil || validateRenditionExecutionSnapshot(work, snapshot) != nil {
			return true, worker.markFailed(
				ctx, claim, store.RenditionFailureTerminal, worker.clock().UTC())
		}
		// The sealed canonical copy is the sole authorization used for both
		// the first provider call and any later durable resume.
		execution.Authorization = snapshot.Authorization
		if err := worker.retryCatalog(ctx, func() error {
			_, beginErr := worker.catalog.BeginRenditionProvider(
				ctx, claim, work.Waiter.ID, worker.clock().UTC(), snapshot)
			return beginErr
		}); err != nil {
			return true, worker.classifyAuthorityError(ctx, claim, err)
		}
	}
	checkpoint := func(handle document.RenditionResumeHandle) error {
		err := worker.retryCatalog(ctx, func() error {
			return worker.catalog.CheckpointRenditionProvider(
				ctx, claim, handle.Value, worker.clock().UTC())
		})
		if err == nil {
			durableResume.Store(true)
		}
		return err
	}
	var result document.RenditionResult
	if resuming {
		result, err = document.ResumeRendition(
			ctx, execution.Provider, snapshot,
			document.RenditionResumeHandle{Value: claim.ResumeHandle}, checkpoint)
	} else {
		result, err = document.RenderRenditionWithResume(
			ctx, execution.Provider, execution.Upload, execution.Authorization, nil, checkpoint)
	}
	if err != nil {
		return true, worker.classifyProviderError(ctx, claim, err, durableResume.Load())
	}
	staged, err := buildRenditionJobCandidate(
		work, execution, result, claim.Epoch, worker.clock().UTC())
	if err != nil {
		return true, worker.markFailed(
			ctx, claim, store.RenditionFailureTerminal, worker.clock().UTC())
	}
	if err := worker.stageArtifactsAndBuild(ctx, claim, staged); err != nil {
		if errors.Is(err, store.ErrRenditionJobFenced) {
			return true, err
		}
		if isRenditionWorkerFatal(err) {
			return true, err
		}
		if !durableResume.Load() {
			return true, worker.markOperatorRequired(
				ctx, claim, worker.clock().UTC())
		}
		return true, worker.markSafeTransient(ctx, claim)
	}
	return true, worker.stageGenerationAndPublish(ctx, claim)
}

func (worker *RenditionWorker) keepLease(
	ctx context.Context, claim store.RenditionJobClaim,
) (context.Context, func() error) {
	leaseCtx, cancel := context.WithCancelCause(ctx)
	stop := make(chan struct{})
	done := make(chan error, 1)
	interval := max(worker.leaseDuration/3, time.Second)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				done <- nil
				return
			case <-leaseCtx.Done():
				done <- nil
				return
			case <-ticker.C:
				err := worker.retryCatalog(leaseCtx, func() error {
					_, renewErr := worker.catalog.RenewRenditionJobClaim(
						leaseCtx, claim, worker.clock().UTC(), worker.leaseDuration)
					return renewErr
				})
				if err != nil {
					if errors.Is(context.Cause(leaseCtx), errRenditionLeaseStopped) {
						done <- nil
						return
					}
					cancel(err)
					done <- err
					return
				}
			}
		}
	}()
	var once sync.Once
	var leaseErr error
	return leaseCtx, func() error {
		once.Do(func() {
			cancel(errRenditionLeaseStopped)
			close(stop)
			leaseErr = <-done
		})
		return leaseErr
	}
}

func (worker *RenditionWorker) retryCatalog(ctx context.Context, operation func() error) error {
	delay := min(worker.idleDelay, 250*time.Millisecond)
	for {
		err := operation()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !worker.catalog.RenditionJobErrorRetryable(err) {
			return renditionWorkerFatal(err)
		}
		if err := waitRenditionWorker(ctx, delay); err != nil {
			return err
		}
	}
}

func (worker *RenditionWorker) catalogOnce(ctx context.Context, operation func() error) error {
	err := operation()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if worker.catalog.RenditionJobErrorRetryable(err) {
		return &renditionWorkerRetryableError{cause: err}
	}
	return renditionWorkerFatal(err)
}

func waitRenditionWorker(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (worker *RenditionWorker) stageArtifactsAndBuild(
	ctx context.Context, claim store.RenditionJobClaim, staged StagedRendition,
) error {
	if err := validateStagedRendition(staged); err != nil {
		return err
	}
	return worker.blobs.WithMutation(ctx, func() error {
		payloads := make(map[string]StagedArtifact, len(staged.Artifacts))
		for _, artifact := range staged.Artifacts {
			payloads[artifact.ID] = artifact
		}
		for index, record := range staged.Build.Artifacts {
			candidate := payloads[record.ID]
			receipt, writeErr := worker.blobs.WriteDetailedContext(
				ctx, io.LimitReader(candidate.Payload, record.Size+1))
			if receipt.Hash != "" {
				encoding, encodingErr := receipt.EncodingName()
				if encodingErr != nil {
					return errors.Join(writeErr, encodingErr)
				}
				physical := store.BlobPhysical{
					Encoding: encoding, StoredBytes: receipt.StoredSize,
					PackEligible: receipt.PackEligible, Created: receipt.Created,
				}
				if record.Role == "sanitized_markdown" {
					physical.MD5 = receipt.MD5
					staged.Build.Artifacts[index].MD5 = receipt.MD5
				}
				if err := worker.retryCatalog(ctx, func() error {
					return worker.catalog.RecordRenditionBlob(
						ctx, receipt.Hash, receipt.Size, physical)
				}); err != nil {
					return errors.Join(writeErr, err)
				}
			}
			if writeErr != nil {
				return writeErr
			}
			if receipt.Hash != record.BlobHash || receipt.Size != record.Size {
				return errors.New("verified rendition artifact receipt does not match its catalog identity")
			}
		}
		if err := store.ValidateRenditionBuildRecord(staged.Build); err != nil {
			return err
		}
		return worker.retryCatalog(ctx, func() error {
			return worker.catalog.StageRenditionJobBuild(
				ctx, claim, staged.Build, worker.clock().UTC())
		})
	})
}

func (worker *RenditionWorker) stageGenerationAndPublish(
	ctx context.Context, claim store.RenditionJobClaim,
) error {
	generationID := renditionJobGenerationID(claim.JobID, claim.Epoch)
	if err := worker.retryCatalog(ctx, func() error {
		_, stageErr := worker.catalog.StageRenditionJobGeneration(
			ctx, claim, generationID, worker.clock().UTC())
		return stageErr
	}); err != nil {
		return err
	}
	err := worker.retryCatalog(ctx, func() error {
		_, publishErr := worker.catalog.PublishRenditionJob(ctx, claim, worker.clock().UTC())
		return publishErr
	})
	return worker.classifyPublicationError(ctx, claim, err)
}

func (worker *RenditionWorker) classifyProviderError(
	ctx context.Context, claim store.RenditionJobClaim, err error, durableResume bool,
) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if isRenditionWorkerFatal(err) {
		return err
	}
	providerError, classified := err.(*document.RenditionProviderError) //nolint:errorlint
	if !classified {
		return worker.markOperatorRequired(
			ctx, claim, worker.clock().UTC())
	}
	if providerError.Code() == document.RenditionErrorAmbiguousSubmission {
		if durableResume {
			now := worker.clock().UTC()
			delay := providerError.RetryAfter()
			if delay <= 0 {
				delay = 30 * time.Second
			}
			return worker.markRetry(
				ctx, claim, store.RenditionFailureTransient, now, now.Add(delay))
		}
		return worker.markOperatorRequired(
			ctx, claim, worker.clock().UTC())
	}
	if document.IsRenditionProviderErrorRetryable(err) {
		now := worker.clock().UTC()
		delay := providerError.RetryAfter()
		if delay <= 0 {
			delay = time.Second
		}
		return worker.markRetry(
			ctx, claim, store.RenditionFailureTransient, now, now.Add(delay))
	}
	return worker.markFailed(
		ctx, claim, store.RenditionFailureTerminal, worker.clock().UTC())
}

func (worker *RenditionWorker) classifyPublicationError(
	ctx context.Context, claim store.RenditionJobClaim, err error,
) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, store.ErrLexicalGenerationStale) {
		return worker.markSafeTransient(ctx, claim)
	}
	if errors.Is(err, store.ErrProcessingConsentRequired) ||
		errors.Is(err, store.ErrProcessingConsentExpired) ||
		errors.Is(err, store.ErrProcessingConsentRevoked) {
		return worker.markFailed(
			ctx, claim, store.RenditionFailureConsent, worker.clock().UTC())
	}
	if errors.Is(err, store.ErrRenditionJobStaleAuthority) {
		return worker.markFailed(
			ctx, claim, store.RenditionFailureStaleAuthority, worker.clock().UTC())
	}
	return err
}

func (worker *RenditionWorker) classifyAuthorityError(
	ctx context.Context, claim store.RenditionJobClaim, err error,
) error {
	if errors.Is(err, store.ErrProcessingConsentRequired) ||
		errors.Is(err, store.ErrProcessingConsentExpired) ||
		errors.Is(err, store.ErrProcessingConsentRevoked) {
		return worker.markFailed(
			ctx, claim, store.RenditionFailureConsent, worker.clock().UTC())
	}
	if errors.Is(err, store.ErrRenditionJobStaleAuthority) || errors.Is(err, store.ErrNotFound) {
		return worker.markFailed(
			ctx, claim, store.RenditionFailureStaleAuthority, worker.clock().UTC())
	}
	return err
}

func (worker *RenditionWorker) markSafeTransient(
	ctx context.Context, claim store.RenditionJobClaim,
) error {
	now := worker.clock().UTC()
	return worker.markRetry(
		ctx, claim, store.RenditionFailureTransient, now, now.Add(30*time.Second))
}

func (worker *RenditionWorker) markRetry(
	ctx context.Context, claim store.RenditionJobClaim, code store.RenditionFailureCode,
	at, availableAt time.Time,
) error {
	return worker.retryCatalog(ctx, func() error {
		return worker.catalog.MarkRenditionJobRetry(ctx, claim, code, at, availableAt)
	})
}

func (worker *RenditionWorker) markOperatorRequired(
	ctx context.Context, claim store.RenditionJobClaim, at time.Time,
) error {
	return worker.retryCatalog(ctx, func() error {
		return worker.catalog.MarkRenditionJobOperatorRequired(ctx, claim, at)
	})
}

func (worker *RenditionWorker) markFailed(
	ctx context.Context, claim store.RenditionJobClaim,
	code store.RenditionFailureCode, at time.Time,
) error {
	return worker.retryCatalog(ctx, func() error {
		return worker.catalog.MarkRenditionJobFailed(ctx, claim, code, at)
	})
}

func validateRenditionExecution(
	work store.RenditionJobWork, execution RenditionExecution,
) error {
	var profile document.ProcessingProfileV1
	if err := json.Unmarshal(
		work.Profile.CanonicalProfile, &profile, json.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("decoding rendition job profile: %w", err)
	}
	if profile.Rendition == nil {
		return errors.New("rendition job profile has no rendition binding")
	}
	descriptor := execution.Provider.Descriptor()
	if descriptor.ID != profile.Rendition.Descriptor.ID ||
		descriptor.Fingerprint != profile.Rendition.Descriptor.Fingerprint {
		return errors.New("rendition runtime provider does not match the immutable profile")
	}
	if execution.Authorization.RenditionRequestFingerprint !=
		work.Profile.RenditionRequestFingerprint ||
		execution.Authorization.SourceSHA256 != work.Job.SourceSHA256 {
		return errors.New("rendition runtime authorization does not match immutable work")
	}
	if execution.RenditionPolicy.MaxSegmentRunes() != profile.EvidenceLexical.MaxSegmentRunes {
		return errors.New("rendition runtime lexical policy does not match immutable profile")
	}
	return nil
}

func validateRenditionExecutionSnapshot(
	work store.RenditionJobWork, snapshot document.RenditionExecutionSnapshotV1,
) error {
	canonical, fingerprint, err := document.CanonicalRenditionExecutionIdentityV1(snapshot.Identity)
	if err != nil {
		return err
	}
	want, wantFingerprint, err := document.CanonicalRenditionExecutionIdentityV1(
		work.ExecutionIdentity)
	if err != nil {
		return err
	}
	if fingerprint != work.Job.ExecutionIdentityFingerprint ||
		wantFingerprint != fingerprint || !bytes.Equal(want, canonical) {
		return errors.New("rendition runtime execution identity does not match immutable work")
	}
	return nil
}

func buildRenditionJobCandidate(
	work store.RenditionJobWork, execution RenditionExecution,
	result document.RenditionResult, claimEpoch int64, at time.Time,
) (StagedRendition, error) {
	normalized, err := document.NormalizeEvidenceV1(result.Evidence, execution.EvidencePolicy)
	if err != nil {
		return StagedRendition{}, fmt.Errorf("normalizing rendition evidence: %w", err)
	}
	evidenceBytes, evidenceChecksum, err := document.MarshalNormalizedEvidenceV1(normalized)
	if err != nil {
		return StagedRendition{}, fmt.Errorf("encoding normalized rendition evidence: %w", err)
	}
	rendition, err := document.BuildRenditionV1(normalized, execution.RenditionPolicy)
	if err != nil {
		return StagedRendition{}, fmt.Errorf("building normalized rendition: %w", err)
	}
	var profile document.ProcessingProfileV1
	if err := json.Unmarshal(
		work.Profile.CanonicalProfile, &profile, json.RejectUnknownMembers(true)); err != nil {
		return StagedRendition{}, fmt.Errorf("decoding rendition profile: %w", err)
	}
	metadata := work.ExecutionIdentity.Upload
	rendition, _, err = document.EnvelopeRenditionV1(rendition, document.RenditionEnvelopeV1{
		BuildID: work.Job.ID, SourceSHA256: work.Job.SourceSHA256,
		SourceFormat:                sourceFormat(metadata.Filename, metadata.MediaFamily, metadata.MediaType),
		SourceMediaType:             metadata.MediaType,
		RenditionRequestFingerprint: work.Job.RenditionRequestFingerprint,
		EvidenceLexicalFingerprint:  work.Job.EvidenceLexicalFingerprint,
		NormalizedEvidenceContract:  profile.EvidenceLexical.NormalizedEvidenceContract,
		UnitKind:                    result.Evidence.UnitKind,
	})
	if err != nil {
		return StagedRendition{}, fmt.Errorf("enveloping normalized rendition: %w", err)
	}
	type retained struct {
		role    string
		payload []byte
	}
	retainedArtifacts := []retained{{role: "normalized_evidence", payload: evidenceBytes}}
	if profile.RetentionDisclosure.RetainSanitizedMarkdown {
		retainedArtifacts = append(retainedArtifacts,
			retained{role: "sanitized_markdown", payload: rendition.Markdown})
	}
	if profile.RetentionDisclosure.RetainProviderMarkdown && len(result.ProviderMarkdown) != 0 {
		retainedArtifacts = append(retainedArtifacts,
			retained{role: string(document.EvidenceArtifactMarkdown), payload: result.ProviderMarkdown})
	}
	if profile.RetentionDisclosure.RetainTypedArtifacts {
		for _, artifact := range result.Artifacts {
			retainedArtifacts = append(retainedArtifacts,
				retained{role: string(artifact.Role), payload: artifact.Payload})
		}
	}
	records := make([]store.RenditionArtifactRecord, len(retainedArtifacts))
	payloads := make([]StagedArtifact, len(retainedArtifacts))
	roleCounts := make(map[string]int)
	for index, artifact := range retainedArtifacts {
		digest := sha256.Sum256(artifact.payload)
		hash := hex.EncodeToString(digest[:])
		ordinal := roleCounts[artifact.role]
		roleCounts[artifact.role]++
		id := renditionArtifactID(work.Job.ID, artifact.role, ordinal, hash)
		records[index] = store.RenditionArtifactRecord{
			ID: id, Role: artifact.role, BlobHash: hash, Size: int64(len(artifact.payload)),
			Checksum: hash, State: store.RenditionArtifactVerified,
		}
		payloads[index] = StagedArtifact{ID: id, Payload: bytes.NewReader(artifact.payload)}
	}
	units := make([]store.RenditionUnitRecord, len(rendition.Units))
	for index, unit := range rendition.Units {
		units[index] = store.RenditionUnitRecord{
			ID: unit.ID, EvidenceUnitID: unit.EvidenceUnitID, Order: unit.Order,
			Checksum: unit.Checksum, HeadingPath: append([]string(nil), unit.HeadingPath...),
			Locator: unit.Locator,
		}
	}
	segments := make([]store.RenditionLexicalSegmentRecord, len(rendition.LexicalSegments))
	for index, segment := range rendition.LexicalSegments {
		segments[index] = store.RenditionLexicalSegmentRecord{
			ID: segment.ID, UnitID: segment.UnitID, Order: segment.Order,
			CharStart: segment.CharStart, CharEnd: segment.CharEnd,
			Checksum: segment.Checksum, Text: segment.Text,
		}
	}
	warnings := make([]string, len(rendition.Warnings))
	for index, warning := range rendition.Warnings {
		warnings[index] = warning.Code
	}
	authorizationBytes, err := json.Marshal(execution.Authorization, json.Deterministic(true))
	if err != nil {
		return StagedRendition{}, fmt.Errorf("encoding rendition authorization receipt: %w", err)
	}
	receiptBytes, err := json.Marshal(result.Receipt, json.Deterministic(true))
	if err != nil {
		return StagedRendition{}, fmt.Errorf("encoding rendition provider receipt: %w", err)
	}
	build := store.RenditionBuildRecord{
		ID: work.Job.ID, VaultID: work.VaultID, SourceSHA256: work.Job.SourceSHA256,
		RenditionRequestFingerprint:       work.Job.RenditionRequestFingerprint,
		EvidenceLexicalFingerprint:        work.Job.EvidenceLexicalFingerprint,
		CapturedArtifactPolicyFingerprint: work.Job.CapturedArtifactPolicyFingerprint,
		CapturedArtifactPolicy:            append(jsontext.Value(nil), work.CapturedArtifactPolicy...),
		AuthorizationChecksum:             renditionBytesSHA256(authorizationBytes),
		ProviderOperationID:               result.Receipt.OperationID,
		ProviderReceipt:                   append(jsontext.Value(nil), receiptBytes...),
		EvidenceChecksum:                  evidenceChecksum, RenditionChecksum: rendition.Checksum,
		MarkdownChecksum: rendition.MarkdownChecksum, Completeness: rendition.Completeness,
		PartialSuccess: slices.Contains(result.Receipt.Warnings, "partial_success"),
		Warnings:       warnings, CompletedAt: result.Receipt.CompletedAt,
		DeclaredArtifactCount: len(records), Artifacts: records,
		Units: units, LexicalSegments: segments,
	}
	attachment := store.RenditionAttachmentRecord{
		ID: work.Waiter.AttachmentID, VaultID: build.VaultID,
		ContentVersionID: work.Waiter.ContentVersionID, BuildID: build.ID,
		Profile: work.Profile, AttachedAt: at.Format("2006-01-02T15:04:05.000000000Z"),
	}
	generationID := renditionJobGenerationID(work.Job.ID, claimEpoch)
	return StagedRendition{
		Rendition: rendition, RenditionPolicy: execution.RenditionPolicy,
		Build: build, Attachment: attachment,
		Head: store.RenditionHeadRecord{
			ContentVersionID:             attachment.ContentVersionID,
			ProcessingProfileFingerprint: work.Profile.Fingerprint,
			AttachmentID:                 attachment.ID, PublishedAt: attachment.AttachedAt,
		},
		LexicalGenerationID: generationID, Artifacts: payloads,
	}, nil
}

func renditionArtifactID(buildID, role string, ordinal int, checksum string) string {
	digest := sha256.Sum256([]byte("docbank:rendition-artifact:v1\x00" + buildID + "\x00" +
		role + "\x00" + strconv.Itoa(ordinal) + "\x00" + checksum))
	return hex.EncodeToString(digest[:])
}

func renditionJobGenerationID(jobID string, epoch int64) string {
	digest := sha256.Sum256([]byte("docbank:rendition-generation:v1\x00" + jobID + "\x00" +
		strconv.FormatInt(epoch, 10)))
	return hex.EncodeToString(digest[:])
}

func renditionBytesSHA256(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func renditionInterfaceNil(value any) bool {
	if value == nil {
		return true
	}
	reflection := reflect.ValueOf(value)
	switch reflection.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflection.IsNil()
	default:
		return false
	}
}
