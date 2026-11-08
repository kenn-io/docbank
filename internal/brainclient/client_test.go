package brainclient

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestBrainClientExplicitIndexingDoesNotGrantWithoutReviewedPlan(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	registry, err := OpenRegistry(root)
	require.NoError(t, err)
	acquired := false
	c := New(registry, Dependencies{Ensure: func(context.Context, string) (*daemonconn.Connection, error) {
		acquired = true
		return nil, nil
	}})
	cfg := ConnectionConfig{Name: "notes", SourceRoot: t.TempDir(), Destination: "/notes", LocalVaultRoot: t.TempDir(),
		SettleTime: 2 * time.Second, ScanInterval: 5 * time.Second, IndexLocal: true}
	require.ErrorContains(t, c.Connect(t.Context(), cfg), "not available yet")
	require.False(t, acquired)
	saved, err := registry.List(t.Context())
	require.NoError(t, err)
	require.Empty(t, saved)
}
