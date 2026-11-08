package brainservice

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// An unavailable manager cannot prove that an existing registration is ours.
// Both explicit operations must stop before writing or issuing mutations.
func TestBrainQualificationManagerFailurePreservesExistingFiles(t *testing.T) {
	root := t.TempDir()
	spec := renderSpec("linux")
	spec.ConfigRoot = filepath.Join(root, "registry")
	spec.UserHome = filepath.Join(root, "home")
	spec.UserConfigRoot = filepath.Join(root, "config")
	reg, err := Render("linux", spec)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(reg.Path), 0700))
	foreign := []byte("synthetic foreign service definition\n")
	require.NoError(t, os.WriteFile(reg.Path, foreign, 0600))
	var calls [][]string
	driver := newServiceDriver("linux", func(_ context.Context, executable string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{executable}, args...))
		return nil, fs.ErrPermission
	}, nil)
	status, err := driver.Install(t.Context(), spec)
	require.ErrorIs(t, err, ErrManager)
	require.Equal(t, "unknown", status.State)
	require.ErrorIs(t, driver.Uninstall(t.Context(), spec), ErrManager)
	for _, call := range calls {
		require.Equal(t, []string{"systemctl", "--user", "show", reg.ID, "--property=LoadState,ActiveState,FragmentPath", "--no-pager"}, call)
	}
	got, err := os.ReadFile(reg.Path)
	require.NoError(t, err)
	require.Equal(t, foreign, got)
	_, err = os.Stat(receiptPath(spec, reg))
	require.ErrorIs(t, err, fs.ErrNotExist)
}
