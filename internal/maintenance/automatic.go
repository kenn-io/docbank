package maintenance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.kenn.io/kit/pack"

	"go.kenn.io/docbank/internal/jobs"
)

// RunPackSchedule performs one bounded pack immediately and then once per
// interval until shutdown. Each run is canceled at the interval and only a
// cancellation caused by that deadline is retried.
func RunPackSchedule(
	ctx context.Context,
	interval time.Duration,
	run func(context.Context) (PackReport, error),
	logger *slog.Logger,
) error {
	if interval <= 0 {
		return errors.New("automatic pack interval must be positive")
	}
	if run == nil {
		return errors.New("automatic pack runner is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	for {
		// found records whether this run packed anything or left more to pack.
		var found bool
		runCtx, cancelRun := context.WithTimeout(ctx, interval)
		jobs.ReportBusy(ctx, true)
		report, err := run(runCtx)
		jobs.ReportBusy(ctx, false)
		deadlineExceeded := errors.Is(runCtx.Err(), context.DeadlineExceeded)
		cancelRun()
		if err != nil && ctx.Err() == nil && deadlineExceeded && isCancellationOnly(err) {
			found = true
			logger.Warn("automatic packing canceled at interval; retrying",
				"interval", interval,
				"blobs", report.Stats.BlobsPacked,
				"raw_bytes", report.Stats.BytesPacked,
				"packs", report.Stats.PacksSealed,
				"error", err,
			)
		} else if err != nil {
			return fmt.Errorf("automatic packing: %w", err)
		} else {
			stats := report.Stats
			found = stats.BlobsPacked > 0 || stats.PacksSealed > 0 || report.More
			if found {
				logger.Info("automatic packing completed",
					"blobs", stats.BlobsPacked,
					"raw_bytes", stats.BytesPacked,
					"packs", stats.PacksSealed,
					"more", report.More,
				)
			}
		}
		if err := jobs.Idle(ctx, interval, found); err != nil {
			return err
		}
	}
}

func isCancellationOnly(err error) bool {
	cancelled, independent := classifyCancellation(err)
	return cancelled && !independent
}

func classifyCancellation(err error) (cancelled, independent bool) {
	if err == nil {
		return false, false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		if len(joined.Unwrap()) == 0 {
			return false, true
		}
		for _, cause := range joined.Unwrap() {
			causeCancelled, causeIndependent := classifyCancellation(cause)
			cancelled = cancelled || causeCancelled
			independent = independent || causeIndependent
		}
		return cancelled, independent
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return classifyCancellation(wrapped.Unwrap())
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true, false
	}
	// These accompany canceled transactions or streams; neither alone
	// establishes cancellation or excuses an independent failure.
	if errors.Is(err, sql.ErrTxDone) || errors.Is(err, pack.ErrVerificationIncomplete) {
		return false, false
	}
	return false, true
}
