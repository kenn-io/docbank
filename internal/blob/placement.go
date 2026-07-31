package blob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
)

const storageOperationRetention = 30 * 24 * time.Hour

var errStorageCleanupDeferred = errors.New("storage cleanup deferred for retry")

type PlacementObjectResult struct {
	Hash                  string `json:"hash"`
	Copied                bool   `json:"copied"`
	DestinationAuthorized bool   `json:"destination_authorized"`
	SourceRevoked         bool   `json:"source_revoked"`
	ReferenceDrift        bool   `json:"reference_drift"`
	AuditPinned           bool   `json:"audit_pinned"`
	PackRepackRequired    bool   `json:"pack_repack_required"`
	CleanupPending        bool   `json:"cleanup_pending"`
}

type PlacementReceipt struct {
	OperationID    string                  `json:"operation_id"`
	PlanDigest     string                  `json:"plan_digest"`
	Completed      int64                   `json:"completed"`
	Copied         int64                   `json:"copied"`
	CopiedBytes    int64                   `json:"copied_bytes"`
	SourceRevoked  int64                   `json:"source_revoked"`
	CleanupPending int64                   `json:"cleanup_pending"`
	Evacuated      bool                    `json:"evacuated,omitempty"`
	Objects        []PlacementObjectResult `json:"objects"`
}

// PlacementRunner executes one persisted plan without holding the daemon
// mutation gate across byte transfer. Each verified object crosses a short
// revalidating catalog transaction before source retirement.
type PlacementRunner struct {
	Metadata *store.Store
	Blobs    *Store
	// Commit serializes short catalog-authority transitions with backup
	// preservation. nil is intended only for direct embedded/test execution.
	Commit func(func() error) error
	// RetryDelay controls durable physical-cleanup retries. Zero uses the
	// daemon default; tests may shorten it without changing production policy.
	RetryDelay time.Duration
}

func (r PlacementRunner) commit(fn func() error) error {
	if r.Commit == nil {
		return fn()
	}
	return r.Commit(fn)
}

