package packstore

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLooseRemovePreservesReplacementAtClaimBoundary(t *testing.T) {
	tests := []struct {
		name    string
		replace func(*testing.T, string) func(*testing.T, string)
	}{
		{
			name: "regular file",
			replace: func(t *testing.T, path string) func(*testing.T, string) {
				t.Helper()
				replacement := []byte("foreign regular replacement")
				require.NoError(t, os.WriteFile(path, replacement, 0o600))
				return func(t *testing.T, path string) {
					t.Helper()
					got, err := os.ReadFile(path)
					require.NoError(t, err)
					assert.Equal(t, replacement, got)
				}
			},
		},
		{
			name: "symlink",
			replace: func(t *testing.T, path string) func(*testing.T, string) {
				t.Helper()
				target := filepath.Join(t.TempDir(), "target")
				require.NoError(t, os.WriteFile(target, []byte("target remains"), 0o600))
				require.NoError(t, os.Symlink(target, path))
				return func(t *testing.T, path string) {
					t.Helper()
					require := require.New(t)
					t.Helper()
					info, err := os.Lstat(path)
					require.NoError(err)
					assert.NotZero(t, info.Mode()&os.ModeSymlink)
					gotTarget, err := os.Readlink(path)
					require.NoError(err)
					assert.Equal(t, target, gotTarget)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := []byte("intended loose object")
			store := newLooseStoreForTest(t, StagingSameDirectory)
			written, err := store.WriteBytes(t.Context(), content, WriteOptions{
				Durability: AtomicPublication,
				Dedup:      VerifyFullHash,
			})
			require.NoError(t, err)
			held := installLooseRemovalReplacement(t, written.Path, tt.replace)

			err = store.Remove(written.Hash, BestEffortRemoval)

			require.ErrorIs(t, err, errIdentityChanged)
			assert.FileExists(t, held, "the displaced intended object remains outside the cleanup path")
			assertNoLooseRemovalClaims(t, written.Path)
		})
	}
}

func TestLooseRemoveDoesNotClobberNewerOccupantWhileRestoringForeignClaim(t *testing.T) {
	assert := assert.New(t)

	content := []byte("intended loose object")
	store := newLooseStoreForTest(t, StagingSameDirectory)
	written, err := store.WriteBytes(t.Context(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})
	require.NoError(t, err)
	foreign := []byte("foreign replacement claimed for removal")
	_ = installLooseRemovalReplacement(t, written.Path, func(t *testing.T, path string) func(*testing.T, string) {
		t.Helper()
		require.NoError(t, os.WriteFile(path, foreign, 0o600))
		return func(*testing.T, string) {}
	})
	newer := []byte("newer occupant must win")
	originalPublish := publishLooseRemovalRestoreFile
	publishLooseRemovalRestoreFile = func(stagingPath, newPath string) error {
		f, createErr := os.OpenFile(newPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if createErr != nil {
			return createErr
		}
		_, writeErr := f.Write(newer)
		closeErr := f.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			return err
		}
		return originalPublish(stagingPath, newPath)
	}
	t.Cleanup(func() { publishLooseRemovalRestoreFile = originalPublish })

	err = store.Remove(written.Hash, BestEffortRemoval)

	require.ErrorIs(t, err, errIdentityChanged)
	require.ErrorIs(t, err, fs.ErrExist)
	assert.Equal(newer, mustReadFile(t, written.Path))
	claims, globErr := filepath.Glob(filepath.Join(
		filepath.Dir(written.Path), "."+filepath.Base(written.Path)+".remove-*",
	))
	require.NoError(t, globErr)
	require.Len(t, claims, 1, "the un-restorable foreign entry remains preserved")
	assert.Equal(foreign, mustReadFile(t, filepath.Join(claims[0], "claimed")))
}

func TestLooseRemovePublishesCompleteRegularRestoreAtomically(t *testing.T) {
	store := newLooseStoreForTest(t, StagingSameDirectory)
	written, err := store.WriteBytes(t.Context(), []byte("atomic restore source"), WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})
	require.NoError(t, err)
	foreign := bytes.Repeat([]byte("complete foreign restoration\n"), 4096)
	_ = installLooseRemovalReplacement(t, written.Path, func(t *testing.T, path string) func(*testing.T, string) {
		t.Helper()
		require.NoError(t, os.WriteFile(path, foreign, 0o600))
		return func(t *testing.T, path string) {
			t.Helper()
			assert.Equal(t, foreign, mustReadFile(t, path))
		}
	})
	originalBeforePublish := beforeLooseRemovalRestorePublish
	publishReached := false
	beforeLooseRemovalRestorePublish = func(stagingPath, canonicalPath string) {
		publishReached = true
		assert.Equal(t, foreign, mustReadFile(t, stagingPath), "private staging is complete before publication")
		_, statErr := os.Lstat(canonicalPath)
		require.ErrorIs(t, statErr, fs.ErrNotExist, "readers cannot observe restoration while it is being copied")
	}
	t.Cleanup(func() { beforeLooseRemovalRestorePublish = originalBeforePublish })

	err = store.Remove(written.Hash, BestEffortRemoval)

	require.ErrorIs(t, err, errIdentityChanged)
	assert.True(t, publishReached)
	assert.Equal(t, foreign, mustReadFile(t, written.Path))
	assertNoLooseRemovalClaims(t, written.Path)
}

