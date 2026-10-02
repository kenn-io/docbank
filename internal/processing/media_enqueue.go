package processing

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/docbank/internal/store"
)

var ErrMediaProcessingUnsupported = errors.New("media processing is unsupported")

// Aggregate job states that decide a media operation's outcome.
const statusFailed, statusAbandoned, statusCompleted, statusPartial = "failed", "abandoned", "completed", "partial"

func (service *Service) mediaProcessingProfile(name string) (configuredProfile, error) {
	profile, ok := service.profiles[name]
	if !ok {
		return configuredProfile{}, ErrProfileNotConfigured
	}
	if profile.portable.Rendition == nil {
		return configuredProfile{}, fmt.Errorf("%w: a rendition profile is required", ErrMediaProcessingUnsupported)
	}
	return profile, nil
}

func mediaProcessingRequested(processing *MediaProcessingRequest) bool {
	return processing != nil && processing.Profile != ""
}

// RenditionRuntimes exposes the service-owned registry to the daemon's
// supervised worker. The service and worker must execute the same admitted
// provider profiles.
func (service *Service) RenditionRuntimes() *RenditionRuntimeRegistry {
	if service == nil {
		return nil
	}
	return service.renditions
}

// EmbeddingRuntimes exposes the service-owned registry to an embedded vault's
// supervised worker. Admission and restart execution use the same profiles.
func (service *Service) EmbeddingRuntimes() *EmbeddingRuntimeRegistry {
	if service == nil {
		return nil
	}
	return service.embeddings
}

func validateMediaProcessing(processing *MediaProcessingRequest) error {
	if processing == nil {
		return nil
	}
	if processing.Profile == "" {
		return errors.New("processing profile is required for an explicit request")
	}
	if len(processing.Profile) > 128 || len(processing.SuppliedInputID) > 128 {
		return errors.New("processing selection exceeds bounds")
	}
	return nil
}

type mediaSourceBinding struct {
	sourceID, sourceVersionID string
}

// EnqueueAuthorized validates one exact current plan and an existing consent
// grant, then durably admits rendition work without running a provider or
// waiting for worker completion.
func (service *Service) EnqueueAuthorized(
	ctx context.Context,
	selector Selector,
	source mediaSourceBinding,
	planFingerprint string,
	authorization store.ProviderOperationAuthorizationRequest,
	suppliedInputID string,
) (Job, error) {
	node, version, _, err := service.resolve(ctx, selector)
	if err != nil {
		return Job{}, err
	}
	profile, err := service.mediaProcessingProfile(selector.Profile)
	if err != nil {
		return Job{}, err
	}
	plan, err := service.planForSource(selector, node, version, profile)
	if err != nil {
		return Job{}, err
	}
	if planFingerprint == "" || planFingerprint != plan.Fingerprint {
		return Job{}, ErrPlanChanged
	}
	want := service.renditionConsentRequest(profile)
	if !sameMediaAuthorization(authorization, want) {
		return Job{}, ErrPlanChanged
	}
	if authorization.PriorAuthorization == nil {
		if _, err := service.catalog.AuthorizeProviderOperation(ctx, authorization); err != nil {
			return Job{}, processingConsentBoundaryError(err)
		}
	}
	inputBinding, err := service.resolveMediaInputBinding(ctx, selector.Profile, version.BlobHash,
		source, suppliedInputID)
	if err != nil {
		return Job{}, err
	}
	job, waiter, err := service.enqueueRendition(ctx, node, version, profile, authorization, inputBinding)
	if err != nil {
		return Job{}, processingConsentBoundaryError(err)
	}
	return Job{ID: waiter.ID, RenditionJobID: job.ID, AttachmentID: waiter.AttachmentID,
		EmbeddingJobIDs: []string{}, ProfileFingerprint: profile.record.Fingerprint,
		ContentVersionID: version.ID}, nil
}

