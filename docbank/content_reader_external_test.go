package docbank_test

import (
	"crypto/sha256"
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
	"go.kenn.io/kit/packstore"
)

func TestContentReader(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "missing")
	for range 2 {
		_, err := docbank.OpenContentReader(t.Context(), docbank.ContentReaderConfig{Root: missing})
		require.ErrorIs(t, err, docbank.ErrVaultNeedsOwner)
		_, err = os.Lstat(missing)
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	root := t.TempDir()
	_, err := docbank.OpenContentReader(t.Context(), docbank.ContentReaderConfig{Root: root})
	require.ErrorIs(t, err, docbank.ErrVaultNeedsOwner)
	_, err = os.Stat(filepath.Join(root, "vault.lock"))
	require.ErrorIs(t, err, os.ErrNotExist)
	v, err := docbank.New(t.Context(), docbank.Config{Root: root})
	require.NoError(t, err)
	_, err = v.Put(t.Context(), "/sessions/x.jsonl", strings.NewReader("synthetic original"), docbank.PutOptions{})
	require.NoError(t, err)
	id := v.ID()
	_, err = docbank.OpenContentReader(t.Context(), docbank.ContentReaderConfig{Root: root})
	require.ErrorIs(t, err, docbank.ErrVaultLocked)
	require.NoError(t, v.Close())
	stabilizeReaderTree(t, root)
	before := snapshotReaderTree(t, root)
	reader, err := docbank.OpenContentReader(t.Context(), docbank.ContentReaderConfig{Root: root})
	require.NoError(t, err)
	require.Equal(t, id, reader.ID())
	_, err = docbank.New(t.Context(), docbank.Config{Root: root})
	require.ErrorIs(t, err, docbank.ErrVaultLocked)
	assertReaderContent(t, reader, "/sessions/x.jsonl", "synthetic original")
	_, err = reader.OpenContent(t.Context(), "/missing")
	require.ErrorIs(t, err, docbank.ErrNotFound)
	_, err = reader.OpenContent(t.Context(), "/sessions")
	require.ErrorIs(t, err, docbank.ErrNotFile)
	require.NoError(t, reader.Close())
	require.NoError(t, reader.Close())
	_, err = reader.OpenContent(t.Context(), "/sessions/x.jsonl")
	require.ErrorIs(t, err, docbank.ErrClosed)
	require.Equal(t, before, snapshotReaderTree(t, root))
}

