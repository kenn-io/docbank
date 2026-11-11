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
)

func TestContentReader(t *testing.T) {
	t.Parallel()
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

func TestContentReaderPackedAndSidecars(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	v, err := docbank.New(t.Context(), docbank.Config{Root: root})
	require.NoError(t, err)
	_, err = v.Put(t.Context(), "/packed", strings.NewReader("packed synthetic data"), docbank.PutOptions{})
	require.NoError(t, err)
	_, err = v.Pack(t.Context(), docbank.PackOptions{})
	require.NoError(t, err)
	require.NoError(t, v.Close())
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		path := filepath.Join(root, "docbank.db") + suffix
		require.NoError(t, os.WriteFile(path, nil, 0o600))
		before := snapshotReaderTree(t, root)
		_, err = docbank.OpenContentReader(t.Context(), docbank.ContentReaderConfig{Root: root})
		require.ErrorIs(t, err, docbank.ErrVaultNeedsOwner)
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
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			hash = sha256.Sum256(data)
		}
		entries = append(entries, fmt.Sprintf("%s %s %d %d %x", relative, info.Mode(), info.Size(), info.ModTime().UnixNano(), hash))
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
		{"missing lock", func(t *testing.T, root string) { require.NoError(t, os.Remove(filepath.Join(root, "vault.lock"))) }},
		{"missing database", func(t *testing.T, root string) { require.NoError(t, os.Remove(filepath.Join(root, "docbank.db"))) }},
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