func (service *Service) enqueueRendition(
	ctx context.Context,
	node store.Node,
	version store.ContentVersion,
	profile configuredProfile,
	authorization store.ProviderOperationAuthorizationRequest,
	inputBinding string,
) (store.RenditionJob, store.RenditionJobWaiter, error) {
	prepared, err := service.prepareExecutableRendition(ctx, node, version, profile, inputBinding)
	if err != nil {
		return store.RenditionJob{}, store.RenditionJobWaiter{}, err
	}
	var job store.RenditionJob
	var waiter store.RenditionJobWaiter
	err = service.gate.MutateContext(ctx, func() error {
		var enqueueErr error
		job, waiter, enqueueErr = service.catalog.EnqueueRenditionJob(ctx, store.RenditionJobRequest{
			ContentVersionID: version.ID, Profile: profile.record,
			CapturedArtifactPolicy: prepared.capturedPolicy, ExecutionIdentity: prepared.identity,
			Authorization: authorization,
		})
		return enqueueErr
	})
	return job, waiter, err
}

func (service *Service) resolveMediaInputBinding(
	ctx context.Context, profile, sourceSHA256 string, source mediaSourceBinding, inputID string,
) (string, error) {
	kind, supplied := suppliedInputKind(profile)
	if !supplied {
		if inputID != "" {
			return "", ErrPlanChanged
		}
		return "", nil
	}
	if source.sourceID == "" {
		return "", store.ErrNotFound
	}
	var input store.SuppliedTranscriptInput
	var err error
	if source.sourceVersionID != "" {
		input, err = service.catalog.SuppliedTranscriptForSourceVersion(
			ctx, service.principal, kind, source.sourceID, source.sourceVersionID, inputID)
	} else {
		input, err = service.catalog.SuppliedTranscriptForSourceID(
			ctx, service.principal, kind, source.sourceID, sourceSHA256, inputID)
	}
	if err != nil {
		return "", err
	}
	return input.InputID, nil
}

// completeMediaAdmission binds the rendition job for a processing receipt
// whose admission committed without one. Other receipts return unchanged.
func (service *Service) completeMediaAdmission(
	ctx context.Context, stored store.MediaPublicationReceipt,
) (store.MediaPublicationReceipt, error) {
	if stored.ProcessingProfile == "" || stored.JobID != "" || stored.OperationState != "queued" {
		return stored, nil
	}
	selector := Selector{NodeID: stored.ProcessingNodeID, ContentVersionID: stored.ContentVersionID,
		Profile: stored.ProcessingProfile}
	source := mediaSourceBinding{sourceID: stored.SourceID, sourceVersionID: stored.SourceVersionID}
	plan, err := service.Plan(ctx, selector)
	if err != nil {
		return store.MediaPublicationReceipt{}, errors.Join(err, service.failMediaProcessing(ctx, stored, err))
	}
	job, err := service.EnqueueAuthorized(ctx, selector, source, plan.Fingerprint,
		stored.ProcessingAuthorization, stored.SuppliedInputID)
	if err != nil {
		return store.MediaPublicationReceipt{}, errors.Join(err, service.failMediaProcessing(ctx, stored, err))
	}
	return service.recordMediaProcessingJob(ctx, stored, job.ID)
}

