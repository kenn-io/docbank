//go:build !windows

package push

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPublicationUnreadableSubtreeNeverRetires(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses fixture directory permissions")
	}
	c, s := newPushTestDaemon(t, nil)
	root := t.TempDir()
	writePublicationFile(t, root, "retained.md")
	writePublicationFile(t, root, "blocked/current.md")
	opts := publicationOptions(root)
	_, err := Run(t.Context(), c, opts)
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(root, "retained.md")))
	blocked := filepath.Join(root, "blocked")
	require.NoError(t, os.Chmod(blocked, 0))
	defer func() { require.NoError(t, os.Chmod(blocked, 0700)) }()
	_, err = Run(t.Context(), c, opts)
	require.Error(t, err)
	status, err := s.NotePublicationStatus(t.Context(), "research")
	require.NoError(t, err)
	require.EqualValues(t, 2, status.Present)
	require.Zero(t, status.Absent)
}
func TestPublicationSymlinkDoesNotRetireExistingNotes(t *testing.T) {
	c, s := newPushTestDaemon(t, nil)
	root := t.TempDir()
	outside := t.TempDir()
	writePublicationFile(t, root, "retained.md")
	writePublicationFile(t, root, "current.md")
	writePublicationFile(t, outside, "other.md")
	opts := publicationOptions(root)
	_, err := Run(t.Context(), c, opts)
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(root, "retained.md")))
	require.NoError(t, os.Remove(filepath.Join(root, "current.md")))
	require.NoError(t, os.Symlink(filepath.Join(outside, "other.md"), filepath.Join(root, "current.md")))
	_, err = Run(t.Context(), c, opts)
	require.Error(t, err)
	status, err := s.NotePublicationStatus(t.Context(), "research")
	require.NoError(t, err)
	require.EqualValues(t, 2, status.Present)
	require.Zero(t, status.Absent)
}
