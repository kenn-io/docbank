package renderpdf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/internal/providerutil/sandbox"
)

func TestRuntimeDiscoveryRejectsSpecialFiles(t *testing.T) {
	root := t.TempDir()
	socketPath := filepath.Join(root, "socket")
	file, err := os.Create(socketPath)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	require.NoError(t, os.Remove(socketPath))
	require.NoError(t, os.WriteFile(filepath.Join(root, "safe"), []byte("safe"), 0o644))
	manifest, err := DiscoverRuntime([]string{root})
	require.NoError(t, err)
	assert.NotEmpty(t, manifest.Identity)
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
