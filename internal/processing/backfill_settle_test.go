package processing

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/jobs"
)

func TestBackfillSettlesWithoutWaitingOutRetryBackoff(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var attempts atomic.Int64
		backfill := &Backfill[string]{
			Name: "settle-retry", Page: 10, IdleDelay: time.Second,
			List: func(_ context.Context, after string, _ int) ([]string, error) {
				if after == "" {
					return []string{"target"}, nil
				}
				return nil, nil
			},
			Key: func(target string) string { return target },
			Process: func(context.Context, string) error {
				attempts.Add(1)
				return errors.New("synthetic failure")
			},
			Logger: slog.New(slog.DiscardHandler),
		}
		supervisor := jobs.New(t.Context(), slog.New(slog.DiscardHandler))
		require.NoError(t, supervisor.StartWorker("settle-retry", backfill.Run))
		start := time.Now()
		// A fake-time bound, so a backfill that never parks fails the test.
		ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
		defer cancel()
		require.NoError(t, supervisor.Settle(ctx))
		assert.Less(t, time.Since(start), backfillFirstRetryDelay,
			"Settle must not wait out the target's retry backoff")
		assert.Equal(t, int64(1), attempts.Load(), "the failed target waits for its retry time")
		assert.Equal(t, []string{"settle-retry"}, supervisor.PendingRetries())
		require.NoError(t, supervisor.Shutdown(t.Context()))
	})
}
