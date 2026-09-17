//go:build linux

package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	root := &PrivateRoot{
		Runtime:         []RuntimeFile{{SourcePath: path, GuestPath: "/usr/bin/socket", SHA256: hex.EncodeToString(sum[:])}},
		RuntimeIdentity: "sha256:" + strings.Repeat("b", 64),
		WorkBytes:       1, InputName: "input", OutputName: "output", MaxOutputBytes: 1,
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
	err := preflightRuntimeFDLimit(MaxRuntimeEntries)
	if errors.Is(err, ErrPrivateRootUnavailable) {
		return
	}
	require.NoError(t, err)
}

func TestPrivateRootControlCapacity(t *testing.T) {
	executable, err := filepath.Abs("runtime")
	require.NoError(t, err)
	files := make([]RuntimeFile, 0, 1000)
	for index := range 1000 {
		files = append(files, RuntimeFile{
			SourcePath: executable, GuestPath: "/usr/runtime/" + filepath.Base(executable) + string(rune('a'+index%26)),
			SHA256: strings.Repeat("a", 64),
		})
	}
	root := &PrivateRoot{
		Runtime: files, RuntimeIdentity: "sha256:" + strings.Repeat("b", 64),
		WorkBytes: 1, InputName: "input", OutputName: "output", MaxOutputBytes: 1,
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
