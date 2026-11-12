package docbank_test

import (
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/synctest"

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
	before := snapshotReaderTree(t, root)
	reader, err := docbank.OpenContentReader(t.Context(), docbank.ContentReaderConfig{Root: root})
	require.NoError(t, err)
	require.Equal(t, id, reader.ID())
	_, err = docbank.New(t.Context(), docbank.Config{Root: root})
	require.ErrorIs(t, err, docbank.ErrVaultLocked)
	content, err := reader.OpenContent(t.Context(), "/sessions/x.jsonl")
	require.NoError(t, err)
	data, err := io.ReadAll(content.Reader)
	require.NoError(t, err)
	require.Equal(t, "synthetic original", string(data))
	require.NoError(t, content.Reader.Verify())
	require.NoError(t, content.Reader.Close())
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

func TestContentReaderRefusesInvalidIdentityAndSchema(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, query, message string }{
		{"newer schema", "UPDATE vault_metadata SET schema_version = 999999", "newer than binary"},
		{"development schema", "UPDATE vault_metadata SET schema_version = 4", "development schema"},
		{"invalid identity", "UPDATE vault_metadata SET vault_uid = 'invalid'", "vault identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			vault, err := docbank.New(t.Context(), docbank.Config{Root: root})
			require.NoError(t, err)
			require.NoError(t, vault.Close())
			db, err := store.DefaultSQLiteDriver().Open(filepath.Join(root, "docbank.db"), docsqlite.OpenOptions{Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Deferred})
			require.NoError(t, err)
			_, err = db.ExecContext(t.Context(), tc.query)
			require.NoError(t, err)
			require.NoError(t, db.Close())
			before := snapshotReaderTree(t, root)
			for range 2 {
				_, err = docbank.OpenContentReader(t.Context(), docbank.ContentReaderConfig{Root: root})
				require.ErrorContains(t, err, tc.message)
				require.NotErrorIs(t, err, docbank.ErrVaultNeedsOwner)
				lock, err := (home.Layout{Root: root}).TryLockExclusive()
				require.NoError(t, err)
				require.NoError(t, lock.Release())
				require.Equal(t, before, snapshotReaderTree(t, root))
			}
		})
	}
}

func TestContentReaderRefusesWrongOwnership(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	vault, err := docbank.New(t.Context(), docbank.Config{Root: root})
	require.NoError(t, err)
	require.NoError(t, vault.Close())
	backend, err := blob.NewFilesystemBackend(filepath.Join(root, "blobs"), nil)
	require.NoError(t, err)
	prior, err := backend.Ownership(t.Context())
	require.NoError(t, err)
	wrong := prior
	wrong.Epoch = "50000000-0000-4000-8000-000000000001"
	require.NoError(t, backend.ReplaceOwnership(t.Context(), wrong, &prior))
	require.NoError(t, backend.Close())
	before := snapshotReaderTree(t, root)
	for range 2 {
		_, err = docbank.OpenContentReader(t.Context(), docbank.ContentReaderConfig{Root: root})
		require.ErrorIs(t, err, packstore.ErrStoreFenced)
		require.NotErrorIs(t, err, docbank.ErrVaultNeedsOwner)
		lock, err := (home.Layout{Root: root}).TryLockExclusive()
		require.NoError(t, err)
		require.NoError(t, lock.Release())
		require.Equal(t, before, snapshotReaderTree(t, root))
	}
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
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		path := filepath.Join(root, "docbank.db") + suffix
		require.NoError(t, os.WriteFile(path, nil, 0o600))
		before := snapshotReaderTree(t, root)
		_, err = docbank.OpenContentReader(t.Context(), docbank.ContentReaderConfig{Root: root})
		require.ErrorIs(t, err, docbank.ErrVaultNeedsOwner)
		require.Equal(t, before, snapshotReaderTree(t, root))
		_, err = docbank.OpenContentReader(t.Context(), docbank.ContentReaderConfig{Root: root})
		require.ErrorIs(t, err, docbank.ErrVaultNeedsOwner)
		lock, err := (home.Layout{Root: root}).TryLockExclusive()
		require.NoError(t, err)
		require.NoError(t, lock.Release())
		require.Equal(t, before, snapshotReaderTree(t, root))
		require.NoError(t, os.Remove(path))
	}
	before := snapshotReaderTree(t, root)
	reader, err := docbank.OpenContentReader(t.Context(), docbank.ContentReaderConfig{Root: root})
	require.NoError(t, err)
	content, err := reader.OpenContent(t.Context(), "/packed")
	require.NoError(t, err)
	data, err := io.ReadAll(content.Reader)
	require.NoError(t, err)
	require.Equal(t, "packed synthetic data", string(data))
	require.NoError(t, content.Reader.Verify())
	require.NoError(t, content.Reader.Close())
	require.NoError(t, reader.Close())
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