func (r PlacementRunner) Start(
	supervisor *jobs.Supervisor, operationID string,
) error {
	if supervisor == nil {
		return errors.New("placement runner requires a job supervisor")
	}
	return supervisor.Start("storage:"+operationID, func(ctx context.Context) error {
		for {
			err := r.Run(ctx, operationID)
			if !errors.Is(err, errStorageCleanupDeferred) {
				return err
			}
			delay := r.RetryDelay
			if delay <= 0 {
				delay = time.Second
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	})
}

func (r PlacementRunner) Resume(
	ctx context.Context, supervisor *jobs.Supervisor,
) error {
	operations, err := r.Metadata.ResumableStorageOperations(ctx)
	if err != nil {
		return err
	}
	for _, operation := range operations {
		if operation.Kind != "place" && operation.Kind != "evacuate" &&
			operation.Kind != "repair" && operation.Kind != "salvage" {
			continue
		}
		if err := r.Start(supervisor, operation.ID); err != nil &&
			!errors.Is(err, jobs.ErrDuplicate) {
			return fmt.Errorf("resuming storage operation %s: %w", operation.ID, err)
		}
	}
	return nil
}

func (r PlacementRunner) Run(ctx context.Context, operationID string) (resultErr error) {
	if r.Metadata == nil || r.Blobs == nil {
		return errors.New("placement runner dependencies are incomplete")
	}
	operation, err := r.Metadata.ClaimStorageOperation(ctx, operationID)
	if err != nil {
		return err
	}
	if operation.Kind == "repair" || operation.Kind == "salvage" {
		return r.runRecovery(ctx, operation)
	}
	if err := r.retirePending(ctx, operationID); err != nil {
		return r.deferCleanup(ctx, operationID, err)
	}
	var plan store.PlacementPlan
	if err := json.Unmarshal([]byte(operation.PlanJSON), &plan); err != nil {
		return r.fail(ctx, operationID, fmt.Errorf("decode placement plan: %w", err))
	}
	if err := store.ValidatePlacementPlan(plan); err != nil {
		return r.fail(ctx, operationID, err)
	}
	receipt := PlacementReceipt{
		OperationID: operationID, PlanDigest: plan.Digest,
		Completed: operation.CompletedObjects, Copied: operation.CopiedObjects,
		CopiedBytes: operation.CopiedBytes,
	}
	if operation.ReceiptJSON != "" {
		if err := json.Unmarshal([]byte(operation.ReceiptJSON), &receipt); err != nil {
			return r.fail(ctx, operationID, fmt.Errorf("decode placement progress receipt: %w", err))
		}
		if receipt.OperationID != operationID || receipt.PlanDigest != plan.Digest ||
			receipt.Completed != operation.CompletedObjects ||
			receipt.Copied != operation.CopiedObjects ||
			receipt.CopiedBytes != operation.CopiedBytes {
			return r.fail(ctx, operationID, errors.New("placement progress receipt is inconsistent"))
		}
	}
	if operation.CompletedObjects > int64(len(plan.Hashes)) {
		return r.fail(ctx, operationID, errors.New("storage operation cursor exceeds its plan"))
	}
	if err := requireScratchSpace(
		remainingPlacementScratch(plan, operation.CompletedObjects),
	); err != nil {
		return r.fail(ctx, operationID, err)
	}
	for index := operation.CompletedObjects; index < int64(len(plan.Hashes)); index++ {
		current, err := r.Metadata.StorageOperation(ctx, operationID)
		if err != nil {
			return err
		}
		if current.CancelRequested {
			return r.cancel(ctx, operationID, receipt)
		}
		if err := ctx.Err(); err != nil {
			// Daemon shutdown is not an operator cancellation. Leave the
			// durable operation resumable so the next daemon can reclaim it.
			return err
		}
		item := plan.Hashes[index]
		result, err := r.placeOne(ctx, operationID, plan.Request, item)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return r.fail(ctx, operationID, fmt.Errorf("placing blob %s: %w", item.Hash, err))
		}
		receipt.Objects = append(receipt.Objects, result)
		receipt.Completed++
		if result.Copied {
			receipt.Copied++
			receipt.CopiedBytes += item.Size
		}
		if result.SourceRevoked {
			receipt.SourceRevoked++
		}
		if result.CleanupPending {
			receipt.CleanupPending++
		}
		progress, err := json.Marshal(receipt)
		if err != nil {
			return r.fail(ctx, operationID, fmt.Errorf("encode placement progress: %w", err))
		}
		if err := r.Metadata.AdvanceStorageOperation(
			ctx, operationID, item.Hash, receipt.Completed,
			receipt.Copied, receipt.CopiedBytes, string(progress),
		); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return err
		}
		if err := r.retirePending(ctx, operationID); err != nil {
			return r.deferCleanup(ctx, operationID, err)
		}
	}
	if operation.Kind == "evacuate" {
		var finalized store.BlobStoreEvacuationFinalization
		err := r.commit(func() error {
			var finalizeErr error
			finalized, finalizeErr = r.Metadata.FinalizeBlobStoreEvacuation(
				ctx, operationID, plan.Request.SourceStoreID,
				plan.Request.DestinationStoreID,
			)
			return finalizeErr
		})
		if err != nil {
			if errors.Is(err, store.ErrStorageOperationCancelled) {
				return r.cancel(ctx, operationID, receipt)
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return r.fail(ctx, operationID, fmt.Errorf("finalizing evacuation: %w", err))
		}
		receipt.SourceRevoked += finalized.RevokedLocations
		receipt.Evacuated = finalized.Detached
		if err := r.retirePending(ctx, operationID); err != nil {
			return r.deferCleanup(ctx, operationID, err)
		}
		if !finalized.Detached {
			err = r.commit(func() error {
				var finalizeErr error
				finalized, finalizeErr = r.Metadata.FinalizeBlobStoreEvacuation(
					ctx, operationID, plan.Request.SourceStoreID,
					plan.Request.DestinationStoreID,
				)
				return finalizeErr
			})
			if err != nil {
				if errors.Is(err, store.ErrStorageOperationCancelled) {
					return r.cancel(ctx, operationID, receipt)
				}
				return r.fail(ctx, operationID, fmt.Errorf(
					"detaching evacuated store after cleanup: %w", err,
				))
			}
			receipt.Evacuated = finalized.Detached
		}
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return r.fail(ctx, operationID, fmt.Errorf("encode placement receipt: %w", err))
	}
	return r.Metadata.FinishStorageOperation(
		ctx, operationID, store.StorageOperationCompleted, string(encoded), "",
		time.Now().Add(storageOperationRetention),
	)
}

func (r PlacementRunner) deferCleanup(
	ctx context.Context, operationID string, failure error,
) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	deferErr := r.Metadata.DeferStorageOperation(
		context.WithoutCancel(ctx), operationID, failure,
	)
	return errors.Join(errStorageCleanupDeferred, failure, deferErr)
}

