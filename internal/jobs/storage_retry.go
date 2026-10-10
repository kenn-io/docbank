package jobs

import (
	"context"
	"errors"
	"time"

	"github.com/cenkalti/backoff/v7"
	"go.kenn.io/docbank/internal/store"
)

var ErrStorageOperationDeferred = errors.New("storage operation deferred for retry")

// RetryStorageOperation retries deferred work until completion or shutdown.
func RetryStorageOperation(ctx context.Context, delay time.Duration, run func() error) error {
	_, err := backoff.Retry(ctx, func() (struct{}, error) {
		err := run()
		if err != nil && !errors.Is(err, ErrStorageOperationDeferred) {
			return struct{}{}, backoff.Permanent(err)
		}
		return struct{}{}, err
	}, backoff.WithBackOff(backoff.NewConstantBackOff(delay)), backoff.WithMaxTries(0), backoff.WithMaxElapsedTime(0))
	if err != nil {
		retryErr := backoff.AsRetryError(err)
		if !errors.Is(retryErr.Cause, backoff.ErrPermanent) && ctx.Err() != nil {
			return ctx.Err()
		}
		return retryErr.LastErr
	}
	return nil
}

// DeferStorageAdmission preserves the operation for another admission attempt.
func DeferStorageAdmission(ctx context.Context, metadata *store.Store, id string, failure error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := metadata.ClaimStorageOperation(ctx, id); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.Join(ErrStorageOperationDeferred, failure, err)
	}
	return DeferStorageOperation(ctx, metadata, id, failure)
}

// DeferStorageOperation preserves claimed work for retry unless shutdown has begun.
func DeferStorageOperation(ctx context.Context, metadata *store.Store, id string, failure error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := metadata.DeferStorageOperation(context.WithoutCancel(ctx), id, failure)
	return errors.Join(ErrStorageOperationDeferred, failure, err)
}
