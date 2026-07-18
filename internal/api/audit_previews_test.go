package api

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/store"
)

func TestAuditPreviewRegistryExpiresPlans(t *testing.T) {
	registry := newAuditPreviewRegistry()
	now := time.Date(2026, time.July, 18, 12, 0, 0, 0, time.UTC)
	registry.now = func() time.Time { return now }
	registry.ttl = time.Minute

	plan := new(store.AuditEnrollmentPlan)
	token, expiresAt, err := registry.issue(plan)
	require.NoError(t, err)
	require.Equal(t, now.Add(time.Minute), expiresAt)

	now = expiresAt
	_, err = registry.take(token)
	require.ErrorIs(t, err, errAuditPreviewUnavailable)
	require.Empty(t, registry.entries)
}