func TestContentReaderRefusesRecoveryWithoutChanges(t *testing.T) {
	t.Parallel()
	for _, change := range []struct {
		name  string
		apply func(*testing.T, string)
	}{
		{"missing marker", func(t *testing.T, root string) {
			t.Helper()
			require.NoError(t, os.Remove(filepath.Join(root, "blobs", ".packstore-owner.json")))
		}},
		{"missing lock", func(t *testing.T, root string) {
			t.Helper()
			require.NoError(t, os.Remove(filepath.Join(root, "vault.lock")))
		}},
		{"missing database", func(t *testing.T, root string) {
			t.Helper()
			require.NoError(t, os.Remove(filepath.Join(root, "docbank.db")))
		}},
		{"empty database", func(t *testing.T, root string) {
			t.Helper()
			require.NoError(t, os.WriteFile(filepath.Join(root, "docbank.db"), nil, 0o600))
		}},
		{"restore handoff", func(t *testing.T, root string) {
			t.Helper()
			require.NoError(t, os.WriteFile(filepath.Join(root, "blobs", "tmp", "primary-restore-handoff.json"), []byte("pending"), 0o600))
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			root := t.TempDir()
			v, err := docbank.New(t.Context(), docbank.Config{Root: root})
			require.NoError(t, err)
			require.NoError(t, v.Close())
			change.apply(t, root)
			before := snapshotReaderTree(t, root)
			_, err = docbank.OpenContentReader(t.Context(), docbank.ContentReaderConfig{Root: root})
			require.ErrorIs(t, err, docbank.ErrVaultNeedsOwner)
			require.Equal(t, before, snapshotReaderTree(t, root))
			_, err = docbank.OpenContentReader(t.Context(), docbank.ContentReaderConfig{Root: root})
			require.ErrorIs(t, err, docbank.ErrVaultNeedsOwner)
			require.Equal(t, before, snapshotReaderTree(t, root))
			lock, err := (home.Layout{Root: root}).TryLockExclusive()
			require.NoError(t, err)
			require.NoError(t, lock.Release())
		})
	}
}

func TestContentReaderRefusesNonRegularPaths(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"database directory", "database symlink", "handoff directory", "handoff symlink"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			vault, err := docbank.New(t.Context(), docbank.Config{Root: root})
			require.NoError(t, err)
			require.NoError(t, vault.Close())
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
			before := snapshotReaderTree(t, root)
			for range 2 {
				_, err = docbank.OpenContentReader(t.Context(), docbank.ContentReaderConfig{Root: root})
				require.Error(t, err)
				require.NotErrorIs(t, err, docbank.ErrVaultNeedsOwner)
				require.Equal(t, before, snapshotReaderTree(t, root))
				lock, err := (home.Layout{Root: root}).TryLockExclusive()
				require.NoError(t, err)
				require.NoError(t, lock.Release())
			}
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
	before := snapshotReaderTree(t, root)
	reader, err := docbank.OpenContentReader(t.Context(), docbank.ContentReaderConfig{Root: root})
	require.NoError(t, err)
	content, err := reader.OpenContent(t.Context(), "/source")
	require.NoError(t, err)
	data, err := io.ReadAll(content.Reader)
	require.NoError(t, err)
	require.Equal(t, "synthetic content", string(data))
	require.NoError(t, content.Reader.Verify())
	require.NoError(t, content.Reader.Close())
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
	for _, present := range []bool{false, true} {
		for _, name := range []string{"root", "child", "missing descendant", "root alias", "alias descendant"} {
			t.Run(fmt.Sprintf("%s_leaf_%t", name, present), func(t *testing.T) {
				registry := t.TempDir()
				t.Setenv("DOCBANK_LOCK_DIR", registry)
				root := t.TempDir()
				vault, err := docbank.New(t.Context(), docbank.Config{Root: root})
				require.NoError(t, err)
				require.NoError(t, vault.Close())
				if !present {
					require.NoError(t, os.Remove(filepath.Join(root, "vault.lock")))
				}
				alias := filepath.Join(t.TempDir(), "alias")
				if strings.HasPrefix(name, "root alias") || strings.HasPrefix(name, "alias ") {
					if err := os.Symlink(root, alias); err != nil {
						t.Skipf("symlinks unavailable: %v", err)
					}
				}
				path := map[string]string{
					"root": root, "child": filepath.Join(root, "locks"),
					"missing descendant": filepath.Join(root, "missing", "locks"),
					"root alias":         alias, "alias descendant": filepath.Join(alias, "locks"),
				}[name]
				t.Setenv("DOCBANK_LOCK_DIR", path)
				before := snapshotReaderTree(t, root)
				for range 2 {
					_, err = docbank.OpenContentReader(t.Context(), docbank.ContentReaderConfig{Root: root})
					require.ErrorContains(t, err, "DOCBANK_LOCK_DIR")
					require.NotErrorIs(t, err, docbank.ErrVaultNeedsOwner)
					require.Equal(t, before, snapshotReaderTree(t, root))
				}
				t.Setenv("DOCBANK_LOCK_DIR", registry)
				lock, err := (home.Layout{Root: root}).TryLockExclusive()
				require.NoError(t, err)
				require.NoError(t, lock.Release())
			})
		}
	}
}

