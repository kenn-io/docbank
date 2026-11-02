//go:build unix

package packstore

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

const (
	unreadableRemovalChild       = "KIT_PACKSTORE_UNREADABLE_REMOVAL_CHILD"
	nonWritableShardPackingChild = "KIT_PACKSTORE_NONWRITABLE_SHARD_PACKING_CHILD"
)

func TestPackRetainsReadableLooseSourceWhenShardDeniesCleanup(t *testing.T) {
	if os.Getenv(nonWritableShardPackingChild) != "" {
		runNonWritableShardPackingChild(t)
		return
	}
	command := exec.CommandContext(context.WithoutCancel(t.Context()), os.Args[0], "-test.run=^TestPackRetainsReadableLooseSourceWhenShardDeniesCleanup$")
	command.Env = append(os.Environ(), nonWritableShardPackingChild+"=1")
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
}

func runNonWritableShardPackingChild(t *testing.T) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)
	t.Helper()
	root, err := os.MkdirTemp("", "kit-packstore-nonwritable-shard-") //nolint:usetesting // the child process helper owns this directory across the re-exec boundary
	require.NoError(err)
	t.Cleanup(func() { require.NoError(os.RemoveAll(root)) })
	layout, err := NewLayout(root, LayoutOptions{Staging: StagingStoreDirectory, StagingDir: "tmp"})
	require.NoError(err)
	content := []byte("readable loose source in non-writable shard")
	hash := writeMaintenanceLoose(t, layout, content)
	path := layout.LoosePath(hash)
	shard := filepath.Dir(path)
	require.NoError(os.Chmod(shard, 0o500))
	dropUnreadableRemovalPrivileges(t, root)
	t.Cleanup(func() { require.NoError(os.Chmod(shard, 0o700)) })
	readable, err := os.Open(path)
	require.NoError(err, "fixture must remain readable")
	require.NoError(readable.Close())
	catalog := newMaintenanceCatalog()
	catalog.addLoose(hash, path)
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(t.Context(), PackOptions{})
	require.NoError(err)
	assert.Equal(1, stats.BlobsPacked)
	assert.Zero(stats.BlobsCorrupt)
	assert.FileExists(path)
	assertNoLooseRemovalClaims(t, path)
	location, err := catalog.Resolve(t.Context(), hash)
	require.NoError(err)
	require.NotNil(location.Pack)
	got, _ := readStoreTest(t, maintainer.store, hash)
	assert.Equal(content, got)
}

func TestUnreadableLooseRemovalUsesIdentityOnlyPin(t *testing.T) {
	if mode := os.Getenv(unreadableRemovalChild); mode != "" {
		runUnreadableRemovalChild(t, mode)
		return
	}
	for _, mode := range []string{"explicit", "orphan", "replacement"} {
		t.Run(mode, func(t *testing.T) {
			command := exec.CommandContext(context.WithoutCancel(t.Context()), os.Args[0], "-test.run=^TestUnreadableLooseRemovalUsesIdentityOnlyPin$")
			command.Env = append(os.Environ(), unreadableRemovalChild+"="+mode)
			output, err := command.CombinedOutput()
			require.NoError(t, err, string(output))
		})
	}
}

func runUnreadableRemovalChild(t *testing.T, mode string) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)
	t.Helper()
	root, err := os.MkdirTemp("", "kit-packstore-unreadable-") //nolint:usetesting // the child process helper owns this directory across the re-exec boundary
	require.NoError(err)
	t.Cleanup(func() { require.NoError(os.RemoveAll(root)) })
	layout, err := NewLayout(root, LayoutOptions{Staging: StagingStoreDirectory, StagingDir: "tmp"})
	require.NoError(err)
	content := fmt.Appendf(nil, "unreadable %s loose content", mode)
	hash := writeMaintenanceLoose(t, layout, content)
	path := layout.LoosePath(hash)
	require.NoError(os.Chmod(path, 0))
	dropUnreadableRemovalPrivileges(t, root)
	file, openErr := os.Open(path)
	if openErr == nil {
		require.NoError(file.Close())
	}
	require.Error(openErr, "fixture must genuinely deny ordinary read access")

	switch mode {
	case "explicit":
		store, err := NewLooseStore(layout)
		require.NoError(err)
		require.NoError(store.Remove(hash, BestEffortRemoval))
		assert.NoFileExists(path)
		assertNoIdentityPinDebris(t, path)
	case "orphan":
		catalog := newMaintenanceCatalog()
		maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
		stats, err := maintainer.Pack(t.Context(), PackOptions{})
		require.NoError(err)
		assert.Equal(1, stats.LooseOrphansRemoved)
		assert.NoFileExists(path)
		assertNoIdentityPinDebris(t, path)
	case "replacement":
		replacement := []byte("unreadable raced replacement")
		held := path + ".held"
		originalHook := beforeLooseRemovalClaim
		var replacementIdentity os.FileInfo
		beforeLooseRemovalClaim = func(gotPath string) {
			if replacementIdentity != nil || filepath.Clean(gotPath) != filepath.Clean(path) {
				return
			}
			require.NoError(os.Rename(path, held))
			require.NoError(os.WriteFile(path, replacement, 0))
			replacementIdentity, err = os.Lstat(path)
			require.NoError(err)
		}
		t.Cleanup(func() { beforeLooseRemovalClaim = originalHook })
		store, err := NewLooseStore(layout)
		require.NoError(err)

		err = store.Remove(hash, BestEffortRemoval)
		require.ErrorIs(err, errIdentityChanged)
		require.NotNil(replacementIdentity)
		canonicalIdentity, statErr := os.Lstat(path)
		require.NoError(statErr)
		assert.True(os.SameFile(replacementIdentity, canonicalIdentity), "the exact unreadable replacement wins the removal race")
		assert.FileExists(held)
		assertNoLooseRemovalClaims(t, path)
		assertNoIdentityPinDebris(t, path)
	default:
		require.FailNow("unknown unreadable-removal child mode", mode)
	}
}

func assertNoIdentityPinDebris(t *testing.T, canonical string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(
		filepath.Dir(canonical), "."+filepath.Base(canonical)+".pin-*",
	))
	require.NoError(t, err)
	assert.Empty(t, matches)
}

func dropUnreadableRemovalPrivileges(t *testing.T, root string) {
	t.Helper()
	require := require.New(t)
	t.Helper()
	if os.Geteuid() != 0 {
		return
	}
	const nobody = 65534
	require.NoError(filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		return os.Chown(path, nobody, nobody)
	}))
	require.NoError(unix.Setgroups(nil))
	require.NoError(unix.Setgid(nobody))
	require.NoError(unix.Setuid(nobody))
	// Some kernels retain filesystem capability state until the credential
	// transition completes. A short stat boundary keeps the fixture ordering
	// explicit without depending on elapsed time.
	_, err := os.Stat(root)
	require.NoError(err)
}
