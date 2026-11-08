package brainclient

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/push"
)

func TestBrainClientExplicitIndexingDoesNotGrantWithoutReviewedPlan(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	registry, err := OpenRegistry(filepath.Join(root, "registry"))
	require.NoError(t, err)
	acquired := false
	c := New(registry, Dependencies{Ensure: func(context.Context, string) (*daemonconn.Connection, error) {
		acquired = true
		return nil, errors.New("unexpected acquisition")
	}})
	cfg := ConnectionConfig{Name: "notes", SourceRoot: t.TempDir(), Destination: "/notes", LocalVaultRoot: t.TempDir(),
		SettleTime: 2 * time.Second, ScanInterval: 5 * time.Second, IndexLocal: true}
	require.ErrorContains(t, c.Connect(t.Context(), cfg), "not available yet")
	require.False(t, acquired)
	saved, err := registry.List(t.Context())
	require.NoError(t, err)
	require.Empty(t, saved)
}

func TestBrainClientRunPinsTargetAndHoldsLock(t *testing.T) {
	t.Parallel()
	registry, err := OpenRegistry(filepath.Join(t.TempDir(), "registry"))
	require.NoError(t, err)
	vault := t.TempDir()
	cfg := ConnectionConfig{Name: "notes", SourceRoot: t.TempDir(), Destination: "/notes", LocalVaultRoot: vault,
		SettleTime: 2 * time.Second, ScanInterval: 5 * time.Second}
	require.NoError(t, registry.Put(t.Context(), cfg))
	var roots []string
	c := New(registry, Dependencies{
		Ensure: func(_ context.Context, root string) (*daemonconn.Connection, error) {
			roots = append(roots, root)
			return daemonconn.NewPushConnection("http://127.0.0.1:1", "synthetic-key")
		},
		Publish: func(ctx context.Context, _ *daemonconn.Connection, opts push.Options) (push.Report, error) {
			require.True(t, opts.Publication)
			require.True(t, opts.Watch)
			require.Equal(t, cfg.SourceRoot, opts.Folder.Source)
			_, err := registry.AcquireRun(ctx, cfg.Name)
			require.ErrorIs(t, err, ErrBusy)
			require.NotNil(t, opts.Reconnect)
			_, err = opts.Reconnect(ctx)
			require.NoError(t, err)
			return push.Report{Added: 1}, nil
		},
	})
	report, err := c.Run(t.Context(), cfg.Name, nil)
	require.NoError(t, err)
	require.Equal(t, 1, report.Added)
	require.Equal(t, []string{vault, vault}, roots)
	release, err := registry.AcquireRun(t.Context(), cfg.Name)
	require.NoError(t, err)
	require.NoError(t, release())
}
