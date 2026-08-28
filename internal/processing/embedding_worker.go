package processing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

var (
	// ErrEmbeddingWorkFenced means an expired or superseded attempt lost its
	// lease or an immutable source/profile/input fence.
	ErrEmbeddingWorkFenced = errors.New("embedding work is fenced")
	// ErrEmbeddingPersistence is deliberately body-free: storage errors may
	// contain private paths or provider-derived values and must not escape the
	// worker boundary.
	ErrEmbeddingPersistence = errors.New("embedding persistence failed")
	// ErrEmbeddingRuntimeUnavailable means no executable adapter is registered
	// for the work's exact descriptor fingerprint.
	ErrEmbeddingRuntimeUnavailable = errors.New("embedding runtime is unavailable")
)

// EmbeddingWorkClaim is the store-owned durable claim authority.
type EmbeddingWorkClaim = store.EmbeddingJobClaim

// EmbeddingWork is the store-owned durable execution authority.
type EmbeddingWork = store.EmbeddingJobWork

// EmbeddingAttemptReceipt is the store-owned sanitized attempt receipt.
type EmbeddingAttemptReceipt = store.EmbeddingAttemptReceipt

type embeddingWorkerCatalog interface {
	ReconcileEmbeddingJobs(ctx context.Context, request store.EmbeddingReconcileRequest) (store.EmbeddingReconcileResult, error)
	ClaimNextEmbeddingWork(ctx context.Context, owner string, at time.Time, lease time.Duration) (EmbeddingWorkClaim, EmbeddingWork, bool, error)
	RenewEmbeddingWork(ctx context.Context, claim EmbeddingWorkClaim, at time.Time, lease time.Duration) (EmbeddingWorkClaim, error)
	ValidateEmbeddingWork(ctx context.Context, claim EmbeddingWorkClaim, work EmbeddingWork, at time.Time) error
	ReauthorizeEmbeddingWork(ctx context.Context, claim EmbeddingWorkClaim, work EmbeddingWork, prior *store.ProviderOperationAuthorization, at time.Time) (store.ProviderOperationAuthorization, error)
	FinishEmbeddingWork(ctx context.Context, claim EmbeddingWorkClaim, receipt EmbeddingAttemptReceipt, at time.Time) error
	FailEmbeddingWork(ctx context.Context, claim EmbeddingWorkClaim, work EmbeddingWork, code store.EmbeddingFailureCode, receipt EmbeddingAttemptReceipt, at time.Time) error
}

type targetedEmbeddingWorkerCatalog interface {
	ClaimEmbeddingWork(ctx context.Context, jobID, owner string, at time.Time,
		lease time.Duration) (EmbeddingWorkClaim, EmbeddingWork, bool, error)
}

type embeddingWorkerAuthority interface {
	RecordRenditionBlob(ctx context.Context, hash string, size int64, physical store.BlobPhysical) error
	StageEmbeddingSetWithLease(ctx context.Context, record store.EmbeddingSetRecord, rootID string, fencingToken int64, at time.Time) error
	PublishEmbeddingHeadWithLease(ctx context.Context, record store.EmbeddingHeadRecord,
		consent store.ProviderOperationAuthorizationRequest, prior store.ProviderOperationAuthorization,
		rootID string, fencingToken int64, at time.Time) (store.ProviderOperationAuthorization, error)
}

// EmbeddingExecution contains only transient provider material.
type EmbeddingExecution struct {
	Provider            document.EmbeddingProvider
	Inputs              []document.EmbeddingInput
	InputGenerationJSON []byte
	Classify            func(error) (EmbeddingProviderFailure, time.Duration)
}

// EmbeddingProviderFailure is a provider-neutral retry classification.
type EmbeddingProviderFailure string

const (
	EmbeddingProviderTransient EmbeddingProviderFailure = "transient"
	EmbeddingProviderPermanent EmbeddingProviderFailure = "permanent"
	EmbeddingProviderCapacity  EmbeddingProviderFailure = "capacity"
)

// EmbeddingRuntime reconstructs transient inputs and provider clients from an
// exact work item. Classify must never return or persist a provider body.
type EmbeddingRuntime interface {
	Ready() bool
	Prepare(ctx context.Context, work EmbeddingWork) (EmbeddingExecution, error)
	Classify(err error) (EmbeddingProviderFailure, time.Duration)
}

// EmbeddingRuntimeRegistry resolves exact immutable descriptor fingerprints.
type EmbeddingRuntimeRegistry struct {
	mu       sync.RWMutex
	runtimes map[string]EmbeddingRuntime
}

