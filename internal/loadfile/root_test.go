package loadfile

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolverRejectsEmptySource(t *testing.T) {
	t.Chdir(t.TempDir())
	resolver, err := NewResolver(t.Context(), "", nil)
	if resolver != nil {
		require.NoError(t, resolver.Close())
	}
	require.ErrorIs(t, err, ErrUnsafeReference)
}

func TestResolverRejectsSizeChangesAndBoundsOpenReads(t *testing.T) {
	for _, size := range []int64{2, 8} {
		for _, beforeOpen := range []bool{true, false} {
			t.Run(fmt.Sprintf("size=%d/before_open=%t", size, beforeOpen), func(t *testing.T) {
				root := t.TempDir()
				require.NoError(t, os.Mkdir(filepath.Join(root, "VOL001"), 0o700))
				path := filepath.Join(root, "VOL001", "source.txt")
				require.NoError(t, os.WriteFile(path, []byte("data"), 0o600))
				resolver, err := NewResolver(t.Context(), root, nil)
				require.NoError(t, err)
				defer func() { require.NoError(t, resolver.Close()) }()
				if beforeOpen {
					require.NoError(t, os.Truncate(path, size))
				}
				file, err := resolver.Open(Volume{Name: "VOL001", DeclaredRoot: "VOL001"}, "source.txt")
				if beforeOpen {
					if file != nil {
						_ = file.Close()
					}
					require.ErrorIs(t, err, ErrMalformedInput)
					return
				}
				require.NoError(t, err)
				defer func() { _ = file.Close() }()
				require.NoError(t, os.Truncate(path, size))
				data, err := io.ReadAll(file)
				require.NoError(t, err)
				assert.Len(t, data, min(4, int(size)), "reads must stay within the inventoried extent")
				end, err := file.Seek(0, io.SeekEnd)
				require.NoError(t, err)
				assert.Equal(t, int64(4), end, "PDF seeks must use the inventoried extent")
				require.ErrorIs(t, file.Close(), ErrMalformedInput)
			})
		}
	}
}

func TestResolverRejectsCaseFoldCollision(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "VOL001"), 0o700))
	lowerPath := filepath.Join(root, "VOL001", "a.pdf")
	upperPath := filepath.Join(root, "VOL001", "A.PDF")
	require.NoError(t, os.WriteFile(lowerPath, []byte("a"), 0o600))
	require.NoError(t, os.WriteFile(upperPath, []byte("b"), 0o600))
	lowerInfo, err := os.Stat(lowerPath)
	require.NoError(t, err)
	upperInfo, err := os.Stat(upperPath)
	require.NoError(t, err)
	if os.SameFile(lowerInfo, upperInfo) {
		t.Skip("filesystem cannot represent a case-fold collision")
	}
	resolver, err := NewResolver(t.Context(), root, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resolver.Close()) })
	volume := Volume{Name: "VOL001", DeclaredRoot: "VOL001"}
	_, err = resolver.Open(volume, "a.pdf")
	require.ErrorIs(t, err, ErrUnsafeReference)
	_, err = resolver.Open(volume, "A.PDF")
	require.ErrorIs(t, err, ErrUnsafeReference)
}

func TestResolverRejectsDirectorySwapAfterInventory(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "VOL001"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "a.pdf"), []byte("original"), 0o600))
	resolver, err := NewResolver(t.Context(), root, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resolver.Close()) })
	require.NoError(t, os.Rename(filepath.Join(root, "VOL001"), filepath.Join(root, "moved")))
	require.NoError(t, os.Symlink("moved", filepath.Join(root, "VOL001")))
	_, err = resolver.Open(Volume{Name: "VOL001", DeclaredRoot: "VOL001"}, "a.pdf")
	require.ErrorIs(t, err, ErrUnsafeReference)
}

func TestResolverEnforcesConfiguredInventoryBounds(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "VOL001"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "a.pdf"), []byte("1234"), 0o600))
	resolver, err := NewResolver(t.Context(), root, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resolver.Close()) })
	resolver.MaxBytes = 3
	_, err = resolver.Open(Volume{Name: "VOL001", DeclaredRoot: "VOL001"}, "a.pdf")
	require.ErrorIs(t, err, ErrLoadfileLimit)
}

func TestResolverRejectsIndividualObjectAboveIngestLimitBeforeOpeningIt(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "VOL001"), 0o700))
	path := filepath.Join(root, "VOL001", "oversized.pdf")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	require.NoError(t, file.Truncate(1<<32+1))
	require.NoError(t, file.Close())
	resolver, err := NewResolver(t.Context(), root, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resolver.Close()) })
	require.ErrorIs(t, resolver.EnforceMaxFileBytes(1<<32), ErrLoadfileLimit)
	require.NoError(t, resolver.EnforceMaxFileBytes(1<<32+1))
}

func TestResolverDiscoversLoadFilesAndEnforcesVolumeBound(t *testing.T) {
	root := t.TempDir()
	for index := range maxPackageVolumes + 1 {
		name := fmt.Sprintf("DISC%03d", index)
		require.NoError(t, os.Mkdir(filepath.Join(root, name), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(root, name, "source.txt"), []byte("x"), 0o600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "DISC000", "package.dat"), []byte("data"), 0o600))
	resolver, err := NewResolver(t.Context(), root, nil)
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
	bounded, err := NewResolver(t.Context(), boundedRoot, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bounded.Close()) })
	dat, opt, volumes, err = bounded.DiscoverPackageFiles()
	require.NoError(t, err)
	assert.Equal(t, "DISC001/package.dat", dat)
	assert.Equal(t, "DISC001/pages.opt", opt)
	assert.Equal(t, []Volume{{Name: "DISC001", DeclaredRoot: "DISC001", Ordinal: 1}}, volumes)
}

func TestResolverCancelsInventory(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := NewResolver(ctx, syntheticRoot(t), nil)
	require.ErrorIs(t, err, context.Canceled)
}

func TestDiscoveryExplainsRootLoadFilesAndRejectsCompetingPageMaps(t *testing.T) {
	root := syntheticRoot(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "package.dat"), []byte("synthetic"), 0o600))
	resolver, err := NewResolver(t.Context(), root, nil)
	require.NoError(t, err)
	dat, pageMap, volumes, err := resolver.DiscoverPackageFiles()
	assert.Empty(t, dat)
	assert.Empty(t, pageMap)
	assert.Empty(t, volumes)
	require.ErrorContains(t, err, "inside a volume directory")
	require.NoError(t, resolver.Close())
	require.NoError(t, os.Remove(filepath.Join(root, "package.dat")))
	require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "DATA", "pages.lfp"), []byte("## synthetic"), 0o600))
	resolver, err = NewResolver(t.Context(), root, nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, resolver.Close()) }()
	dat, pageMap, volumes, err = resolver.DiscoverPackageFiles()
	assert.Empty(t, dat)
	assert.Empty(t, pageMap)
	assert.Empty(t, volumes)
	require.ErrorIs(t, err, ErrMalformedInput)
}