func remainingPlacementScratch(plan store.PlacementPlan, completed int64) int64 {
	var required int64
	for index := completed; index < int64(len(plan.Hashes)); index++ {
		required = max(required, plan.Hashes[index].ScratchBytes)
	}
	return required
}

type StorageRecoveryReceipt struct {
	OperationID string `json:"operation_id"`
	PlanDigest  string `json:"plan_digest"`
	Hash        string `json:"hash"`
	Kind        string `json:"kind"`
	Completed   bool   `json:"completed"`
}

func (r PlacementRunner) runRecovery(
	ctx context.Context, operation store.StorageOperation,
) error {
	var plan store.StorageRecoveryPlan
	if err := json.Unmarshal([]byte(operation.PlanJSON), &plan); err != nil {
		return r.fail(ctx, operation.ID, fmt.Errorf("decode storage recovery plan: %w", err))
	}
	if err := store.ValidateStorageRecoveryPlan(plan); err != nil {
		return r.fail(ctx, operation.ID, err)
	}
	if operation.CancelRequested {
		return r.cancelRecovery(ctx, operation, plan)
	}
	hash, err := packstore.ParseHash(plan.Hash)
	if err != nil {
		return r.fail(ctx, operation.ID, err)
	}
	release, err := r.Blobs.acquireLocation(
		ctx, packstore.StoreID(plan.Destination), hash,
	)
	if err != nil {
		return err
	}
	defer release()
	if err := r.Metadata.BeginStorageRecoveryPublication(
		ctx, operation.ID, plan,
	); err != nil {
		if errors.Is(err, store.ErrStorageOperationCancelled) {
			return r.cancelRecovery(ctx, operation, plan)
		}
		return r.fail(ctx, operation.ID, err)
	}
	var location packstore.ReadLocation
	switch plan.Kind {
	case "repair":
		destination, ok := r.Blobs.RepairBackend(
			packstore.StoreID(plan.Destination),
		)
		if !ok {
			return r.fail(ctx, operation.ID, fmt.Errorf(
				"%w: destination store %s cannot repair loose content",
				packstore.ErrStoreUnavailable, plan.Destination,
			))
		}
		var sourceErrors error
		for _, candidate := range plan.Sources {
			source, ok := r.Blobs.ReadBackend(candidate.StoreID)
			if !ok {
				sourceErrors = errors.Join(sourceErrors, fmt.Errorf(
					"%w: source store %s is not bound",
					packstore.ErrStoreUnavailable, candidate.StoreID,
				))
				continue
			}
			repaired, repairErr := repairOne(
				ctx, source, destination, hash, candidate, plan.Size,
			)
			if repairErr != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				sourceErrors = errors.Join(sourceErrors, repairErr)
				continue
			}
			location = packstore.ReadLocation{
				StoreID:    packstore.StoreID(plan.Destination),
				Generation: repaired.Generation, Loose: &repaired.Location,
			}
			break
		}
		if location.StoreID == "" {
			return r.fail(ctx, operation.ID, fmt.Errorf(
				"all storage recovery sources failed: %w", sourceErrors,
			))
		}
	case "salvage":
		sourceLocation := plan.Sources[0]
		source, err := r.Blobs.SalvageBackend(ctx, sourceLocation.StoreID)
		if err != nil {
			return r.fail(ctx, operation.ID, err)
		}
		defer func() { _ = closePlacementBackend(source) }()
		destination, ok := r.Blobs.WritableBackend(
			packstore.StoreID(plan.Destination),
		)
		if !ok {
			return r.fail(ctx, operation.ID, packstore.ErrStoreUnavailable)
		}
		moved, err := packstore.Move(
			ctx, readBackendOnly{ReadBackend: source}, destination,
			packstore.MoveRequest{
				Source:      sourceLocation,
				Destination: packstore.StoreID(plan.Destination),
				Identity:    packstore.BlobIdentity{Hash: hash, Size: plan.Size},
			},
		)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return r.fail(ctx, operation.ID, err)
		}
		location = moved.Destination
	}
	receipt := StorageRecoveryReceipt{
		OperationID: operation.ID, PlanDigest: plan.Digest,
		Hash: plan.Hash, Kind: plan.Kind, Completed: true,
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return r.fail(ctx, operation.ID, err)
	}
	if err := r.commit(func() error {
		return r.Metadata.CommitStorageRecovery(
			context.WithoutCancel(ctx), operation.ID, plan, location,
			string(encoded), time.Now().Add(storageOperationRetention),
		)
	}); err != nil {
		if errors.Is(err, store.ErrStorageOperationCancelled) {
			current, readErr := r.Metadata.StorageOperation(
				context.WithoutCancel(ctx), operation.ID,
			)
			if readErr != nil {
				return errors.Join(err, readErr)
			}
			return r.cancelRecovery(ctx, current, plan)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return r.fail(ctx, operation.ID, err)
	}
	return nil
}