func (registry *EmbeddingRuntimeRegistry) Fingerprints() []string {
	if registry == nil {
		return nil
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	values := make([]string, 0, len(registry.runtimes))
	for value := range registry.runtimes {
		values = append(values, value)
	}
	slices.Sort(values)
	return values
}

func NewEmbeddingRuntimeRegistry() *EmbeddingRuntimeRegistry {
	return &EmbeddingRuntimeRegistry{runtimes: make(map[string]EmbeddingRuntime)}
}

func (registry *EmbeddingRuntimeRegistry) Ready() bool {
	if registry == nil {
		return false
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	return len(registry.runtimes) != 0
}

func (registry *EmbeddingRuntimeRegistry) Register(fingerprint string, runtime EmbeddingRuntime) error {
	if registry == nil || len(fingerprint) != sha256.Size*2 || embeddingInterfaceNil(runtime) || !runtime.Ready() {
		return errors.New("embedding runtime registration is invalid")
	}
	if _, err := hex.DecodeString(fingerprint); err != nil {
		return errors.New("embedding runtime descriptor fingerprint is invalid")
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, exists := registry.runtimes[fingerprint]; exists {
		return errors.New("embedding runtime descriptor is already registered")
	}
	registry.runtimes[fingerprint] = runtime
	return nil
}

func (registry *EmbeddingRuntimeRegistry) Prepare(ctx context.Context, work EmbeddingWork) (EmbeddingExecution, error) {
	if registry == nil {
		return EmbeddingExecution{}, ErrEmbeddingRuntimeUnavailable
	}
	registry.mu.RLock()
	runtime := registry.runtimes[work.Descriptor.Fingerprint]
	registry.mu.RUnlock()
	if embeddingInterfaceNil(runtime) {
		return EmbeddingExecution{}, ErrEmbeddingRuntimeUnavailable
	}
	execution, err := runtime.Prepare(ctx, work)
	if err == nil {
		execution.Classify = runtime.Classify
	}
	return execution, err
}

// ResolveQueryEncoder implements retrieval.QueryEncoderResolver without
// weakening the descriptor identity stored with the vector space.
func (registry *EmbeddingRuntimeRegistry) ResolveQueryEncoder(_ context.Context,
	descriptor document.EmbeddingDescriptor,
) (document.EmbeddingProvider, error) {
	if registry == nil {
		return nil, ErrEmbeddingRuntimeUnavailable
	}
	registry.mu.RLock()
	runtime := registry.runtimes[descriptor.Fingerprint]
	registry.mu.RUnlock()
	providerRuntime, ok := runtime.(*ProviderEmbeddingRuntime)
	if !ok {
		return nil, ErrEmbeddingRuntimeUnavailable
	}
	return providerRuntime.QueryProvider(descriptor)
}

func (registry *EmbeddingRuntimeRegistry) Classify(err error) (EmbeddingProviderFailure, time.Duration) {
	// Registry preparation binds classification to the selected execution.
	// There is no safe cross-runtime classification for an unbound error.
	return EmbeddingProviderPermanent, 0
}

// EmbeddingWorkerConfig binds the provider-neutral worker to one vault.
type EmbeddingWorkerConfig struct {
	Catalog                embeddingWorkerCatalog
	Authority              embeddingWorkerAuthority
	Blobs                  renditionBlobWriter
	GenerationBlobs        embeddingRuntimeBlobs
	Runtime                EmbeddingRuntime
	Gate                   RenditionMutationGate
	Owner                  string
	LeaseDuration          time.Duration
	IdleDelay              time.Duration
	RetryLimit             int
	RetryBaseDelay         time.Duration
	MaxRetryDelay          time.Duration
	AttemptLifetime        time.Duration
	MaxRows                int
	MaxDimensions          int
	MaxVectorBlobBytes     int64
	Clock                  func() time.Time
	Wait                   func(context.Context, time.Duration) error
	DescriptorFingerprints []string
}

// EmbeddingWorker publishes each binding head independently.
type EmbeddingWorker struct {
	catalog                                        embeddingWorkerCatalog
	authority                                      embeddingWorkerAuthority
	blobs                                          renditionBlobWriter
	generationBlobs                                embeddingRuntimeBlobs
	runtime                                        EmbeddingRuntime
	gate                                           RenditionMutationGate
	owner                                          string
	leaseDuration, idleDelay                       time.Duration
	retryLimit                                     int
	retryBaseDelay, maxRetryDelay, attemptLifetime time.Duration
	maxRows, maxDimensions                         int
	maxVectorBlobBytes                             int64
	clock                                          func() time.Time
	wait                                           func(context.Context, time.Duration) error
	descriptorFingerprints                         []string
	reconcileAfter                                 string
}

func NewEmbeddingWorker(config EmbeddingWorkerConfig) (*EmbeddingWorker, error) {
	if embeddingInterfaceNil(config.Catalog) || embeddingInterfaceNil(config.Authority) || embeddingInterfaceNil(config.Blobs) ||
		embeddingInterfaceNil(config.GenerationBlobs) ||
		embeddingInterfaceNil(config.Runtime) || !config.Runtime.Ready() || embeddingInterfaceNil(config.Gate) {
		return nil, errors.New("embedding worker requires catalog, blob store, ready runtime, and operation gate")
	}
	if config.Owner == "" || len(config.Owner) > 128 {
		return nil, errors.New("embedding worker owner is invalid")
	}
	if config.LeaseDuration < 3*time.Second || config.LeaseDuration > time.Hour ||
		config.IdleDelay <= 0 || config.IdleDelay > time.Minute {
		return nil, errors.New("embedding worker lease or idle delay is invalid")
	}
	if config.RetryLimit < 1 || config.RetryLimit > 10 || config.RetryBaseDelay < 0 ||
		config.MaxRetryDelay <= 0 || config.MaxRetryDelay > time.Minute ||
		config.RetryBaseDelay > config.MaxRetryDelay {
		return nil, errors.New("embedding worker retry policy is invalid")
	}
	if config.AttemptLifetime < config.LeaseDuration || config.AttemptLifetime > 24*time.Hour ||
		config.MaxRows < 1 || config.MaxRows > 100_000 || config.MaxDimensions < 1 ||
		config.MaxDimensions > 1_048_576 || config.MaxVectorBlobBytes < 1 || config.MaxVectorBlobBytes > 64<<20 {
		return nil, errors.New("embedding worker attempt or payload bounds are invalid")
	}
	if len(config.DescriptorFingerprints) == 0 {
		return nil, errors.New("embedding worker requires executable descriptor fingerprints")
	}
	if config.Clock == nil {
		config.Clock = func() time.Time { return time.Now().UTC() }
	}
	if config.Wait == nil {
		config.Wait = waitEmbeddingWorker
	}
	return &EmbeddingWorker{
		catalog: config.Catalog, authority: config.Authority, blobs: config.Blobs,
		generationBlobs: config.GenerationBlobs, runtime: config.Runtime, gate: config.Gate,
		owner: config.Owner, leaseDuration: config.LeaseDuration, idleDelay: config.IdleDelay,
		retryLimit: config.RetryLimit, retryBaseDelay: config.RetryBaseDelay,
		maxRetryDelay: config.MaxRetryDelay, attemptLifetime: config.AttemptLifetime,
		maxRows: config.MaxRows, maxDimensions: config.MaxDimensions,
		maxVectorBlobBytes: config.MaxVectorBlobBytes, clock: config.Clock, wait: config.Wait,
		descriptorFingerprints: slices.Clone(config.DescriptorFingerprints),
	}, nil
}

// Run remains alive across an empty queue and exits promptly on cancellation.
func (worker *EmbeddingWorker) Run(ctx context.Context) error {
	if worker == nil {
		return errors.New("embedding worker is nil")
	}
	for {
		processed, err := worker.ScanOnce(ctx)
		if err != nil {
			return err
		}
		if processed == 0 {
			if err := worker.wait(ctx, worker.idleDelay); err != nil {
				return err
			}
		}
	}
}

// ScanOnce drains the currently eligible queue snapshot. Each claim is a
// separate publication unit; one failed binding cannot roll back a sibling.
func (worker *EmbeddingWorker) ScanOnce(ctx context.Context) (int, error) {
	if worker == nil {
		return 0, errors.New("embedding worker is nil")
	}
	processed := 0
	reconciled := false
	for {
		if err := ctx.Err(); err != nil {
			return processed, err
		}
		found := false
		if err := worker.gate.MutateContext(ctx, func() error {
			if !reconciled {
				result, err := worker.catalog.ReconcileEmbeddingJobs(ctx, store.EmbeddingReconcileRequest{
					After: worker.reconcileAfter, Limit: 100, At: worker.clock().UTC(),
					DescriptorFingerprints: worker.descriptorFingerprints,
					HydrateGeneration: func(ctx context.Context, generation store.EmbeddingInputGenerationRecord) (store.EmbeddingInputGenerationRecord, error) {
						return hydrateEmbeddingGeneration(ctx, worker.generationBlobs, generation)
					},
				})
				if err != nil {
					return ErrEmbeddingPersistence
				}
				worker.reconcileAfter = result.Next
				reconciled = true
			}
			claim, work, claimed, err := worker.catalog.ClaimNextEmbeddingWork(
				ctx, worker.owner, worker.clock().UTC(), worker.leaseDuration)
			if err != nil {
				return ErrEmbeddingPersistence
			}
			found = claimed
			if !found {
				return nil
			}
			processed++
			return worker.processClaim(ctx, claim, work)
		}); err != nil {
			if ctx.Err() != nil {
				return processed, ctx.Err()
			}
			if isEmbeddingWorkFence(err) {
				continue
			}
			return processed, err
		}
		if !found {
			return processed, nil
		}
	}
}

// RunJob processes one exact ready embedding job without consuming unrelated
// provider work from the vault queue.
func (worker *EmbeddingWorker) RunJob(ctx context.Context, jobID string) (bool, error) {
	if worker == nil {
		return false, errors.New("embedding worker is nil")
	}
	target, ok := worker.catalog.(targetedEmbeddingWorkerCatalog)
	if !ok {
		return false, errors.New("embedding catalog does not support targeted claims")
	}
	processed := false
	err := worker.gate.MutateContext(ctx, func() error {
		claim, work, found, claimErr := target.ClaimEmbeddingWork(ctx, jobID, worker.owner,
			worker.clock().UTC(), worker.leaseDuration)
		if claimErr != nil || !found {
			return claimErr
		}
		processed = true
		return worker.processClaim(ctx, claim, work)
	})
	if err != nil {
		if ctx.Err() != nil {
			return processed, ctx.Err()
		}
		if isEmbeddingWorkFence(err) {
			return processed, nil
		}
	}
	return processed, err
}

func (worker *EmbeddingWorker) processClaim(ctx context.Context, claim EmbeddingWorkClaim, work EmbeddingWork) (retErr error) {
	started := worker.clock().UTC()
	attemptCtx, cancelAttempt := context.WithTimeout(ctx, worker.attemptLifetime)
	defer cancelAttempt()
	receipt := EmbeddingAttemptReceipt{AttemptID: claim.AttemptID, ProviderFingerprint: work.Descriptor.Fingerprint,
		ProfileFingerprint: work.ProcessingProfile.Fingerprint, BindingID: work.Binding.Name,
		InputKind: work.Binding.InputKind, Rows: len(work.InputGeneration.Inputs), Dimensions: work.Descriptor.Dimension}
	leaseCtx, stopLease := worker.keepEmbeddingLease(attemptCtx, claim)
	defer func() { retErr = errors.Join(retErr, stopLease()) }()
	if err := validateEmbeddingWork(work, worker.maxRows, worker.maxDimensions); err != nil {
		return worker.failClaim(leaseCtx, claim, work, store.EmbeddingFailureStaleAuthority, &receipt, started)
	}

	materializedGeneration := work.InputGeneration
	result, authorization, code, err := worker.executeBatches(leaseCtx, claim, work, &materializedGeneration, &receipt, started)
	if err != nil {
		if leaseCtx.Err() != nil {
			return fmt.Errorf("embedding provider work canceled: %w", context.Cause(leaseCtx))
		}
		return worker.failClaim(leaseCtx, claim, work, code, &receipt, started)
	}
	if err := worker.catalog.ValidateEmbeddingWork(leaseCtx, claim, work, worker.clock().UTC()); err != nil {
		if isEmbeddingWorkFence(err) {
			return ErrEmbeddingWorkFenced
		}
		return worker.failClaim(leaseCtx, claim, work, store.EmbeddingFailureStaleAuthority, &receipt, started)
	}
	work.InputGeneration = materializedGeneration
	record, payload, err := buildEmbeddingSet(work, result, worker.clock().UTC())
	if err != nil || int64(len(payload)) > worker.maxVectorBlobBytes {
		return worker.failClaim(leaseCtx, claim, work, store.EmbeddingFailureInvalidResponse, &receipt, started)
	}
	err = worker.persistEmbeddingSet(leaseCtx, claim, record, payload)
	if err != nil {
		if isEmbeddingWorkFence(err) {
			return ErrEmbeddingWorkFenced
		}
		if leaseCtx.Err() != nil {
			return fmt.Errorf("embedding persistence canceled: %w", context.Cause(leaseCtx))
		}
		if errors.Is(err, errEmbeddingReceiptMismatch) {
			return worker.failClaim(leaseCtx, claim, work, store.EmbeddingFailureInvalidResponse, &receipt, started)
		}
		return ErrEmbeddingPersistence
	}
	head := store.EmbeddingHeadRecord{Key: store.EmbeddingHeadKey{ContentVersionID: work.ContentVersionID,
		BindingID: work.Binding.Name, InputKind: work.Binding.InputKind}, SetID: record.ID,
		VectorSpaceID: work.VectorSpaceID, ProcessingProfileFingerprint: work.ProcessingProfile.Fingerprint,
		PublishedAt: metadataEmbeddingTime(worker.clock().UTC())}
	if _, err := worker.authority.PublishEmbeddingHeadWithLease(leaseCtx, head, work.Consent,
		authorization, claim.AttemptID, claim.Epoch, worker.clock().UTC()); err != nil {
		if isEmbeddingWorkFence(err) {
			return ErrEmbeddingWorkFenced
		}
		return worker.failClaim(leaseCtx, claim, work, store.EmbeddingFailureStaleAuthority, &receipt, started)
	}
	receipt.Elapsed = worker.clock().UTC().Sub(started)
	if err := worker.catalog.FinishEmbeddingWork(leaseCtx, claim, receipt, worker.clock().UTC()); err != nil {
		if isEmbeddingWorkFence(err) {
			return ErrEmbeddingWorkFenced
		}
		return ErrEmbeddingPersistence
	}
	return nil
}

func (worker *EmbeddingWorker) executeBatches(ctx context.Context, claim EmbeddingWorkClaim, work EmbeddingWork,
	materializedGeneration *store.EmbeddingInputGenerationRecord, receipt *EmbeddingAttemptReceipt, started time.Time,
) (document.EmbeddingResult, store.ProviderOperationAuthorization, store.EmbeddingFailureCode, error) {
	batchSize := min(work.Binding.MaxBatchItems, worker.maxRows)
	result := document.EmbeddingResult{Vectors: make([]document.EmbeddingVector, 0, len(work.InputGeneration.Inputs))}
	var prior store.ProviderOperationAuthorization
	for offset := 0; offset < len(work.InputGeneration.Inputs); {
		end := min(offset+batchSize, len(work.InputGeneration.Inputs))
		var batchResult document.EmbeddingResult
		transientAttempts := 0
		for {
			if worker.clock().UTC().Sub(started) > worker.attemptLifetime {
				return document.EmbeddingResult{}, prior, store.EmbeddingFailureProviderUnavailable, errors.New("attempt expired")
			}
			if err := worker.catalog.ValidateEmbeddingWork(ctx, claim, work, worker.clock().UTC()); err != nil {
				return document.EmbeddingResult{}, prior, store.EmbeddingFailureStaleAuthority, err
			}
			execution, err := worker.runtime.Prepare(ctx, work)
			if err != nil || embeddingInterfaceNil(execution.Provider) {
				closeEmbeddingInputs(execution.Inputs)
				return document.EmbeddingResult{}, prior, store.EmbeddingFailureProviderUnavailable, ErrEmbeddingRuntimeUnavailable
			}
			hydrated, err := validateEmbeddingExecution(work, execution)
			if err != nil {
				closeEmbeddingInputs(execution.Inputs)
				return document.EmbeddingResult{}, prior, store.EmbeddingFailureInvalidResponse, err
			}
			*materializedGeneration = hydrated
			end, err = boundedEmbeddingBatchEnd(execution.Provider, work, execution.Inputs, offset, end)
			if err != nil {
				closeEmbeddingInputs(execution.Inputs)
				return document.EmbeddingResult{}, prior, store.EmbeddingFailureInputRejected, err
			}
			batch := execution.Inputs[offset:end]
			priorPtr := (*store.ProviderOperationAuthorization)(nil)
			if prior.GrantID != "" {
				priorPtr = &prior
			}
			authorized, err := worker.catalog.ReauthorizeEmbeddingWork(ctx, claim, work, priorPtr, worker.clock().UTC())
			if err != nil {
				closeEmbeddingInputs(execution.Inputs)
				return document.EmbeddingResult{}, prior, store.EmbeddingFailureAuthorization, err
			}
			prior = authorized
			authorization := document.EmbeddingAuthorization{ProviderID: work.Descriptor.ID,
				DescriptorFingerprint: work.Descriptor.Fingerprint, PolicyFingerprint: work.Descriptor.PolicyFingerprint,
				MaxBatchItems: min(work.Binding.MaxBatchItems, len(batch)), MaxInputBytes: work.Binding.MaxInputBytes,
				MaxResponseBytes: work.Binding.MaxResponseBytes}
			if err := document.ValidateEmbeddingProviderRequest(execution.Provider, batch, authorization); err != nil {
				closeEmbeddingInputs(execution.Inputs)
				return document.EmbeddingResult{}, prior, store.EmbeddingFailureInputRejected, err
			}
			receipt.ProviderCalls++
			batchResult, err = execution.Provider.Embed(ctx, batch, authorization)
			closeEmbeddingInputs(execution.Inputs)
			if err == nil {
				if err := document.ValidateEmbeddingProviderResult(work.Descriptor, batch, authorization, batchResult); err != nil {
					return document.EmbeddingResult{}, prior, store.EmbeddingFailureInvalidResponse, err
				}
				batchResult = normalizeEmbeddingProviderResult(batch, batchResult)
				break
			}
			if ctx.Err() != nil {
				return document.EmbeddingResult{}, prior, store.EmbeddingFailureProviderUnavailable, ctx.Err()
			}
			classification, retryAfter := execution.Classify(err)
			if classification == EmbeddingProviderCapacity {
				if len(batch) > 1 {
					batchSize = max(1, len(batch)/2)
					end = offset + batchSize
					receipt.Retries++
					continue
				}
				return document.EmbeddingResult{}, prior, store.EmbeddingFailureInputRejected, err
			}
			if classification != EmbeddingProviderTransient {
				return document.EmbeddingResult{}, prior, store.EmbeddingFailureInputRejected, err
			}
			transientAttempts++
			if transientAttempts == worker.retryLimit {
				return document.EmbeddingResult{}, prior, store.EmbeddingFailureProviderUnavailable, err
			}
			receipt.Retries++
			delay := min(worker.retryBaseDelay*(1<<(transientAttempts-1)), worker.maxRetryDelay)
			if retryAfter > 0 {
				delay = min(retryAfter, worker.maxRetryDelay)
			}
			if err := worker.wait(ctx, delay); err != nil {
				return document.EmbeddingResult{}, prior, store.EmbeddingFailureProviderUnavailable, err
			}
		}
		result.Vectors = append(result.Vectors, batchResult.Vectors...)
		offset = end
	}
	return result, prior, "", nil
}

func boundedEmbeddingBatchEnd(provider document.EmbeddingProvider, work EmbeddingWork,
	inputs []document.EmbeddingInput, offset, upper int,
) (int, error) {
	vectorBytes := int64(work.Descriptor.Dimension) * 4
	if vectorBytes < 1 || work.Binding.MaxResponseBytes < vectorBytes {
		return 0, errors.New("embedding response byte authority cannot hold one vector")
	}
	upper = min(upper, offset+int(work.Binding.MaxResponseBytes/vectorBytes))
	if upper <= offset {
		return 0, errors.New("embedding batch response authority is invalid")
	}
	valid := func(end int) error {
		authorization := document.EmbeddingAuthorization{ProviderID: work.Descriptor.ID,
			DescriptorFingerprint: work.Descriptor.Fingerprint, PolicyFingerprint: work.Descriptor.PolicyFingerprint,
			MaxBatchItems: end - offset, MaxInputBytes: work.Binding.MaxInputBytes,
			MaxResponseBytes: work.Binding.MaxResponseBytes}
		return document.ValidateEmbeddingProviderRequest(provider, inputs[offset:end], authorization)
	}
	if err := valid(offset + 1); err != nil {
		return 0, err
	}
	low := offset + 1
	for high := upper; low < high; {
		middle := low + (high-low+1)/2
		if err := valid(middle); err != nil {
			high = middle - 1
		} else {
			low = middle
		}
	}
	return low, nil
}

func normalizeEmbeddingProviderResult(inputs []document.EmbeddingInput, result document.EmbeddingResult) document.EmbeddingResult {
	if len(result.Vectors) == 0 || result.Vectors[0].Index == nil {
		return result
	}
	ordered := make([]document.EmbeddingVector, len(inputs))
	for _, vector := range result.Vectors {
		ordered[*vector.Index] = vector
	}
	result.Vectors = ordered
	return result
}

var errEmbeddingReceiptMismatch = errors.New("embedding blob receipt mismatch")

func (worker *EmbeddingWorker) persistEmbeddingSet(ctx context.Context, claim EmbeddingWorkClaim,
	record store.EmbeddingSetRecord, payload []byte,
) error {
	return worker.blobs.WithMutation(ctx, func() error {
		receipt, writeErr := worker.blobs.WriteDetailedContext(ctx, bytes.NewReader(payload))
		if writeErr != nil {
			return writeErr
		}
		if receipt.Hash != record.VectorSet.PayloadBlobHash || receipt.Size != int64(len(payload)) {
			return errEmbeddingReceiptMismatch
		}
		encoding, err := receipt.EncodingName()
		if err != nil {
			return err
		}
		physical := store.BlobPhysical{Encoding: encoding, StoredBytes: receipt.StoredSize,
			PackEligible: receipt.PackEligible, Created: receipt.Created}
		if err := worker.authority.RecordRenditionBlob(ctx, receipt.Hash, receipt.Size, physical); err != nil {
			return err
		}
		return worker.authority.StageEmbeddingSetWithLease(ctx, record,
			claim.AttemptID, claim.Epoch, worker.clock().UTC())
	})
}

func (worker *EmbeddingWorker) failClaim(ctx context.Context, claim EmbeddingWorkClaim, work EmbeddingWork,
	code store.EmbeddingFailureCode, receipt *EmbeddingAttemptReceipt, started time.Time,
) error {
	receipt.FailureCode = string(code)
	receipt.Elapsed = worker.clock().UTC().Sub(started)
	if err := worker.catalog.FailEmbeddingWork(ctx, claim, work, code, *receipt, worker.clock().UTC()); err != nil {
		if isEmbeddingWorkFence(err) {
			return ErrEmbeddingWorkFenced
		}
		if ctx.Err() != nil {
			return fmt.Errorf("embedding failure recording canceled: %w", context.Cause(ctx))
		}
		return ErrEmbeddingPersistence
	}
	return nil
}

func isEmbeddingWorkFence(err error) bool {
	return errors.Is(err, ErrEmbeddingWorkFenced) ||
		errors.Is(err, store.ErrEmbeddingJobFenced) ||
		errors.Is(err, store.ErrCurrentRenditionRootFenced)
}

func (worker *EmbeddingWorker) keepEmbeddingLease(ctx context.Context, claim EmbeddingWorkClaim) (context.Context, func() error) {
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
				_, err := worker.catalog.RenewEmbeddingWork(leaseCtx, claim, worker.clock().UTC(), worker.leaseDuration)
				if err != nil {
					cancel(ErrEmbeddingWorkFenced)
					done <- ErrEmbeddingWorkFenced
					return
				}
			}
		}
	}()
	var once sync.Once
	var result error
	return leaseCtx, func() error {
		once.Do(func() {
			close(stop)
			result = <-done
			cancel(nil)
		})
		return result
	}
}

