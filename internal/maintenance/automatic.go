package maintenance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// RunPackSchedule performs one bounded pack immediately and then once per
// interval until shutdown. A failed run remains visible as a failed daemon job
// instead of being silently retried forever.
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

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		report, err := run(ctx)
		if err != nil {
			return fmt.Errorf("automatic packing: %w", err)
		}
		stats := report.Stats
		if stats.BlobsPacked > 0 || stats.PacksSealed > 0 || report.More {
			logger.Info("automatic packing completed",
				"blobs", stats.BlobsPacked,
				"raw_bytes", stats.BytesPacked,
				"packs", stats.PacksSealed,
				"more", report.More,
			)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
