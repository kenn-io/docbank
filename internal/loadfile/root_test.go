package loadfile

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolverRejectsCaseFoldCollision(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "VOL001"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "a.pdf"), []byte("a"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "A.PDF"), []byte("b"), 0o600))
	resolver, err := NewResolver(root, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resolver.Close()) })
	volume := Volume{Name: "VOL001", DeclaredRoot: "VOL001"}
	_, err = resolver.Resolve(volume, "a.pdf")
	require.ErrorIs(t, err, ErrUnsafeReference)
	_, err = resolver.Resolve(volume, "A.PDF")
	require.ErrorIs(t, err, ErrUnsafeReference)
}

func TestResolverRejectsDirectorySwapAfterInventory(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "VOL001"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "a.pdf"), []byte("original"), 0o600))
	resolver, err := NewResolver(root, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resolver.Close()) })
	require.NoError(t, os.Rename(filepath.Join(root, "VOL001"), filepath.Join(root, "moved")))
	require.NoError(t, os.Symlink("moved", filepath.Join(root, "VOL001")))
	_, err = resolver.Resolve(Volume{Name: "VOL001", DeclaredRoot: "VOL001"}, "a.pdf")
	require.ErrorIs(t, err, ErrUnsafeReference)
}

func TestResolverEnforcesConfiguredInventoryBounds(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "VOL001"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "a.pdf"), []byte("1234"), 0o600))
	resolver, err := NewResolver(root, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resolver.Close()) })
	resolver.MaxBytes = 3
	_, err = resolver.Resolve(Volume{Name: "VOL001", DeclaredRoot: "VOL001"}, "a.pdf")
	require.ErrorIs(t, err, ErrLoadfileLimit)
}

func TestResolverDiscoversLoadFilesAndEnforcesVolumeBound(t *testing.T) {
	root := t.TempDir()
	for index := range maxPackageVolumes + 1 {
		name := fmt.Sprintf("DISC%03d", index)
		require.NoError(t, os.Mkdir(filepath.Join(root, name), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(root, name, "source.txt"), []byte("x"), 0o600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "DISC000", "package.dat"), []byte("data"), 0o600))
	resolver, err := NewResolver(root, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resolver.Close()) })
	dat, opt, volumes, err := resolver.DiscoverPackageFiles()
	require.ErrorIs(t, err, ErrLoadfileLimit)
	assert.Empty(t, dat)
	assert.Empty(t, opt)
	assert.Empty(t, volumes)

	boundedRoot := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(boundedRoot, "DISC001"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(boundedRoot, "DISC001", "package.dat"), []byte("data"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(boundedRoot, "DISC001", "pages.opt"), []byte("pages"), 0o600))
	bounded, err := NewResolver(boundedRoot, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bounded.Close()) })
	dat, opt, volumes, err = bounded.DiscoverPackageFiles()
	require.NoError(t, err)
	assert.Equal(t, "DISC001/package.dat", dat)
	assert.Equal(t, "DISC001/pages.opt", opt)
	assert.Equal(t, []Volume{{Name: "DISC001", DeclaredRoot: "DISC001", Ordinal: 1}}, volumes)
}
