//go:build linux || darwin

package filepublish

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestRootPublicationFallsBackToNoReplaceLink(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		renameErr error
		existing  bool
		linked    bool
	}{
		{name: "rename flags unsupported", renameErr: unix.EINVAL, linked: true},
		{name: "no-replace unsupported", renameErr: unix.ENOTSUP, linked: true},
		{name: "fallback keeps existing name", renameErr: unix.EINVAL, existing: true},
		{name: "other rename failure", renameErr: unix.EXDEV},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "stage"), []byte("copy"), 0o600))
			if tc.existing {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "photo.jpg"), []byte("edited"), 0o600))
			}
			handle, err := os.Open(dir)
			require.NoError(t, err)
			defer func() { require.NoError(t, handle.Close()) }()
			err = publishRootNoReplace(handle, "stage", handle, "photo.jpg", func(_ int, old string, _ int, target string) error {
				return &os.LinkError{Op: "renameat2", Old: old, New: target, Err: tc.renameErr}
			})
			content, readErr := os.ReadFile(filepath.Join(dir, "photo.jpg"))
			switch {
			case tc.linked:
				require.NoError(t, err)
				require.NoError(t, readErr)
				require.Equal(t, "copy", string(content))
			case tc.existing:
				require.ErrorIs(t, err, os.ErrExist)
				require.Equal(t, "edited", string(content))
			default:
				require.ErrorIs(t, err, tc.renameErr)
				require.ErrorIs(t, readErr, os.ErrNotExist)
			}
		})
	}
}
