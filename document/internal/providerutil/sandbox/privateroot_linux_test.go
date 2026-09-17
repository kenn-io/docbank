//go:build linux

package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
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
	root := &PrivateRoot{
		Runtime:         []RuntimeFile{{SourcePath: path, GuestPath: "/usr/bin/socket", SHA256: hex.EncodeToString(sum[:])}},
		RuntimeIdentity: "sha256:" + strings.Repeat("b", 64),
		WorkBytes:       1, InputName: "input", OutputName: "output", MaxOutputBytes: 1,
		WarmupInput: []byte("warmup"), WarmupInputSHA256: "c6cf1309cd700e5a84e18d0b1d5877b9a608141037ac40445d484398256fc56c",
	}
	_, err := prepareRuntimeFiles(context.Background(), root)
	require.ErrorIs(t, err, ErrRuntimeSpecialFile)
}

func TestRuntimeIdentityMismatchFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime")
	require.NoError(t, os.WriteFile(path, []byte("original"), 0o600))
	root := &PrivateRoot{
		Runtime:         []RuntimeFile{{SourcePath: path, GuestPath: "/usr/bin/runtime", SHA256: strings.Repeat("a", 64)}},
		RuntimeIdentity: "sha256:" + strings.Repeat("b", 64),
		WorkBytes:       1, InputName: "input", OutputName: "output", MaxOutputBytes: 1,
		WarmupInput: []byte("warmup"), WarmupInputSHA256: "c6cf1309cd700e5a84e18d0b1d5877b9a608141037ac40445d484398256fc56c",
	}
	_, err := prepareRuntimeFiles(context.Background(), root)
	require.ErrorIs(t, err, ErrRuntimeIdentityMismatch)
}

func TestRuntimeSourceMutationAfterSealingCannotChangeBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime")
	original := []byte("original runtime bytes")
	require.NoError(t, os.WriteFile(path, original, 0o600))
	sum := sha256.Sum256(original)
	entry := RuntimeFile{SourcePath: path, GuestPath: "/usr/bin/runtime", SHA256: hex.EncodeToString(sum[:])}
	memfd, _, err := sealRuntimeFile(context.Background(), entry, MaxRuntimeBytes)
	require.NoError(t, err)
	defer func() { _ = memfd.Close() }()
	require.NoError(t, os.WriteFile(path, []byte("replacement"), 0o600))
	got, err := os.ReadFile("/proc/self/fd/" + strconv.Itoa(int(memfd.Fd())))
	require.NoError(t, err)
	assert.Equal(t, original, got)
}

func TestRuntimePreflightRequiresHeadroom(t *testing.T) {
	if os.Getenv("DOCBANK_PREFLIGHT_HELPER") == "1" {
		runtimeEntries := 1
		required := requiredRuntimeFDs(runtimeEntries)
		limit := unix.Rlimit{Cur: required - 1, Max: required + 64}
		require.NoError(t, unix.Setrlimit(unix.RLIMIT_NOFILE, &limit))
		require.NoError(t, preflightRuntimeFDLimit(runtimeEntries))
		var raised unix.Rlimit
		require.NoError(t, unix.Getrlimit(unix.RLIMIT_NOFILE, &raised))
		require.GreaterOrEqual(t, raised.Cur, required)

		limit = unix.Rlimit{Cur: required - 1, Max: required - 1}
		require.NoError(t, unix.Setrlimit(unix.RLIMIT_NOFILE, &limit))
		require.ErrorIs(t, preflightRuntimeFDLimit(runtimeEntries), ErrPrivateRootUnavailable)
		return
	}
	command := exec.Command(os.Args[0], "-test.run", "^TestRuntimePreflightRequiresHeadroom$", "-test.v") //nolint:gosec // fixed selector runs this test binary only
	command.Env = append(os.Environ(), "DOCBANK_PREFLIGHT_HELPER=1")
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func TestPrivateRootControlCapacity(t *testing.T) {
	executable, err := filepath.Abs("runtime")
	require.NoError(t, err)
	content := []byte("discovered runtime bytes")
	require.NoError(t, os.WriteFile(executable, content, 0o600))
	t.Cleanup(func() { _ = os.Remove(executable) })
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
		Mode: SupervisedFileMode, Executable: executable, ExecutableSHA256: strings.Repeat("a", 64),
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
