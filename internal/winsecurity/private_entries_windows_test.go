//go:build windows

package winsecurity

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"
)

func TestPrivateEntriesPinnedCreationAndDirectoryValidation(t *testing.T) {
	parent, err := os.Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, parent.Close()) })
	sentinel := filepath.Join(parent.Name(), "sentinel")
	require.NoError(t, os.WriteFile(sentinel, []byte("unrelated synthetic bytes"), 0o600))
	before, err := os.Stat(sentinel)
	require.NoError(t, err)
	t.Cleanup(func() {
		data, err := os.ReadFile(sentinel)
		require.NoError(t, err)
		require.Equal(t, []byte("unrelated synthetic bytes"), data)
		after, err := os.Stat(sentinel)
		require.NoError(t, err)
		require.Equal(t, before.Mode(), after.Mode())
	})
	d, err := MkdirPrivatePinnedFileAt(parent, "private")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	require.NoError(t, ValidatePrivateDirectoryFile(d))
	require.Error(t, os.Rename(filepath.Join(parent.Name(), "private"), filepath.Join(parent.Name(), "moved")))
	f, err := CreatePrivateMovableFileAt(d, "body")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, f.Close()) })
	require.NoError(t, safefileio.ValidatePrivateCurrentUserFile(f))
	require.Error(t, ValidatePrivateDirectoryFile(f))
	_, err = CreatePrivatePinnedFileAt(d, "body")
	require.ErrorIs(t, err, os.ErrExist)
	for _, component := range []string{"..", `x\y`, "x:y", "."} {
		_, err = MkdirPrivateMovableFileAt(d, component)
		require.Error(t, err)
	}
}