func TestContentReaderPackedAndSidecars(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	v, err := docbank.New(t.Context(), docbank.Config{Root: root})
	require.NoError(t, err)
	receipt, err := v.Put(t.Context(), "/packed", strings.NewReader("packed synthetic data"), docbank.PutOptions{})
	require.NoError(t, err)
	report, err := v.Pack(t.Context(), docbank.PackOptions{})
	require.NoError(t, err)
	require.GreaterOrEqual(t, report.BlobsPacked, 1)
	require.NoFileExists(t, looseBlobPath(root, receipt.Computed.SHA256))
	require.NoError(t, v.Close())
	stabilizeReaderTree(t, root)
	before := snapshotReaderTree(t, root)
	reader, err := docbank.OpenContentReader(t.Context(), docbank.ContentReaderConfig{Root: root})
	require.NoError(t, err)
	assertReaderContent(t, reader, "/packed", "packed synthetic data")
	require.NoError(t, reader.Close())
	require.Equal(t, before, snapshotReaderTree(t, root))
}

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
	for range 2 {
		_, err := docbank.OpenContentReader(t.Context(), docbank.ContentReaderConfig{Root: root})
		check(err)
		require.Equal(t, before, snapshotReaderTree(t, root))
	}
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
		fenced      bool
	}
	cases := []refusalCase{
		{"missing marker", func(t *testing.T, root string) {
			t.Helper()
			require.NoError(t, os.Remove(filepath.Join(root, "blobs", ".packstore-owner.json")))
		}, true, "missing primary storage", false},
		{"missing blobs", func(t *testing.T, root string) {
			t.Helper()
			require.NoError(t, os.RemoveAll(filepath.Join(root, "blobs")))
		}, true, "missing primary storage", false},
		{"missing lock", func(t *testing.T, root string) {
			t.Helper()
			require.NoError(t, os.Remove(filepath.Join(root, "vault.lock")))
		}, true, "", false},
		{"missing database", func(t *testing.T, root string) {
			t.Helper()
			require.NoError(t, os.Remove(filepath.Join(root, "docbank.db")))
		}, true, "", false},
		{"empty database", func(t *testing.T, root string) {
			t.Helper()
			require.NoError(t, os.WriteFile(filepath.Join(root, "docbank.db"), nil, 0o600))
		}, true, "", false},
		{"sidecar", func(t *testing.T, root string) {
			t.Helper()
			require.NoError(t, os.WriteFile(filepath.Join(root, "docbank.db-wal"), nil, 0o600))
		}, true, "sidecar", false},
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
		}, true, "pending restore handoff", false},
		{"wrong marker", func(t *testing.T, root string) {
			t.Helper()
			backend, err := blob.NewFilesystemBackend(filepath.Join(root, "blobs"), nil)
			require.NoError(t, err)
			prior, err := backend.Ownership(t.Context())
			require.NoError(t, err)
			wrong := prior
			wrong.Epoch = "50000000-0000-4000-8000-000000000001"
			require.NoError(t, backend.ReplaceOwnership(t.Context(), wrong, &prior))
			require.NoError(t, backend.Close())
		}, false, "", true},
		{"newer schema", func(t *testing.T, root string) {
			t.Helper()
			db, err := store.DefaultSQLiteDriver().Open(filepath.Join(root, "docbank.db"), docsqlite.OpenOptions{Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Deferred})
			require.NoError(t, err)
			_, err = db.ExecContext(t.Context(), "UPDATE vault_metadata SET schema_version = 999999")
			require.NoError(t, err)
			require.NoError(t, db.Close())
		}, false, "newer than binary", false},
		{"released schema", func(t *testing.T, root string) {
			t.Helper()
			path := filepath.Join(root, "docbank.db")
			require.NoError(t, os.Remove(path))
			schema, err := os.ReadFile(filepath.Join("..", "internal", "store", "testdata", "schema-v0.15.0.sql"))
			require.NoError(t, err)
			db, err := store.DefaultSQLiteDriver().Open(path, docsqlite.OpenOptions{Access: docsqlite.Create, TransactionMode: docsqlite.Immediate})
			require.NoError(t, err)
			_, err = db.ExecContext(t.Context(), string(schema))
			require.NoError(t, err)
			_, err = db.ExecContext(t.Context(), `INSERT INTO vault_metadata(singleton,vault_uid,schema_version) VALUES(1,'10000000-0000-4000-8000-000000000028',28)`)
			require.NoError(t, err)
			require.NoError(t, db.Close())
		}, true, "database requires initialization or upgrade", false},
	}
	for _, name := range []string{"database directory", "database symlink", "handoff directory", "handoff symlink"} {
		cases = append(cases, refusalCase{name, func(t *testing.T, root string) {
			t.Helper()
			path := filepath.Join(root, "docbank.db")
			if strings.HasPrefix(name, "handoff") {
				path = filepath.Join(root, "blobs", "tmp", "primary-restore-handoff.json")
			} else {
				require.NoError(t, os.Remove(path))
			}
			if strings.HasSuffix(name, "directory") {
				require.NoError(t, os.Mkdir(path, 0o700))
			} else if err := os.Symlink(filepath.Join(t.TempDir(), "target"), path); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
		}, false, "not a regular file", false})
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
				if tc.fenced {
					require.ErrorIs(t, err, packstore.ErrStoreFenced)
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

func TestContentReaderTempInsideVault(t *testing.T) {
	root := t.TempDir()
	registry := t.TempDir()
	t.Setenv("DOCBANK_LOCK_DIR", registry)
	v, err := docbank.New(t.Context(), docbank.Config{Root: root})
	require.NoError(t, err)
	_, err = v.Put(t.Context(), "/source", strings.NewReader("synthetic content"), docbank.PutOptions{})
	require.NoError(t, err)
	require.NoError(t, v.Close())
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		_, err = os.Lstat(filepath.Join(root, "docbank.db") + suffix)
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	tmp := filepath.Join(root, "tmp")
	require.NoError(t, os.Mkdir(tmp, 0o700))
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, tmp)
	}
	stabilizeReaderTree(t, root)
	before := snapshotReaderTree(t, root)
	reader, err := docbank.OpenContentReader(t.Context(), docbank.ContentReaderConfig{Root: root})
	require.NoError(t, err)
	assertReaderContent(t, reader, "/source", "synthetic content")
	require.NoError(t, reader.Close())
	require.Equal(t, before, snapshotReaderTree(t, root))
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
	for _, dangling := range []bool{false, true} {
		t.Run(fmt.Sprintf("dangling_%t", dangling), func(t *testing.T) {
			registry := t.TempDir()
			t.Setenv("DOCBANK_LOCK_DIR", registry)
			root := t.TempDir()
			vault, err := docbank.New(t.Context(), docbank.Config{Root: root})
			require.NoError(t, err)
			require.NoError(t, vault.Close())
			path := filepath.Join(root, "missing", "locks")
			if dangling {
				path = filepath.Join(t.TempDir(), "dangling")
				if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), path); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			t.Setenv("DOCBANK_LOCK_DIR", path)
			assertReaderRefusedUnchanged(t, root, func(err error) {
				t.Helper()
				require.Error(t, err)
				require.NotErrorIs(t, err, docbank.ErrVaultNeedsOwner)
				if !dangling {
					require.ErrorContains(t, err, "DOCBANK_LOCK_DIR")
				}
			})
			t.Setenv("DOCBANK_LOCK_DIR", registry)
			lock, err := (home.Layout{Root: root}).TryLockExclusive()
			require.NoError(t, err)
			require.NoError(t, lock.Release())
		})
	}
}

