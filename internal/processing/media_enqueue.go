package processing

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/docbank/internal/store"
)

var ErrMediaProcessingUnsupported = errors.New("media processing is unsupported")

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

// MediaContinuationWorker resumes durable enqueue intents and records their
// terminal aggregate state. It performs provider work only through the
// existing supervised rendition and embedding queues.
type MediaContinuationWorker struct {
	Service   *Service
	IdleDelay time.Duration
}

func (worker *MediaContinuationWorker) Run(ctx context.Context) error {
	if worker == nil || worker.Service == nil {
		return errors.New("media continuation service is required")
	}
	delay := worker.IdleDelay
	if delay <= 0 {
		delay = time.Second
	}
	for {
		processed, err := worker.RunOne(ctx)
		if err != nil && (ctx.Err() != nil || !worker.Service.mediaProcessingRetryable(err)) {
			return err
		}
		if processed && err == nil {
			continue
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

func (worker *MediaContinuationWorker) RunOne(ctx context.Context) (bool, error) {
	if worker == nil || worker.Service == nil {
		return false, errors.New("media continuation service is required")
	}
	service := worker.Service
	continuations, err := service.catalog.MediaProcessingContinuations(ctx, 250, service.principal)
	if err != nil || len(continuations) == 0 {
		return false, err
	}
	for _, continuation := range continuations {
		processed, processErr := worker.runContinuation(ctx, continuation)
		if processErr != nil || processed {
			return processed, processErr
		}
	}
	return false, nil
}

func (worker *MediaContinuationWorker) runContinuation(
	ctx context.Context, continuation store.MediaPublicationReceipt,
) (bool, error) {
	service := worker.Service
	profile, err := service.mediaProcessingProfile(continuation.ProcessingProfile)
	if err != nil {
		return worker.failContinuation(ctx, continuation, err)
	}
	want := service.renditionConsentRequest(profile)
	if continuation.ProcessingPrincipal != service.principal ||
		continuation.ProcessingScope != service.scope ||
		continuation.ProcessingProfileFingerprint != profile.record.Fingerprint ||
		!sameMediaAuthorization(continuation.ProcessingAuthorization, want) ||
		continuation.ProcessingAuthorization.PriorAuthorization == nil {
		return worker.failContinuation(ctx, continuation, ErrPlanChanged)
	}
	version, err := service.catalog.ContentVersionByID(ctx, continuation.ContentVersionID)
	if err != nil {
		return worker.failContinuation(ctx, continuation, err)
	}
	source := mediaSourceBinding{sourceID: continuation.SourceID, sourceVersionID: continuation.SourceVersionID}
	if _, err := service.resolveMediaInputBinding(ctx, continuation.ProcessingProfile,
		version.BlobHash, source, continuation.SuppliedInputID); err != nil {
		return worker.failContinuation(ctx, continuation, err)
	}
	if continuation.JobID == "" {
		selector := Selector{NodeID: continuation.ProcessingNodeID,
			ContentVersionID: continuation.ContentVersionID, Profile: continuation.ProcessingProfile}
		plan, err := service.Plan(ctx, selector)
		if err != nil {
			return worker.failContinuation(ctx, continuation, err)
		}
		job, err := service.EnqueueAuthorized(ctx, selector, source, plan.Fingerprint,
			continuation.ProcessingAuthorization, continuation.SuppliedInputID)
		if err != nil {
			return worker.failContinuation(ctx, continuation, err)
		}
		_, err = service.recordMediaProcessingJob(ctx, continuation, job.ID)
		return true, err
	}
	status, err := service.Status(ctx, continuation.JobID)
	if err != nil {
		return worker.failContinuation(ctx, continuation, err)
	}
	switch {
	case status.State == "completed" || status.Phase == "embedding":
		if len(profile.portable.Embeddings) != 0 {
			version, readErr := service.catalog.ContentVersionByID(ctx, continuation.ContentVersionID)
			if readErr != nil {
				return worker.failContinuation(ctx, continuation, readErr)
			}
			if _, runErr := service.runEmbeddings(ctx, version, profile,
				continuation.ProcessingPrincipal, continuation.ProcessingScope,
				continuation.ProcessingAuthorization.PriorAuthorization.GrantID, nil); runErr != nil {
				if ctx.Err() != nil {
					return false, runErr
				}
				return worker.failContinuation(ctx, continuation, runErr)
			}
			status, err = service.Status(ctx, continuation.JobID)
			if err != nil {
				return worker.failContinuation(ctx, continuation, err)
			}
			switch status.State {
			case "failed", "abandoned":
				return worker.failContinuation(ctx, continuation, ErrRenditionFailed)
			case "completed", "partial":
			default:
				return false, nil
			}
		}
		err = service.mediaMutation(context.WithoutCancel(ctx), func() error {
			_, updateErr := service.catalog.FinishMediaProcessing(context.WithoutCancel(ctx),
				continuation.OperationID, continuation.ProcessingPrincipal, true)
			return updateErr
		})
		return true, err
	case status.State == "failed" || status.State == "abandoned":
		return worker.failContinuation(ctx, continuation, ErrRenditionFailed)
	default:
		return false, nil
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

func (worker *MediaContinuationWorker) failContinuation(
	ctx context.Context, continuation store.MediaPublicationReceipt, cause error,
) (bool, error) {
	err := worker.Service.failMediaProcessing(ctx, continuation, cause)
	return err == nil, err
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