// mediaOperationState derives a queued processing receipt's state from the
// rendition waiter it binds. Terminal outcomes are recorded once by the
// backfill, and a receipt without a job reports what admission stored.
func (service *Service) mediaOperationState(
	ctx context.Context, stored store.MediaPublicationReceipt,
) (store.MediaPublicationReceipt, error) {
	if stored.ProcessingProfile == "" || stored.JobID == "" || stored.OperationState != "queued" {
		return stored, nil
	}
	derived := stored
	derived.OperationState, derived.CoverageState = store.MediaOperationFailed, "unavailable"
	status, err := service.Status(ctx, stored.JobID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Purge and version delete remove waiters; in-flight work then fails.
		return derived, nil
	case err != nil:
		return store.MediaPublicationReceipt{}, err
	}
	switch status.State {
	case statusFailed, statusAbandoned:
		// The backfill can reopen an abandoned or authorization-failed embedding
		// job, so it records embedding outcomes.
		if status.Phase == "embedding" {
			derived.OperationState, derived.CoverageState = "queued", "pending"
		}
		return derived, nil
	case statusCompleted, statusPartial:
		// The backfill fails a receipt whose profile changed after admission.
		profile, ok := service.profiles[stored.ProcessingProfile]
		if !ok || profile.record.Fingerprint != stored.ProcessingProfileFingerprint {
			return derived, nil
		}
		// Embedding profiles finish when the backfill records it, after it rebuilds the index.
		if len(profile.portable.Embeddings) == 0 {
			derived.OperationState, derived.CoverageState = "succeeded", "transcribed"
			return derived, nil
		}
		derived.OperationState, derived.CoverageState = "queued", "pending"
		return derived, nil
	}
	current, err := service.admittedByCurrentIncarnation(ctx, stored.JobID)
	if err != nil {
		return store.MediaPublicationReceipt{}, err
	}
	if current {
		derived.OperationState, derived.CoverageState = "queued", "pending"
	}
	return derived, nil
}

// admittedByCurrentIncarnation reports whether the job's waiter holds authority
// from the current processing incarnation. Restore grants no provider
// authority, so unfinished work admitted by an earlier incarnation cannot finish.
func (service *Service) admittedByCurrentIncarnation(ctx context.Context, jobID string) (bool, error) {
	waiter, err := service.catalog.RenditionJobWaiterByID(ctx, jobID)
	if err != nil {
		return false, err
	}
	incarnation, err := service.catalog.CurrentProcessingIncarnation(ctx)
	if err != nil {
		return false, err
	}
	return waiter.AuthorizationIncarnationID == incarnation.ID, nil
}

// MediaProcessingTargets lists this principal's queued processing receipts.
func (service *Service) MediaProcessingTargets(
	ctx context.Context, after string, limit int,
) ([]store.MediaPublicationReceipt, error) {
	return service.catalog.MediaProcessingContinuations(ctx, service.principal, after, limit)
}

