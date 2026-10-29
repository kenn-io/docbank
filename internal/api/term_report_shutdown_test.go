package api

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/reporting"
	"go.kenn.io/docbank/report"
)

func TestShutdownClosesReportsAfterContextExpires(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		budget := report.NewBudget(1 << 20)
		t.Cleanup(func() { require.NoError(t, budget.Close()) })
		cache := reporting.NewCache(nil, budget)
		sessions := newWebSessionRegistry()
		// Keep an upload draining so shutdown must take the canceled-context path.
		sessions.uploadGroup.Add(1)
		defer sessions.uploadGroup.Done()
		server := &Server{webSessions: sessions, termReports: cache, reportBudget: budget}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		require.ErrorIs(t, server.Shutdown(ctx), context.Canceled)
		_, err := cache.Create(t.Context(), "owner", &reporting.Service{}, report.Request{})
		require.ErrorIs(t, err, reporting.ErrUnavailable)
		_, err = budget.Reserve(t.Context(), 1)
		require.ErrorIs(t, err, report.ErrBudgetClosed)
	})
}
