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

func TestDefaultRuntimeRootsFollowArchitecture(t *testing.T) {
	assert.Contains(t, defaultRuntimeRootsForArch("amd64"), "/usr/lib/x86_64-linux-gnu")
	assert.Contains(t, defaultRuntimeRootsForArch("amd64"), "/lib/x86_64-linux-gnu")
	assert.Contains(t, defaultRuntimeRootsForArch("amd64"), "/lib64/ld-linux-x86-64.so.2")
	assert.NotContains(t, defaultRuntimeRootsForArch("amd64"), "/usr/lib/aarch64-linux-gnu")
	assert.NotContains(t, defaultRuntimeRootsForArch("amd64"), "/lib/aarch64-linux-gnu")
	assert.Contains(t, defaultRuntimeRootsForArch("arm64"), "/usr/lib/aarch64-linux-gnu")
	assert.Contains(t, defaultRuntimeRootsForArch("arm64"), "/lib/aarch64-linux-gnu")
	assert.Contains(t, defaultRuntimeRootsForArch("arm64"), "/lib/ld-linux-aarch64.so.1")
	assert.NotContains(t, defaultRuntimeRootsForArch("arm64"), "/usr/lib/x86_64-linux-gnu")
	assert.NotContains(t, defaultRuntimeRootsForArch("arm64"), "/lib/x86_64-linux-gnu")
}

func TestRuntimeDiscoveryExpandsDirectorySymlinkRoot(t *testing.T) {
	for _, relative := range []bool{false, true} {
		t.Run(strconv.FormatBool(relative), func(t *testing.T) {
			parent := t.TempDir()
			target := filepath.Join(parent, "runtime")
			require.NoError(t, os.Mkdir(target, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(target, "file"), []byte("runtime"), 0o644))
			link := filepath.Join(parent, "runtime-link")
			linkTarget := target
			if relative {
				linkTarget = "runtime"
			}
			require.NoError(t, os.Symlink(linkTarget, link))

			manifest, err := DiscoverRuntime([]string{link})
			require.NoError(t, err)
			require.Len(t, manifest.Files, 1)
			assert.Equal(t, filepath.Join(link, "file"), manifest.Files[0].GuestPath)
		})
	}
}

func TestRuntimeDiscoveryMaterializesSelectedFileSymlink(t *testing.T) {
	parent := t.TempDir()
	targetDirectory := filepath.Join(parent, "architecture")
	require.NoError(t, os.Mkdir(targetDirectory, 0o755))
	target := filepath.Join(targetDirectory, "loader")
	require.NoError(t, os.WriteFile(target, []byte("loader"), 0o755))
	link := filepath.Join(parent, "ld-linux.so.1")
	require.NoError(t, os.Symlink(filepath.Join("architecture", "loader"), link))

	manifest, err := DiscoverRuntime([]string{link})
	require.NoError(t, err)
	require.Len(t, manifest.Files, 1)
	assert.Equal(t, link, manifest.Files[0].GuestPath)
	targetInfo, err := os.Stat(target)
	require.NoError(t, err)
	sourceInfo, err := os.Stat(manifest.Files[0].SourcePath)
	require.NoError(t, err)
	assert.True(t, os.SameFile(targetInfo, sourceInfo))
	assert.Empty(t, manifest.Symlinks)
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
			WarmupInput: []byte("warmup"), WarmupInputSHA256: "c6cf1309cd700e5a84e18d0b1d5877b9a608141037ac40445d484398256fc56c",
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
		WarmupInput: []byte("warmup"), WarmupInputSHA256: "c6cf1309cd700e5a84e18d0b1d5877b9a608141037ac40445d484398256fc56c",
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