func TestLooseRemoveRestoresConcurrentClaimWrites(t *testing.T) {
	assert := assert.New(t)

	store := newLooseStoreForTest(t, StagingSameDirectory)
	written, err := store.WriteBytes(t.Context(), []byte("concurrent restore source"), WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})
	require.NoError(t, err)
	foreign := []byte("foreign replacement before restore")
	_ = installLooseRemovalReplacement(t, written.Path, func(t *testing.T, path string) func(*testing.T, string) {
		t.Helper()
		require.NoError(t, os.WriteFile(path, foreign, 0o600))
		return func(*testing.T, string) {}
	})
	updated := []byte("foreign replacement after writer update")
	originalBeforePublish := beforeLooseRemovalRestorePublish
	writerRan := false
	var writerIdentity fs.FileInfo
	beforeLooseRemovalRestorePublish = func(sourcePath, canonicalPath string) {
		writerRan = true
		writer, openErr := os.OpenFile(
			filepath.Join(filepath.Dir(sourcePath), "claimed"),
			os.O_WRONLY|os.O_TRUNC,
			0,
		)
		require.NoError(t, openErr)
		writerIdentity, openErr = writer.Stat()
		require.NoError(t, openErr)
		_, writeErr := writer.Write(updated)
		require.NoError(t, writeErr)
		require.NoError(t, writer.Sync())
		require.NoError(t, writer.Close())
		_, statErr := os.Lstat(canonicalPath)
		require.ErrorIs(t, statErr, fs.ErrNotExist)
	}
	t.Cleanup(func() { beforeLooseRemovalRestorePublish = originalBeforePublish })

	err = store.Remove(written.Hash, BestEffortRemoval)

	require.ErrorIs(t, err, errIdentityChanged)
	assert.True(writerRan)
	assert.Equal(updated, mustReadFile(t, written.Path))
	canonicalIdentity, statErr := os.Stat(written.Path)
	require.NoError(t, statErr)
	assert.True(os.SameFile(writerIdentity, canonicalIdentity), "restoration keeps the writer's exact inode")
	assertNoLooseRemovalClaims(t, written.Path)
}