func validateEmbeddingWork(work EmbeddingWork, maxRows, maxDimensions int) error {
	descriptor, err := document.NewEmbeddingDescriptor(work.Descriptor)
	if err != nil || !reflect.DeepEqual(descriptor, work.Descriptor) {
		return errors.New("embedding descriptor is not canonical")
	}
	if descriptor.Dimension > maxDimensions || len(work.InputGeneration.Inputs) > maxRows ||
		len(work.InputGeneration.Inputs) == 0 || descriptor.Fingerprint != work.Binding.Descriptor.Fingerprint ||
		descriptor.ID != work.Binding.Descriptor.ID || descriptor.Dimension != work.Binding.Dimensions ||
		descriptor.CompatibilityID != work.Binding.CompatibilityID || descriptor.Model != work.Binding.Model ||
		descriptor.Metric != work.Binding.Metric || descriptor.Normalization != work.Binding.Normalization ||
		descriptor.ScalarEncoding != work.Binding.ScalarEncoding || descriptor.ModelInput.Fingerprint != work.Binding.ModelInput.Fingerprint {
		return errors.New("embedding descriptor does not match binding")
	}
	var profile document.ProcessingProfileV1
	if err := json.Unmarshal(work.ProcessingProfile.CanonicalProfile, &profile); err != nil {
		return errors.New("embedding processing profile is invalid")
	}
	canonical, fingerprints, err := document.CanonicalProfile(profile)
	if err != nil || !bytes.Equal(canonical, work.ProcessingProfile.CanonicalProfile) ||
		fingerprints.Profile != work.ProcessingProfile.Fingerprint || fingerprints.VectorSpace[work.Binding.Name] != work.VectorSpaceID {
		return errors.New("embedding processing profile authority drifted")
	}
	found := false
	for _, binding := range profile.Embeddings {
		if binding.Name == work.Binding.Name && reflect.DeepEqual(binding, work.Binding) {
			found = true
		}
	}
	if !found || work.InputGeneration.SourceVersionID != work.ContentVersionID ||
		work.InputGeneration.ProcessingProfileFingerprint != work.ProcessingProfile.Fingerprint {
		return errors.New("embedding binding or generation authority drifted")
	}
	return nil
}

