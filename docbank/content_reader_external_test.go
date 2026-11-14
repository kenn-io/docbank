package docbank_test

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	docbank "go.kenn.io/docbank/docbank"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/store"
	docsqlite "go.kenn.io/docbank/sqlite"
)

func assertReaderContent(t *testing.T, reader *docbank.ContentReader, path, want string) {
	t.Helper()
	content, err := reader.OpenContent(t.Context(), path)
	require.NoError(t, err)
	data, err := io.ReadAll(content.Reader)
	require.NoError(t, err)
	require.Equal(t, want, string(data))
	require.NoError(t, content.Reader.Verify())
	require.NoError(t, content.Reader.Close())
}

func assertReaderRefusedUnchanged(t *testing.T, root string, check func(error)) {
	t.Helper()
	stabilizeReaderTree(t, root)
	before := snapshotReaderTree(t, root)
	_, err := docbank.OpenContentReader(t.Context(), docbank.ContentReaderConfig{Root: root})
	check(err)
	require.Equal(t, before, snapshotReaderTree(t, root))
}

func snapshotReaderTree(t *testing.T, root string) []string {
	t.Helper()
	var entries []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("reading entry info for %s: %w", path, err)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		var hash [32]byte
		target := ""
		if info.Mode()&os.ModeSymlink != 0 {
			target, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			hash = sha256.Sum256(data)
		}
		entries = append(entries, fmt.Sprintf("%s %s %d %d %x %s", relative, info.Mode(), info.Size(), info.ModTime().UnixNano(), hash, target))
		return nil
	})
	require.NoError(t, err)
	sort.Strings(entries)
	return entries
}

// Windows can defer directory timestamp updates from fixture writes until a
// later handle closes. Set them explicitly before taking the baseline.
func stabilizeReaderTree(t *testing.T, root string) {
	t.Helper()
	if runtime.GOOS != "windows" {
		return
	}
	stamp := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Chtimes(path, stamp, stamp)
		}
		return nil
	})
	require.NoError(t, err)
}

func TestContentReaderRefusesRecoveryWithoutChanges(t *testing.T) {
	t.Parallel()
	type refusalCase struct {
		name        string
		apply       func(*testing.T, string)
		ownerNeeded bool
		message     string
	}
	cases := []refusalCase{
		{"missing marker", func(t *testing.T, root string) {
			t.Helper()
			require.NoError(t, os.Remove(filepath.Join(root, "blobs", ".packstore-owner.json")))
		}, true, "missing primary storage"},
		{"missing blobs", func(t *testing.T, root string) {
			t.Helper()
			require.NoError(t, os.RemoveAll(filepath.Join(root, "blobs")))
		}, true, "missing primary storage"},
		{"restore handoff", func(t *testing.T, root string) {
			t.Helper()
			backend, err := blob.NewFilesystemBackend(filepath.Join(root, "blobs"), nil)
			require.NoError(t, err)
			prior, err := backend.Ownership(t.Context())
			require.NoError(t, err)
			require.NoError(t, backend.Close())
			next := prior
			next.Epoch = "50000000-0000-4000-8000-000000000001"
			database, err := os.ReadFile(filepath.Join(root, "docbank.db"))
			require.NoError(t, err)
			digest := fmt.Sprintf("%x", sha256.Sum256(database))
			handoff, err := blob.NewPrimaryRestoreHandoff(filepath.Join(root, "blobs"), next, &digest)
			require.NoError(t, err)
			require.NoError(t, handoff.Prepare(t.Context()))
		}, true, "pending restore handoff"},
	}
	for _, name := range []string{"handoff directory", "handoff symlink"} {
		cases = append(cases, refusalCase{name, func(t *testing.T, root string) {
			t.Helper()
			path := filepath.Join(root, "blobs", "tmp", "primary-restore-handoff.json")
			if strings.HasSuffix(name, "directory") {
				require.NoError(t, os.Mkdir(path, 0o700))
			} else if err := os.Symlink(filepath.Join(t.TempDir(), "target"), path); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
		}, false, "not a regular file"})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			vault, err := docbank.New(t.Context(), docbank.Config{Root: root})
			require.NoError(t, err)
			require.NoError(t, vault.Close())
			tc.apply(t, root)
			assertReaderRefusedUnchanged(t, root, func(err error) {
				t.Helper()
				if tc.ownerNeeded {
					require.ErrorIs(t, err, docbank.ErrVaultNeedsOwner)
				} else {
					require.NotErrorIs(t, err, docbank.ErrVaultNeedsOwner)
				}
				if tc.message != "" {
					require.ErrorContains(t, err, tc.message)
				}
			})
			lock, err := (home.Layout{Root: root}).TryLockExclusive()
			require.NoError(t, err)
			require.NoError(t, lock.Release())
		})
	}
}

