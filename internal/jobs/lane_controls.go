package jobs

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.kenn.io/docbank/internal/store"
)

const LanePollInterval = 100 * time.Millisecond

const unreadableControlsReason = "waiting for readable lane controls: "

// AdmitStorageOperation waits outside mutation gates and preserves the current pass.
// Unreadable lane controls hold the lane paused, and the waiting operation's
// error says why until the controls can be read again.
func AdmitStorageOperation(
	ctx context.Context, metadata *store.Store, id string,
) (cancelled bool, err error) {
	deferred := false
	noted := ""
	for {
		operation, err := metadata.StorageOperation(ctx, id)
		if err != nil {
			return false, err
		}
		// An earlier daemon run may have left this note; adopt it so it clears
		// once the controls read again, even while the lane stays paused.
		if noted == "" && strings.HasPrefix(operation.Error, unreadableControlsReason) {
			noted = operation.Error
		}
		paused, reason := false, ""
		if !operation.CancelRequested {
			control, err := metadata.LaneControl(ctx, operation.Kind)
			if errors.Is(err, store.ErrLaneControl) {
				return false, err
			}
			paused = err != nil || control.Paused
			if err != nil {
				reason = unreadableControlsReason + err.Error()
			}
		}
		if operation.CancelRequested || !paused {
			if deferred {
				if _, err := metadata.ClaimStorageOperation(ctx, id); err != nil {
					return false, err
				}
			}
			return operation.CancelRequested, nil
		}
		if operation.State == store.StorageOperationRunning && !deferred {
			var failure error
			if reason != "" {
				failure = errors.New(reason)
			}
			if err := metadata.DeferStorageOperation(ctx, id, failure); err != nil {
				return false, err
			}
			deferred, noted = true, reason
		} else if reason != noted {
			if err := metadata.NoteQueuedStorageOperation(ctx, id, reason); err != nil {
				return false, err
			}
			noted = reason
		}
		if err := Wait(ctx, LanePollInterval); err != nil {
			return false, err
		}
	}
}

func Wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