func validateEmbeddingExecution(work EmbeddingWork, execution EmbeddingExecution) (store.EmbeddingInputGenerationRecord, error) {
	if len(execution.Inputs) != len(work.InputGeneration.Inputs) ||
		!reflect.DeepEqual(execution.Provider.Descriptor(), work.Descriptor) || execution.Classify == nil {
		return store.EmbeddingInputGenerationRecord{}, errors.New("embedding execution does not match work")
	}
	for index, input := range execution.Inputs {
		reference := work.InputGeneration.Inputs[index]
		if input.Key != reference.ID || input.Kind != work.Binding.InputKind || input.Role != document.EmbeddingRoleDocument {
			return store.EmbeddingInputGenerationRecord{}, errors.New("embedding execution input order or role drifted")
		}
		if input.Kind == document.EmbeddingInputRenditionChunk {
			if workerHashString(work.Binding.ModelInput.EncodeDocument(input.Text)) != reference.RenderedChecksum {
				return store.EmbeddingInputGenerationRecord{}, errors.New("embedding execution text checksum drifted")
			}
		} else if embeddingInterfaceNil(input.Source) || input.Source.Metadata().SHA256 != reference.RenderedChecksum {
			return store.EmbeddingInputGenerationRecord{}, errors.New("embedding execution original checksum drifted")
		}
	}
	if work.Binding.InputKind != document.EmbeddingInputRenditionChunk || work.InputGeneration.GenerationBlobHash == "" {
		return work.InputGeneration, nil
	}
	hydrated, err := store.HydrateEmbeddingInputGeneration(work.InputGeneration, execution.InputGenerationJSON)
	if err != nil {
		return store.EmbeddingInputGenerationRecord{}, err
	}
	return hydrated, nil
}