func TestContentReaderRefusesReleasedSchema(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	vault, err := docbank.New(t.Context(), docbank.Config{Root: root})
	require.NoError(t, err)
	id := vault.ID()
	require.NoError(t, vault.Close())
	path := filepath.Join(root, "docbank.db")
	require.NoError(t, os.Remove(path))
	schema, err := os.ReadFile(filepath.Join("..", "internal", "store", "testdata", "schema-v0.15.0.sql"))
	require.NoError(t, err)
	db, err := store.DefaultSQLiteDriver().Open(path, docsqlite.OpenOptions{Access: docsqlite.Create, TransactionMode: docsqlite.Immediate})
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), string(schema))
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO vault_metadata(singleton,vault_uid,schema_version) VALUES(1,?,28)`, id)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO nodes(id,parent_id,name,kind,revision,created_at,modified_at)
		VALUES(1,NULL,'','dir',1,'2026-08-16T12:00:00.000000000Z','2026-08-16T12:00:00.000000000Z')`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		_, err = os.Lstat(path + suffix)
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	before := snapshotReaderTree(t, root)
	for range 2 {
		_, err = docbank.OpenContentReader(t.Context(), docbank.ContentReaderConfig{Root: root})
		require.ErrorIs(t, err, docbank.ErrVaultNeedsOwner)
		require.ErrorContains(t, err, "database requires initialization or upgrade")
		lock, err := (home.Layout{Root: root}).TryLockExclusive()
		require.NoError(t, err)
		require.NoError(t, lock.Release())
		require.Equal(t, before, snapshotReaderTree(t, root))
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
	before := snapshotReaderTree(t, root)
	reader, err := docbank.OpenContentReader(t.Context(), docbank.ContentReaderConfig{Root: root})
	require.NoError(t, err)
	content, err := reader.OpenContent(t.Context(), "/compressed")
	require.NoError(t, err)
	got, err := io.ReadAll(content.Reader)
	require.NoError(t, err)
	require.Equal(t, data, string(got))
	require.NoError(t, content.Reader.Verify())
	require.NoError(t, content.Reader.Close())
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
	before := snapshotReaderTree(t, root)
	reader, err := docbank.OpenContentReader(t.Context(), docbank.ContentReaderConfig{Root: root})
	require.NoError(t, err)
	_, err = reader.OpenContent(t.Context(), "/trashed")
	require.ErrorIs(t, err, docbank.ErrNotFound)
	content, err := reader.OpenContent(t.Context(), "/tampered")
	require.NoError(t, err)
	_, err = io.ReadAll(content.Reader)
	require.Error(t, err)
	require.Error(t, content.Reader.Verify())
	require.NoError(t, content.Reader.Close())
	require.NoError(t, reader.Close())
	require.Equal(t, before, snapshotReaderTree(t, root))
}
