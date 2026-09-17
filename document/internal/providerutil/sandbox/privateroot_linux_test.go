//go:build linux

package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestRuntimePreparationRejectsSpecialFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "socket")
	data := []byte("discovered")
	sum := sha256.Sum256(data)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	require.NoError(t, os.Remove(path))
	require.NoError(t, unix.Mkfifo(path, 0o600))
	entry := RuntimeFile{SourcePath: path, GuestPath: "/usr/bin/socket", SHA256: hex.EncodeToString(sum[:])}
	_, err := copyRuntimeFile(entry, filepath.Join(t.TempDir(), "snapshot"), MaxRuntimeBytes)
	require.ErrorIs(t, err, ErrRuntimeSpecialFile)
}

func TestRuntimeIdentityMismatchFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime")
	require.NoError(t, os.WriteFile(path, []byte("original"), 0o600))
	entry := RuntimeFile{SourcePath: path, GuestPath: "/usr/bin/runtime", SHA256: strings.Repeat("a", 64)}
	_, err := copyRuntimeFile(entry, filepath.Join(t.TempDir(), "snapshot"), MaxRuntimeBytes)
	require.ErrorIs(t, err, ErrRuntimeIdentityMismatch)
}

func TestRuntimeSourceMutationAfterCopyCannotChangeBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime")
	original := []byte("original runtime bytes")
	require.NoError(t, os.WriteFile(path, original, 0o600))
	sum := sha256.Sum256(original)
	entry := RuntimeFile{SourcePath: path, GuestPath: "/usr/bin/runtime", SHA256: hex.EncodeToString(sum[:])}
	target := filepath.Join(t.TempDir(), "snapshot")
	_, err := copyRuntimeFile(entry, target, MaxRuntimeBytes)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte("replacement"), 0o600))
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, original, got)
}

func TestPrivateRootControlCapacity(t *testing.T) {
	executable, err := filepath.Abs(filepath.Join(t.TempDir(), "runtime"))
	require.NoError(t, err)
	content := []byte("discovered runtime bytes")
	require.NoError(t, os.WriteFile(executable, content, 0o600))
	digest := sha256.Sum256(content)
	files := make([]RuntimeFile, 0, 1000)
	for index := range 1000 {
		files = append(files, RuntimeFile{
			SourcePath: executable, GuestPath: "/usr/runtime/file-" + strconv.Itoa(index),
			SHA256: hex.EncodeToString(digest[:]),
		})
	}
	root := &PrivateRoot{
		Runtime: files, RuntimeIdentity: "sha256:" + strings.Repeat("b", 64),
		WorkBytes: 1, InputName: "input", OutputName: "output", MaxOutputBytes: 1,
		WarmupInput: []byte("warmup"), WarmupInputSHA256: "c6cf1309cd700e5a84e18d0b1d5877b9a608141037ac40445d484398256fc56c",
	}
	policy := Policy{
		Mode: LibreOfficeMode, Executable: executable, ExecutableSHA256: strings.Repeat("a", 64),
		Arguments: []string{"x"}, Environment: []string{"LANG=C"},
		MaxStdinBytes: 1, MaxStdoutBytes: 1, PrivateRoot: root,
	}
	encoded, err := encodeControl(launchControl{Policy: policy, StdinSHA256: strings.Repeat("a", 64)})
	require.NoError(t, err)
	assert.Less(t, len(encoded), int(MaxPrivateRootControlBytes))
}

func TestRuntimeLandlockPathsUseGuestParents(t *testing.T) {
	root := &PrivateRoot{
		Runtime: []RuntimeFile{
			{SourcePath: "/host/cache/libfoo.so", GuestPath: "/usr/lib/libfoo.so"},
			{SourcePath: "/host/cache/font.ttf", GuestPath: "/usr/share/fonts/font.ttf"},
		},
		Symlinks: []RuntimeSymlink{{GuestPath: "/lib/libfoo.so", Target: "../usr/lib/libfoo.so"}},
	}

	paths := runtimeLandlockPaths("/usr/lib/libreoffice/program/soffice.bin", root)
	assert.Equal(t, []string{"/lib", "/usr/lib", "/usr/lib/libreoffice/program", "/usr/share/fonts"}, paths)
	assert.NotContains(t, paths, "/host/cache")
}

func TestAddLandlockPathsHandlesMissingEntriesByMode(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	require.NoError(t, addLandlockPaths(^uintptr(0), []string{missing}, unix.LANDLOCK_ACCESS_FS_READ_FILE, true))

	err := addLandlockPaths(^uintptr(0), []string{missing}, unix.LANDLOCK_ACCESS_FS_READ_FILE, false)
	require.ErrorIs(t, err, unix.ENOENT)

	existing := filepath.Join(t.TempDir(), "existing")
	require.NoError(t, os.WriteFile(existing, []byte("path"), 0o600))
	err = addLandlockPaths(^uintptr(0), []string{existing}, unix.LANDLOCK_ACCESS_FS_READ_FILE, true)
	require.ErrorContains(t, err, "add Landlock path")
}