// ContinueMediaProcessing finishes interrupted admission, runs the embeddings a
// completed media rendition needs, and records the terminal outcome.
func (service *Service) ContinueMediaProcessing(ctx context.Context, continuation store.MediaPublicationReceipt) error {
	profile, err := service.mediaProcessingProfile(continuation.ProcessingProfile)
	if err != nil {
		return service.failMediaProcessing(ctx, continuation, err)
	}
	want := service.renditionConsentRequest(profile)
	if continuation.ProcessingPrincipal != service.principal ||
		continuation.ProcessingScope != service.scope ||
		continuation.ProcessingProfileFingerprint != profile.record.Fingerprint ||
		!sameMediaAuthorization(continuation.ProcessingAuthorization, want) ||
		continuation.ProcessingAuthorization.PriorAuthorization == nil {
		return service.failMediaProcessing(ctx, continuation, ErrPlanChanged)
	}
	derived, err := service.mediaOperationState(ctx, continuation)
	if err != nil {
		return err
	}
	// Completed work keeps its outcome when its input is revoked before the backfill runs.
	if derived.OperationState == "succeeded" {
		return service.mediaMutation(context.WithoutCancel(ctx), func() error {
			_, err := service.catalog.FinishMediaProcessing(context.WithoutCancel(ctx),
				continuation.OperationID, continuation.ProcessingPrincipal, true)
			return err
		})
	}
	version, err := service.catalog.ContentVersionByID(ctx, continuation.ContentVersionID)
	if err != nil {
		return service.failMediaProcessing(ctx, continuation, err)
	}
	source := mediaSourceBinding{sourceID: continuation.SourceID, sourceVersionID: continuation.SourceVersionID}
	if _, err := service.resolveMediaInputBinding(ctx, continuation.ProcessingProfile,
		version.BlobHash, source, continuation.SuppliedInputID); err != nil {
		return service.failMediaProcessing(ctx, continuation, err)
	}
	if continuation.JobID == "" {
		_, err = service.completeMediaAdmission(ctx, continuation)
		return err
	}
	status, err := service.Status(ctx, continuation.JobID)
	if err != nil {
		return service.failMediaProcessing(ctx, continuation, err)
	}
	switch {
	case status.State == statusCompleted || status.Phase == "embedding":
		if len(profile.portable.Embeddings) != 0 {
			if _, runErr := service.runEmbeddings(ctx, version, profile,
				continuation.ProcessingPrincipal, continuation.ProcessingScope,
				continuation.ProcessingAuthorization.PriorAuthorization.GrantID, nil); runErr != nil {
				// Consent denial is a job outcome; other errors during shutdown are not.
				if ctx.Err() != nil && !isEmbeddingConsentFailure(runErr) {
					return runErr
				}
				return service.failMediaProcessing(ctx, continuation, runErr)
			}
			status, err = service.Status(ctx, continuation.JobID)
			if err != nil {
				return service.failMediaProcessing(ctx, continuation, err)
			}
			switch status.State {
			case statusFailed, statusAbandoned:
				return service.failMediaProcessing(ctx, continuation, ErrRenditionFailed)
			case statusCompleted, statusPartial:
			default:
				return nil
			}
		}
		return service.mediaMutation(context.WithoutCancel(ctx), func() error {
			_, updateErr := service.catalog.FinishMediaProcessing(context.WithoutCancel(ctx),
				continuation.OperationID, continuation.ProcessingPrincipal, true)
			return updateErr
		})
	case status.State == statusFailed || status.State == statusAbandoned:
		return service.failMediaProcessing(ctx, continuation, ErrRenditionFailed)
	default:
		current, err := service.admittedByCurrentIncarnation(ctx, continuation.JobID)
		if err != nil {
			return service.failMediaProcessing(ctx, continuation, err)
		}
		if !current {
			return service.failMediaProcessing(ctx, continuation, ErrRenditionFailed)
		}
		return nil
	}
}

func sameMediaAuthorization(got, want store.ProviderOperationAuthorizationRequest) bool {
	return got.Principal == want.Principal && got.Scope == want.Scope &&
		got.ProfileFingerprint == want.ProfileFingerprint &&
		got.DisclosureFingerprint == want.DisclosureFingerprint &&
		slices.Equal(got.InputClasses, want.InputClasses) &&
		slices.Equal(got.RetainedArtifactClasses, want.RetainedArtifactClasses)
}

func (service *Service) recordMediaProcessingJob(
	ctx context.Context, receipt store.MediaPublicationReceipt, jobID string,
) (store.MediaPublicationReceipt, error) {
	ctx = context.WithoutCancel(ctx)
	var stored store.MediaPublicationReceipt
	err := service.mediaMutation(ctx, func() error {
		var err error
		stored, err = service.catalog.SetMediaProcessingJob(ctx, receipt.OperationID, receipt.ProcessingPrincipal, jobID)
		return err
	})
	return stored, err
}

func (service *Service) mediaProcessingRetryable(err error) bool {
	return service.catalog.RenditionJobErrorRetryable(err) || errors.Is(err, ErrEmbeddingPersistence)
}

func (service *Service) failMediaProcessing(
	ctx context.Context, continuation store.MediaPublicationReceipt, cause error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if service.mediaProcessingRetryable(cause) {
		return cause
	}
	return service.mediaMutation(context.WithoutCancel(ctx), func() error {
		_, updateErr := service.catalog.FailMediaProcessing(context.WithoutCancel(ctx),
			continuation.OperationID, continuation.ProcessingPrincipal)
		return updateErr
	})
}
