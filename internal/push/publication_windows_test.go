//go:build windows

package push

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestPublicationSharingLockNeverRetires(t *testing.T) {
	c, s := newPushTestDaemon(t, nil)
	root := t.TempDir()
	writePublicationFile(t, root, "retained.md")
	writePublicationFile(t, root, "current.md")
	opts := publicationOptions(root)
	_, err := Run(t.Context(), c, opts)
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(root, "retained.md")))
	path, err := windows.UTF16PtrFromString(filepath.Join(root, "current.md"))
	require.NoError(t, err)
	handle, err := windows.CreateFile(path, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	require.NoError(t, err)
	_, runErr := Run(t.Context(), c, opts)
	require.NoError(t, windows.CloseHandle(handle))
	require.Error(t, runErr)
	status, err := s.NotePublicationStatus(t.Context(), "research")
	require.NoError(t, err)
	require.EqualValues(t, 2, status.Present)
	require.Zero(t, status.Absent)
	moved := root + "-moved"
	require.NoError(t, os.Rename(root, moved))
	require.NoError(t, os.Rename(moved, root))
}
