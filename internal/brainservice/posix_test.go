//go:build !windows

package brainservice

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBrainServiceLinuxFakeLifecycle(t *testing.T) {
	d, s, m := driverFixtureFor(t, "linux")
	status, err := d.Install(t.Context(), s)
	require.NoError(t, err)
	require.True(t, status.Installed && status.Owned && status.Running)
	calls := len(m.calls)
	_, err = d.Install(t.Context(), s)
	require.NoError(t, err)
	for _, call := range m.calls[calls:] {
		require.Contains(t, []string{"show", "cat"}, managerOperation(call))
	}
	require.NoError(t, d.Uninstall(t.Context(), s))
	status, err = d.Status(t.Context(), s)
	require.NoError(t, err)
	require.Equal(t, "absent", status.State)
}