func TestContentReaderCompressedLoose(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	vault, err := docbank.New(t.Context(), docbank.Config{Root: root, LooseCompression: docbank.LooseCompressionOptions{Enabled: true, MinBytes: 1, MinSavingsPercent: 0}})
	require.NoError(t, err)
	data := strings.Repeat("synthetic compressed data ", 256)
	receipt, err := vault.Put(t.Context(), "/compressed", strings.NewReader(data), docbank.PutOptions{})
	require.NoError(t, err)
	require.Equal(t, "zstd", receipt.Physical.Encoding)
	require.NoError(t, vault.Close())
	stabilizeReaderTree(t, root)
	before := snapshotReaderTree(t, root)
	reader, err := docbank.OpenContentReader(t.Context(), docbank.ContentReaderConfig{Root: root})
	require.NoError(t, err)
	assertReaderContent(t, reader, "/compressed", data)
	require.NoError(t, reader.Close())
	require.Equal(t, before, snapshotReaderTree(t, root))
}

func TestContentReaderTamperedAndTrashed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	vault, err := docbank.New(t.Context(), docbank.Config{Root: root})
	require.NoError(t, err)
	receipt, err := vault.Put(t.Context(), "/tampered", strings.NewReader("synthetic original"), docbank.PutOptions{})
	require.NoError(t, err)
	_, err = vault.Put(t.Context(), "/trashed", strings.NewReader("synthetic trash"), docbank.PutOptions{})
	require.NoError(t, err)
	_, err = vault.TrashPath(t.Context(), "/trashed", docbank.RevisionOptions{})
	require.NoError(t, err)
	require.NoError(t, vault.Close())
	path := looseBlobPath(root, receipt.Computed.SHA256)
	damage, err := os.ReadFile(path)
	require.NoError(t, err)
	damage[0] ^= 1
	require.NoError(t, os.WriteFile(path, damage, 0o600))
	stabilizeReaderTree(t, root)
	before := snapshotReaderTree(t, root)
	reader, err := docbank.OpenContentReader(t.Context(), docbank.ContentReaderConfig{Root: root})
	require.NoError(t, err)
	_, err = reader.OpenContent(t.Context(), "/trashed")
	require.ErrorIs(t, err, docbank.ErrNotFound)
	content, err := reader.OpenContent(t.Context(), "/tampered")
	require.NoError(t, err)
	_, err = io.ReadAll(content.Reader)
	require.ErrorIs(t, err, packstore.ErrPhysicalCorrupt)
	require.ErrorIs(t, content.Reader.Verify(), packstore.ErrPhysicalCorrupt)
	require.ErrorIs(t, content.Reader.Close(), packstore.ErrPhysicalCorrupt)
	require.NoError(t, reader.Close())
	require.Equal(t, before, snapshotReaderTree(t, root))
}
