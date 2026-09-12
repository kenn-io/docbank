package emailpdf

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAvailabilityReportsUnreachableUserManager(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+filepath.Join(runtimeDir, "missing-bus"))
	r := &Runtime{config: RuntimeConfig{Spool: t.TempDir()}}
	err := r.CheckAvailable(t.Context())
	require.ErrorIs(t, err, ErrUnavailable)
	if _, statErr := os.Stat("/sys/fs/cgroup/cgroup.controllers"); statErr == nil {
		require.ErrorContains(t, err, "systemd user manager")
	}
	entries, err := os.ReadDir(r.config.Spool)
	require.NoError(t, err)
	require.Empty(t, entries, "unreachable manager must not create a probe service or staging")
}

func TestAvailabilityHonorsCancellationBeforeProbe(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, (&Runtime{}).CheckAvailable(ctx), context.Canceled)
}