func TestContentReaderCloseWaitsForStream(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	v, err := docbank.New(t.Context(), docbank.Config{Root: root})
	require.NoError(t, err)
	_, err = v.Put(t.Context(), "/source", strings.NewReader("synthetic"), docbank.PutOptions{})
	require.NoError(t, err)
	require.NoError(t, v.Close())
	synctest.Test(t, func(t *testing.T) {
		reader, err := docbank.OpenContentReader(t.Context(), docbank.ContentReaderConfig{Root: root})
		require.NoError(t, err)
		content, err := reader.OpenContent(t.Context(), "/source")
		require.NoError(t, err)
		done := make(chan error, 1)
		go func() { done <- reader.Close() }()
		synctest.Wait()
		select {
		case err := <-done:
			require.FailNow(t, "reader closed with an open stream", "error: %v", err)
		default:
		}
		data, err := io.ReadAll(content.Reader)
		require.NoError(t, err)
		require.Equal(t, "synthetic", string(data))
		require.NoError(t, content.Reader.Verify())
		require.NoError(t, content.Reader.Close())
		require.NoError(t, <-done)
		require.NoError(t, reader.Close())
	})
}

func TestNilContentReader(t *testing.T) {
	t.Parallel()
	var reader *docbank.ContentReader
	require.Empty(t, reader.ID())
	_, err := reader.OpenContent(t.Context(), "/source")
	require.ErrorIs(t, err, docbank.ErrClosed)
	require.NoError(t, reader.Close())
}

func TestContentReaderRejectsRegistryInsideVault(t *testing.T) {
	root := t.TempDir()
	vault, err := docbank.New(t.Context(), docbank.Config{Root: root})
	require.NoError(t, err)
	require.NoError(t, vault.Close())
	t.Setenv("DOCBANK_LOCK_DIR", filepath.Join(root, "missing", "locks"))
	assertReaderRefusedUnchanged(t, root, func(err error) {
		t.Helper()
		require.ErrorContains(t, err, "DOCBANK_LOCK_DIR")
		require.NotErrorIs(t, err, docbank.ErrVaultNeedsOwner)
	})
}

type recordingScratchDriver struct {
	docsqlite.Driver

	paths []string
}

func (d *recordingScratchDriver) Open(path string, opts docsqlite.OpenOptions) (*sql.DB, error) {
	if opts.Access == docsqlite.Create {
		d.paths = append(d.paths, path)
	}
	return d.Driver.Open(path, opts)
}

func TestContentReaderStorageUnchanged(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	_, err := docbank.OpenContentReader(t.Context(), docbank.ContentReaderConfig{Root: missing})
	require.ErrorIs(t, err, docbank.ErrVaultNeedsOwner)
	_, err = os.Lstat(missing)
	require.ErrorIs(t, err, os.ErrNotExist)

	for _, kind := range []string{"loose", "packed", "tampered", "external registry"} {
		t.Run(kind, func(t *testing.T) {
			registry := ""
			if kind == "external registry" {
				registry = t.TempDir()
				t.Setenv("DOCBANK_LOCK_DIR", registry)
			} else {
				t.Parallel()
			}
			root := t.TempDir()
			vault, err := docbank.New(t.Context(), docbank.Config{Root: root})
			require.NoError(t, err)
			data := "synthetic original"
			receipt, err := vault.Put(t.Context(), "/source", strings.NewReader(data), docbank.PutOptions{})
			require.NoError(t, err)
			id := vault.ID()
			if kind == "packed" {
				report, err := vault.Pack(t.Context(), docbank.PackOptions{})
				require.NoError(t, err)
				require.GreaterOrEqual(t, report.BlobsPacked, 1)
				require.NoFileExists(t, looseBlobPath(root, receipt.Computed.SHA256))
			}
			require.NoError(t, vault.Close())
			if kind == "tampered" {
				path := looseBlobPath(root, receipt.Computed.SHA256)
				damage, err := os.ReadFile(path)
				require.NoError(t, err)
				damage[0] ^= 1
				require.NoError(t, os.WriteFile(path, damage, 0o600))
			}
			stabilizeReaderTree(t, root)
			before := snapshotReaderTree(t, root)
			config := docbank.ContentReaderConfig{Root: root}
			var driver *recordingScratchDriver
			if kind == "external registry" {
				driver = &recordingScratchDriver{Driver: store.DefaultSQLiteDriver()}
				config.SQLite = driver
			}
			reader, err := docbank.OpenContentReader(t.Context(), config)
			require.NoError(t, err)
			if driver != nil {
				require.Len(t, driver.paths, 1)
				canonicalRegistry, err := filepath.EvalSymlinks(registry)
				require.NoError(t, err)
				require.Equal(t, canonicalRegistry, filepath.Dir(driver.paths[0]))
			}
			if kind == "loose" {
				require.Equal(t, id, reader.ID())
				_, err = docbank.New(t.Context(), docbank.Config{Root: root})
				require.ErrorIs(t, err, docbank.ErrVaultLocked)
			}
			if kind == "tampered" {
				content, err := reader.OpenContent(t.Context(), "/source")
				require.NoError(t, err)
				_, err = io.ReadAll(content.Reader)
				require.Error(t, err)
				_ = content.Reader.Close()
			} else {
				assertReaderContent(t, reader, "/source", data)
			}
			require.NoError(t, reader.Close())
			require.Equal(t, before, snapshotReaderTree(t, root))
		})
	}
}