func TestLooseRemoveTreatsPostPublicationReplacementAsLaterAction(t *testing.T) {
	tests := []struct {
		name    string
		replace func(*testing.T, string) func(*testing.T, string)
		check   func(*testing.T, string)
	}{
		{
			name: "regular file",
			replace: func(t *testing.T, path string) func(*testing.T, string) {
				t.Helper()
				require.NoError(t, os.WriteFile(path, []byte("published regular foreign entry"), 0o600))
				return func(*testing.T, string) {}
			},
			check: func(t *testing.T, path string) {
				t.Helper()
				assert.Equal(t, []byte("published regular foreign entry"), mustReadFile(t, path))
			},
		},
		{
			name: "symlink",
			replace: func(t *testing.T, path string) func(*testing.T, string) {
				t.Helper()
				target := filepath.Join(t.TempDir(), "foreign-target")
				require.NoError(t, os.WriteFile(target, []byte("target"), 0o600))
				require.NoError(t, os.Symlink(target, path))
				return func(*testing.T, string) {}
			},
			check: func(t *testing.T, path string) {
				t.Helper()
				info, err := os.Lstat(path)
				require.NoError(t, err)
				assert.NotZero(t, info.Mode()&os.ModeSymlink)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			store := newLooseStoreForTest(t, StagingSameDirectory)
			written, err := store.WriteBytes(t.Context(), []byte("post-publication source"), WriteOptions{
				Durability: AtomicPublication,
				Dedup:      VerifyFullHash,
			})
			require.NoError(err)
			_ = installLooseRemovalReplacement(t, written.Path, tt.replace)
			newer := []byte("later external occupant")
			originalAfterPublish := afterLooseRemovalRestorePublish
			published := false
			afterLooseRemovalRestorePublish = func(path string) {
				published = true
				tt.check(t, path)
				require.NoError(os.Remove(path))
				require.NoError(os.WriteFile(path, newer, 0o600))
			}
			t.Cleanup(func() { afterLooseRemovalRestorePublish = originalAfterPublish })

			err = store.Remove(written.Hash, BestEffortRemoval)

			require.ErrorIs(err, errIdentityChanged)
			assert.True(published)
			assert.Equal(newer, mustReadFile(t, written.Path))
			assertNoLooseRemovalClaims(t, written.Path)
		})
	}
}

func TestLooseRemoveDoesNotClobberPreexistingAsideName(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	store := newLooseStoreForTest(t, StagingSameDirectory)
	written, err := store.WriteBytes(t.Context(), []byte("aside collision source"), WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})
	require.NoError(err)
	originalCreate := createLooseRemovalAside
	var collision string
	createLooseRemovalAside = func(path string) error {
		if collision == "" {
			collision = path
			require.NoError(os.Mkdir(path, 0o700))
			require.NoError(os.WriteFile(filepath.Join(path, "owner"), []byte("preexisting"), 0o600))
			return fs.ErrExist
		}
		return originalCreate(path)
	}
	t.Cleanup(func() { createLooseRemovalAside = originalCreate })

	err = store.Remove(written.Hash, BestEffortRemoval)

	require.NoError(err)
	assert.NoFileExists(written.Path)
	require.NotEmpty(collision)
	assert.Equal([]byte("preexisting"), mustReadFile(t, filepath.Join(collision, "owner")))
}

func TestLooseRemovePreservesUnsupportedForeignReplacementInAside(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	store := newLooseStoreForTest(t, StagingSameDirectory)
	written, err := store.WriteBytes(t.Context(), []byte("unsupported replacement source"), WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})
	require.NoError(err)
	held := written.Path + ".held"
	originalHook := beforeLooseRemovalClaim
	triggered := false
	beforeLooseRemovalClaim = func(path string) {
		if triggered {
			return
		}
		triggered = true
		require.NoError(os.Rename(path, held))
		require.NoError(os.Mkdir(path, 0o700))
		require.NoError(os.WriteFile(filepath.Join(path, "owner"), []byte("foreign directory"), 0o600))
	}
	t.Cleanup(func() { beforeLooseRemovalClaim = originalHook })

	err = store.Remove(written.Hash, BestEffortRemoval)

	require.ErrorIs(err, errIdentityChanged)
	require.ErrorContains(err, "unsupported mode")
	assert.FileExists(held)
	assert.NoDirExists(written.Path, "unsupported entries are never recreated unsafely")
	claims, globErr := filepath.Glob(filepath.Join(
		filepath.Dir(written.Path), "."+filepath.Base(written.Path)+".remove-*",
	))
	require.NoError(globErr)
	require.Len(claims, 1)
	assert.Equal([]byte("foreign directory"), mustReadFile(t, filepath.Join(claims[0], "claimed", "owner")))
}

