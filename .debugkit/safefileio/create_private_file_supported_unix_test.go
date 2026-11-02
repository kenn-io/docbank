//go:build darwin || linux

package safefileio_test

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"
)

func TestCreatePrivateFileCreatesOwnerOnlyFileDespitePermissiveUmask(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "record.json")
	previous := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(previous) })

	file, err := safefileio.CreatePrivateFile(path)
	require.NoError(err)
	defer func() { _ = file.Close() }()

	info, err := os.Lstat(path)
	require.NoError(err)
	assert.True(t, info.Mode().IsRegular())
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	stat, ok := info.Sys().(*syscall.Stat_t)
	require.True(ok)
	assert.Equal(t, uint32(os.Getuid()), stat.Uid)

	_, err = file.WriteString("{}")
	require.NoError(err)
	_, err = file.Seek(0, io.SeekStart)
	require.NoError(err)
	data, err := io.ReadAll(file)
	require.NoError(err)
	assert.Equal(t, "{}", string(data))
}

func TestCreatePrivateFileRejectsExistingFile(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "record.json")
	require.NoError(os.WriteFile(path, []byte("original"), 0o644))

	file, err := safefileio.CreatePrivateFile(path)
	require.ErrorIs(err, fs.ErrExist)
	require.Nil(file)

	data, err := os.ReadFile(path)
	require.NoError(err)
	assert.Equal(t, "original", string(data))
	info, err := os.Stat(path)
	require.NoError(err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
}

func TestCreatePrivateFileRejectsExistingSymlink(t *testing.T) {
	tests := []struct {
		name         string
		createTarget bool
	}{
		{name: "dangling", createTarget: false},
		{name: "to regular file", createTarget: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			dir := t.TempDir()
			target := filepath.Join(dir, "target")
			link := filepath.Join(dir, "link")
			if tt.createTarget {
				require.NoError(os.WriteFile(target, []byte("target"), 0o644))
			}
			require.NoError(os.Symlink(target, link))

			file, err := safefileio.CreatePrivateFile(link)
			require.ErrorIs(err, fs.ErrExist)
			require.Nil(file)

			linkInfo, err := os.Lstat(link)
			require.NoError(err)
			assert.NotZero(t, linkInfo.Mode()&os.ModeSymlink)
			if !tt.createTarget {
				_, err := os.Lstat(target)
				assert.ErrorIs(t, err, fs.ErrNotExist)
				return
			}
			data, err := os.ReadFile(target)
			require.NoError(err)
			assert.Equal(t, "target", string(data))
			info, err := os.Stat(target)
			require.NoError(err)
			assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
		})
	}
}

func TestCreatePrivateTempCreatesDistinctPrivateFiles(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		prefix  string
		suffix  string
	}{
		{name: "star replaced", pattern: "rec-*.json", prefix: "rec-", suffix: ".json"},
		{name: "last star replaced", pattern: "a*b-*.tmp", prefix: "a*b-", suffix: ".tmp"},
		{name: "no star appends", pattern: "rec-", prefix: "rec-", suffix: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			dir := t.TempDir()
			seen := map[string]bool{}
			for range 3 {
				file, err := safefileio.CreatePrivateTemp(dir, tt.pattern)
				require.NoError(err)
				require.NoError(file.Close())

				assert.Equal(t, dir, filepath.Dir(file.Name()))
				base := filepath.Base(file.Name())
				require.True(strings.HasPrefix(base, tt.prefix), base)
				require.True(strings.HasSuffix(base, tt.suffix), base)
				random := strings.TrimSuffix(strings.TrimPrefix(base, tt.prefix), tt.suffix)
				assert.NotEmpty(t, random)
				assert.NotContains(t, random, "*")
				assert.False(t, seen[base], "duplicate name %s", base)
				seen[base] = true

				info, err := os.Lstat(file.Name())
				require.NoError(err)
				assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
			}
		})
	}
}

func TestCreatePrivateTempRejectsPatternWithPathSeparator(t *testing.T) {
	dir := t.TempDir()

	file, err := safefileio.CreatePrivateTemp(dir, "sub/rec-*")
	require.Error(t, err)
	require.Nil(t, file)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}
