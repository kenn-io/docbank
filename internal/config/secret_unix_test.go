//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestSecretRejectsFIFOWithoutOpeningAStream(t *testing.T) {
	clearServerEnvironment(t)
	path := filepath.Join(t.TempDir(), "fifo")
	require.NoError(t, unix.Mkfifo(path, 0o600))
	t.Setenv("DOCBANK_API_KEY_FILE", path)
	_, err := Load(t.TempDir())
	require.Error(t, err)
}

func TestSecretFileOwnershipModesAndSymlinks(t *testing.T) {
	for _, mode := range []os.FileMode{0o400, 0o600, 0o644, 0o640, 0o440, 0o700} {
		t.Run(mode.String(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "key")
			require.NoError(t, os.WriteFile(path, []byte("synthetic-key"), 0o600))
			require.NoError(t, os.Chmod(path, mode))
			file, err := openSecret(path)
			if mode == 0o400 || mode == 0o600 {
				require.NoError(t, err)
				require.NoError(t, file.Close())
			} else {
				require.Error(t, err)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(path, []byte("synthetic-key"), 0o600))
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(path, link))
	_, err := openSecret(link)
	require.Error(t, err)
}
