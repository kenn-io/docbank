//go:build windows

package blob

import (
	"io"
	"os"
	"path/filepath"
	"strings"
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

func TestOpenNoFollowWindowsLongPath(t *testing.T) {
	dir := t.TempDir()
	for len(dir) < 300 {
		dir = filepath.Join(dir, strings.Repeat("long-path-", 4))
	}
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, "source.txt")
	require.Greater(t, len(path), 260)
	require.NoError(t, os.WriteFile(path, []byte("long source"), 0o600))

	file, err := OpenNoFollow(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	data, err := io.ReadAll(file)
	require.NoError(t, err)
	require.Equal(t, "long source", string(data))
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