func buildEmbeddingSet(work EmbeddingWork, result document.EmbeddingResult, now time.Time) (store.EmbeddingSetRecord, []byte, error) {
	keys := make([]string, len(work.InputGeneration.Inputs))
	checksums := make([]string, len(keys))
	values := make([][]float64, len(keys))
	for index, input := range work.InputGeneration.Inputs {
		keys[index], checksums[index] = input.ID, input.RenderedChecksum
		values[index] = make([]float64, len(result.Vectors[index].Values))
		for column, value := range result.Vectors[index].Values {
			values[index][column] = float64(value)
		}
	}
	set, err := document.NewVectorSetV1(document.VectorSetV1Input{VectorSpaceFingerprint: work.VectorSpaceID,
		Metric: work.Descriptor.Metric, Normalization: work.Descriptor.Normalization,
		Dimension: work.Descriptor.Dimension, InputKeys: keys, InputChecksums: checksums, Values: values})
	if err != nil {
		return store.EmbeddingSetRecord{}, nil, err
	}
	payload, checksum, err := document.EncodeVectorSetV1(set)
	if err != nil {
		return store.EmbeddingSetRecord{}, nil, err
	}
	decoded, err := document.DecodeVectorSetV1(payload, document.VectorBounds{MaxRows: len(keys), MaxDimension: work.Descriptor.Dimension, MaxBytes: len(payload)})
	if err != nil {
		return store.EmbeddingSetRecord{}, nil, err
	}
	verified, verifiedChecksum, err := document.EncodeVectorSetV1(decoded)
	if err != nil || verifiedChecksum != checksum || !bytes.Equal(verified, payload) {
		return store.EmbeddingSetRecord{}, nil, errors.New("canonical vector-set verification failed")
	}
	space := store.EmbeddingVectorSpaceRecord{ID: work.VectorSpaceID,
		ContractVersion: store.EmbeddingVectorSpaceContractV1, Descriptor: work.Descriptor}
	vectorSet := store.EmbeddingVectorSetRecord{ID: checksum, ContractVersion: store.EmbeddingVectorSetContractV1,
		VectorSpaceID: work.VectorSpaceID, PayloadBlobHash: workerHashBytes(payload), PayloadSize: int64(len(payload)),
		PayloadChecksum: checksum, Payload: payload}
	setID := workerHashString("embedding-set/v1\x00" + work.VaultID + "\x00" + work.ContentVersionID + "\x00" +
		work.ProcessingProfile.Fingerprint + "\x00" + work.Binding.Name + "\x00" + string(work.Binding.InputKind) + "\x00" +
		work.InputGeneration.ID + "\x00" + checksum)
	record := store.EmbeddingSetRecord{ID: setID, VaultID: work.VaultID, BindingID: work.Binding.Name,
		InputKind: work.Binding.InputKind, ContentVersionID: work.ContentVersionID,
		ProcessingProfileFingerprint: work.ProcessingProfile.Fingerprint,
		EmbeddingInputFingerprint:    work.EmbeddingInputFingerprint,
		VectorSpace:                  space, InputGeneration: work.InputGeneration, VectorSet: vectorSet, CreatedAt: metadataEmbeddingTime(now)}
	return record, payload, nil
}

func metadataEmbeddingTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z")
}
func workerHashString(value string) string { return workerHashBytes([]byte(value)) }
func workerHashBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func waitEmbeddingWorker(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func closeEmbeddingInputs(inputs []document.EmbeddingInput) {
	seen := make(map[document.AuthorizedUpload]struct{})
	for _, input := range inputs {
		if embeddingInterfaceNil(input.Source) {
			continue
		}
		if _, exists := seen[input.Source]; exists {
			continue
		}
		seen[input.Source] = struct{}{}
		_ = input.Source.Close()
	}
}

func embeddingInterfaceNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	return reflected.Kind() == reflect.Pointer && reflected.IsNil()
}
