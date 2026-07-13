//go:build windows

package blob

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenNoFollowWindowsRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.txt")
	require.NoError(t, os.WriteFile(path, []byte("source"), 0o600))
	file, err := OpenNoFollow(path)
	require.NoError(t, err)
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(file)
	require.NoError(t, err)
	require.Equal(t, "source", string(data))
}

func TestOpenNoFollowWindowsRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	link := filepath.Join(dir, "link.txt")
	require.NoError(t, os.WriteFile(target, []byte("source"), 0o600))
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("creating a Windows symlink requires developer mode: %v", err)
	}
	file, err := OpenNoFollow(link)
	if file != nil {
		_ = file.Close()
	}
	require.Error(t, err)
}
