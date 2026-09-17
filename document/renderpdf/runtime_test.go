package renderpdf

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/internal/providerutil/sandbox"
)

func TestRuntimeDiscoveryRejectsSpecialFiles(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the owner proof uses a Linux Unix socket")
	}
	root := t.TempDir()
	socketPath := filepath.Join(root, "socket")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	require.NoError(t, os.WriteFile(filepath.Join(root, "safe"), []byte("safe"), 0o644))
	_, err = DiscoverRuntime([]string{root})
	require.ErrorIs(t, err, sandbox.ErrRuntimeSpecialFile)
}

func TestRuntimeExecutableMappingIncludesMode0644ELF(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "libmergedlo.so")
	require.NoError(t, os.WriteFile(path, []byte{0x7f, 'E', 'L', 'F', 1, 2, 3}, 0o644))
	manifest, err := DiscoverRuntime([]string{root})
	require.NoError(t, err)
	require.Len(t, manifest.Files, 1)
	assert.True(t, manifest.Files[0].Executable)
}

func TestDiscoveredRuntimeManifestControlCapacity(t *testing.T) {
	root := t.TempDir()
	for index := range 1_000 {
		path := filepath.Join(root, "runtime-"+strings.Repeat("0", 4-len(strconv.Itoa(index)))+strconv.Itoa(index))
		require.NoError(t, os.WriteFile(path, []byte("runtime"), 0o644))
	}
	manifest, err := DiscoverRuntime([]string{root})
	require.NoError(t, err)
	digestBytes := sha256.Sum256([]byte("renderer"))
	policy := sandbox.Policy{
		Mode: sandbox.SupervisedFileMode, Executable: "/renderer",
		ExecutableSHA256: hex.EncodeToString(digestBytes[:]), Arguments: []string{"renderer"},
		Environment: []string{"LANG=C"}, MaxStdinBytes: 1, MaxStdoutBytes: 1,
		PrivateRoot: &sandbox.PrivateRoot{
			Runtime: manifest.Files, Symlinks: manifest.Symlinks,
			RuntimeIdentity: manifest.Identity, WorkBytes: 1,
			InputName: "input", OutputName: "output", MaxOutputBytes: 1,
		},
	}
	encoded, err := json.Marshal(struct {
		Policy      sandbox.Policy `json:"policy"`
		StdinSHA256 string         `json:"stdin_sha256"`
	}{Policy: policy, StdinSHA256: strings.Repeat("a", 64)})
	require.NoError(t, err)
	assert.Less(t, len(encoded), int(sandbox.MaxPrivateRootControlBytes))
}

func TestRuntimeIdentityChangesAfterSourceMutation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "runtime")
	require.NoError(t, os.WriteFile(path, []byte("before"), 0o644))
	manifest, err := DiscoverRuntime([]string{root})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte("after"), 0o644))
	identity, err := runtimeIdentityForManifest(manifest.Files, manifest.Symlinks)
	require.NoError(t, err)
	assert.NotEqual(t, manifest.Identity, identity)
}

func TestRuntimeDepthAndEntryCeilingsAreNamed(t *testing.T) {
	assert.Equal(t, 64, sandbox.MaxRuntimeDepth)
	assert.Equal(t, 250_000, sandbox.MaxRuntimeEntries)
	assert.Equal(t, sandbox.MaxRuntimeBytes, int64(4<<30))
}

func TestRuntimeSymlinkValidationRejectsParentTraversal(t *testing.T) {
	root := sandbox.PrivateRoot{
		RuntimeIdentity: "sha256:" + strings.Repeat("a", 64),
		WorkBytes:       1, InputName: "in", OutputName: "out", MaxOutputBytes: 1,
		Symlinks: []sandbox.RuntimeSymlink{{GuestPath: "/lib/link", Target: "../usr/lib"}},
	}
	policy := sandbox.Policy{
		Mode: sandbox.SupervisedFileMode, Executable: "/usr/bin/renderer",
		ExecutableSHA256: strings.Repeat("a", 64), Arguments: []string{"renderer"},
		Environment: []string{"LANG=C"}, MaxStdinBytes: 1, MaxStdoutBytes: 1,
		PrivateRoot: &root,
	}
	assert.Error(t, policy.Validate())
}