func repairOne(
	ctx context.Context,
	source packstore.ReadBackend,
	destination packstore.RepairBackend,
	hash packstore.Hash,
	location packstore.ReadLocation,
	size int64,
) (receipt packstore.LooseReceipt, resultErr error) {
	stream, sourceSize, err := openRecoverySource(ctx, source, hash, location)
	if err != nil {
		return packstore.LooseReceipt{}, fmt.Errorf("opening repair source: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, stream.Close()) }()
	if sourceSize != size {
		return packstore.LooseReceipt{}, packstore.ErrPhysicalCorrupt
	}
	repaired, err := destination.RepairLoose(
		ctx, hash, stream, packstore.PublishOptions{
			ExpectedSize: size, SizeKnown: true,
			Durability: packstore.DurablePublication, MaxBytes: size,
		},
	)
	if err != nil {
		return packstore.LooseReceipt{}, fmt.Errorf("publishing repaired loose object: %w", err)
	}
	if err := stream.Verify(); err != nil {
		return packstore.LooseReceipt{}, fmt.Errorf("verifying repair source: %w", err)
	}
	return repaired, nil
}

func closePlacementBackend(backend packstore.Backend) error {
	if closer, ok := backend.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

func openRecoverySource(
	ctx context.Context,
	backend packstore.ReadBackend,
	hash packstore.Hash,
	location packstore.ReadLocation,
) (packstore.VerifiedReadCloser, int64, error) {
	if location.Loose != nil {
		stream, size, err := backend.OpenLoose(ctx, hash, *location.Loose)
		if err != nil {
			return nil, 0, fmt.Errorf("opening loose recovery source: %w", err)
		}
		return stream, size, nil
	}
	if location.Pack != nil {
		stream, size, err := backend.OpenPack(ctx, hash, *location.Pack)
		if err != nil {
			return nil, 0, fmt.Errorf("opening packed recovery source: %w", err)
		}
		return stream, size, nil
	}
	return nil, 0, packstore.ErrPhysicalAuthorityMissing
}

func (r PlacementRunner) placeOne(
	ctx context.Context, operationID string,
	request store.PlacementRequest, item store.PlacementHash,
) (PlacementObjectResult, error) {
	hash, err := packstore.ParseHash(item.Hash)
	if err != nil {
		return PlacementObjectResult{}, fmt.Errorf(
			"parsing placement blob hash: %w", err,
		)
	}
	release, err := r.Blobs.acquireLocation(
		ctx, packstore.StoreID(request.DestinationStoreID), hash,
	)
	if err != nil {
		return PlacementObjectResult{}, err
	}
	defer release()

	destination, err := r.currentLocation(ctx, item.Hash, request.DestinationStoreID)
	copied := false
	if errors.Is(err, store.ErrNotFound) {
		if item.Source.StoreID == "" {
			return PlacementObjectResult{}, packstore.ErrPhysicalAuthorityMissing
		}
		source, ok := r.Blobs.ReadBackend(item.Source.StoreID)
		if !ok {
			return PlacementObjectResult{}, fmt.Errorf(
				"%w: source store %s is not bound",
				packstore.ErrStoreUnavailable, item.Source.StoreID,
			)
		}
		target, ok := r.Blobs.WritableBackend(packstore.StoreID(request.DestinationStoreID))
		if !ok {
			return PlacementObjectResult{}, fmt.Errorf(
				"%w: destination store %s is not bound",
				packstore.ErrStoreUnavailable, request.DestinationStoreID,
			)
		}
		moved, moveErr := packstore.Move(ctx, source, target, packstore.MoveRequest{
			Source: item.Source, Destination: packstore.StoreID(request.DestinationStoreID),
			Identity: packstore.BlobIdentity{Hash: hash, Size: item.Size},
		})
		if moveErr != nil {
			return PlacementObjectResult{}, fmt.Errorf("moving placement blob: %w", moveErr)
		}
		if !moved.Verified {
			return PlacementObjectResult{}, errors.New("destination publication lacks verification")
		}
		destination = moved.Destination
		copied = moved.Created
	} else if err != nil {
		return PlacementObjectResult{}, err
	} else {
		if err := r.verifyPlacementDestination(
			ctx, hash, item.Size, destination,
		); err != nil {
			return PlacementObjectResult{}, err
		}
		if item.Destination == nil {
			// A resumed operation may observe the destination it verified and
			// cataloged immediately before a process stop, before progress advanced.
			copied = true
		}
	}
	var committed store.PlacementCommit
	err = r.commit(func() error {
		var commitErr error
		committed, commitErr = r.Metadata.CommitPlacement(
			ctx, operationID, request, item, destination,
		)
		return commitErr
	})
	if err != nil {
		return PlacementObjectResult{}, err
	}
	result := PlacementObjectResult{
		Hash: item.Hash, Copied: copied,
		DestinationAuthorized: committed.DestinationAuthorized,
		SourceRevoked:         committed.SourceRevoked,
		ReferenceDrift:        committed.ReferenceDrift, AuditPinned: committed.AuditPinned,
		PackRepackRequired: committed.PackRepackRequired,
	}
	return result, nil
}

func (r PlacementRunner) verifyPlacementDestination(
	ctx context.Context,
	hash packstore.Hash,
	size int64,
	location packstore.ReadLocation,
) (resultErr error) {
	if location.StoreID != packstore.StoreID(r.Metadata.PrimaryBlobStoreID()) {
		observation := r.Blobs.RefreshStore(ctx, string(location.StoreID))
		if observation.State != StoreOnline {
			sentinel := packstore.ErrStoreUnavailable
			if observation.State == StoreFenced {
				sentinel = packstore.ErrStoreFenced
			}
			return fmt.Errorf(
				"%w: destination store %s is %s: %s",
				sentinel, location.StoreID, observation.State, observation.Detail,
			)
		}
	}
	backend, ok := r.Blobs.ReadBackend(location.StoreID)
	if !ok {
		return fmt.Errorf(
			"%w: destination store %s is not bound",
			packstore.ErrStoreUnavailable, location.StoreID,
		)
	}
	stream, logicalSize, err := openRecoverySource(ctx, backend, hash, location)
	if err != nil {
		return fmt.Errorf("opening placement destination: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, stream.Close()) }()
	if logicalSize != size {
		return packstore.ErrPhysicalCorrupt
	}
	if _, err := io.Copy(io.Discard, stream); err != nil {
		return fmt.Errorf("reading placement destination: %w", err)
	}
	if err := stream.Verify(); err != nil {
		return fmt.Errorf("verifying placement destination: %w", err)
	}
	return nil
}

func (r PlacementRunner) retirePending(ctx context.Context, operationID string) error {
	cleanups, err := r.Metadata.StorageOperationCleanups(ctx, operationID)
	if err != nil {
		return err
	}
	for _, cleanup := range cleanups {
		backend, ok := r.Blobs.WritableBackend(packstore.StoreID(cleanup.StoreID))
		if !ok {
			return fmt.Errorf("%w: cleanup store %s is not bound",
				packstore.ErrStoreUnavailable, cleanup.StoreID)
		}
		if err := backend.Retire(ctx, cleanup.Ref); err != nil {
			return fmt.Errorf("retiring storage operation object: %w", err)
		}
		if err := r.Metadata.CompleteStorageOperationCleanup(
			context.WithoutCancel(ctx), operationID, cleanup,
		); err != nil {
			return err
		}
	}
	return nil
}

func (r PlacementRunner) currentLocation(
	ctx context.Context, hash, storeID string,
) (packstore.ReadLocation, error) {
	parsed, err := packstore.ParseHash(hash)
	if err != nil {
		return packstore.ReadLocation{}, fmt.Errorf(
			"parsing current location blob hash: %w", err,
		)
	}
	resolution, err := r.Metadata.ResolveBlobLocations(ctx, parsed)
	if err != nil {
		return packstore.ReadLocation{}, err
	}
	if !resolution.Member {
		return packstore.ReadLocation{}, store.ErrNotFound
	}
	for _, candidate := range resolution.Candidates {
		if candidate.StoreID == packstore.StoreID(storeID) {
			return candidate, nil
		}
	}
	return packstore.ReadLocation{}, store.ErrNotFound
}

func (r PlacementRunner) fail(ctx context.Context, id string, failure error) error {
	finishErr := r.Metadata.FinishStorageOperation(
		context.WithoutCancel(ctx), id, store.StorageOperationFailed, "",
		failure.Error(), time.Now().Add(storageOperationRetention),
	)
	return errors.Join(failure, finishErr)
}

func (r PlacementRunner) cancel(
	ctx context.Context, id string, receipt PlacementReceipt,
) error {
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return r.fail(ctx, id, err)
	}
	return r.Metadata.FinishStorageOperation(
		context.WithoutCancel(ctx), id, store.StorageOperationCancelled,
		string(encoded), "", time.Now().Add(storageOperationRetention),
	)
}

func (r PlacementRunner) cancelRecovery(
	ctx context.Context,
	operation store.StorageOperation,
	plan store.StorageRecoveryPlan,
) error {
	receipt := StorageRecoveryReceipt{
		OperationID: operation.ID,
		PlanDigest:  plan.Digest,
		Hash:        plan.Hash,
		Kind:        plan.Kind,
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return r.fail(ctx, operation.ID, err)
	}
	return r.Metadata.FinishStorageOperation(
		context.WithoutCancel(ctx), operation.ID, store.StorageOperationCancelled,
		string(encoded), "", time.Now().Add(storageOperationRetention),
	)
}