func TestPackSweepPreservesReplacementAtClaimBoundary(t *testing.T) {
	layout := layoutForStoreTest(t)
	content := []byte("redundant indexed loose object")
	entry := buildStoreTestPack(t, layout, content)
	require.Equal(t, entry.Hash, writeMaintenanceLoose(t, layout, content))
	catalog := newMaintenanceCatalog()
	catalog.members[entry.Hash] = Reference{Hash: entry.Hash}
	catalog.entries[entry.Hash] = entry
	catalog.packs[entry.PackID] = PackRecord{PackID: entry.PackID, EntryCount: 1, StoredBytes: entry.StoredLen}
	path := layout.LoosePath(entry.Hash)
	replacement := []byte("foreign indexed replacement")
	held := installLooseRemovalReplacement(t, path, func(t *testing.T, path string) func(*testing.T, string) {
		t.Helper()
		require.NoError(t, os.WriteFile(path, replacement, 0o600))
		return func(t *testing.T, path string) {
			t.Helper()
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, replacement, got)
		}
	})
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(t.Context(), PackOptions{})

	require.ErrorIs(t, err, errIdentityChanged)
	assert.Zero(t, stats.LooseSwept)
	assert.FileExists(t, held)
	assert.Equal(t, replacement, mustReadFile(t, path))
	assertNoLooseRemovalClaims(t, path)
}

func TestPackOrphanSweepPreservesReplacementAtClaimBoundary(t *testing.T) {
	layout := layoutForStoreTest(t)
	hash := writeMaintenanceLoose(t, layout, []byte("orphan loose object"))
	path := layout.LoosePath(hash)
	replacement := []byte("foreign orphan replacement")
	held := installLooseRemovalReplacement(t, path, func(t *testing.T, path string) func(*testing.T, string) {
		t.Helper()
		require.NoError(t, os.WriteFile(path, replacement, 0o600))
		return func(t *testing.T, path string) {
			t.Helper()
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, replacement, got)
		}
	})
	maintainer := newMaintainerForTest(t, newMaintenanceCatalog(), layout, DefaultLimits())

	stats, err := maintainer.Pack(t.Context(), PackOptions{})

	require.ErrorIs(t, err, errIdentityChanged)
	assert.Zero(t, stats.LooseOrphansRemoved)
	assert.FileExists(t, held)
	assert.Equal(t, replacement, mustReadFile(t, path))
	assertNoLooseRemovalClaims(t, path)
}

func TestPackReportsNoncanonicalSourceClaimFailureAfterCatalogCommit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	layout := layoutForStoreTest(t)
	content := bytes.Repeat([]byte("noncanonical packed source\n"), 32)
	hash := hashForTest(content)
	path := filepath.Join(layout.Root(), "legacy", hash.String())
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(os.WriteFile(path, content, 0o600))
	catalog := newMaintenanceCatalog()
	addMaintenanceCandidate(catalog, Candidate{Hash: hash, Paths: []string{path}, Size: int64(len(content))})
	claimErr := errors.New("injected packed source claim failure")
	originalClaim := claimLooseRemovalPath
	claimLooseRemovalPath = func(oldPath, asidePath string) error {
		if filepath.Clean(oldPath) == filepath.Clean(path) {
			return claimErr
		}
		return originalClaim(oldPath, asidePath)
	}
	t.Cleanup(func() { claimLooseRemovalPath = originalClaim })
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(t.Context(), PackOptions{})

	require.ErrorIs(err, claimErr)
	assert.Equal(1, stats.PacksSealed)
	assert.Equal(1, stats.BlobsPacked)
	assert.FileExists(path, "failed cleanup remains retry-visible")
	got, _ := readStoreTest(t, maintainer.store, hash)
	assert.Equal(content, got, "the committed pack remains authoritative")
}

func installLooseRemovalReplacement(
	t *testing.T,
	path string,
	replace func(*testing.T, string) func(*testing.T, string),
) string {
	t.Helper()
	originalHook := beforeLooseRemovalClaim
	held := path + ".held"
	triggered := false
	var check func(*testing.T, string)
	beforeLooseRemovalClaim = func(gotPath string) {
		if triggered || filepath.Clean(gotPath) != filepath.Clean(path) {
			return
		}
		triggered = true
		require.NoError(t, os.Rename(path, held))
		check = replace(t, path)
	}
	t.Cleanup(func() { beforeLooseRemovalClaim = originalHook })
	t.Cleanup(func() {
		require.True(t, triggered, "cleanup reached the deterministic claim boundary")
		if check != nil {
			check(t, path)
		}
	})
	return held
}

func assertNoLooseRemovalClaims(t *testing.T, canonical string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(
		filepath.Dir(canonical), "."+filepath.Base(canonical)+".remove-*",
	))
	require.NoError(t, err)
	assert.Empty(t, matches)
}
