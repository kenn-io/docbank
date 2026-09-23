package processing_test

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/backup"
)

func TestPackageSuppliedTextSearchSurvivesBackupRestore(t *testing.T) {
	root := t.TempDir()
	catalog, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })

	source := createPackageTestFile(t, catalog, blobs, "source.pdf", "application/pdf", []byte("synthetic source"))
	senderText := createPackageTestFile(t, catalog, blobs, "sender.txt", "text/plain", []byte("quenchwood sender text"))
	_, err = processing.PublishPackageSuppliedText(t.Context(), catalog, blobs,
		uuid.NewString(), "row-1", source, senderText, "utf-8")
	require.NoError(t, err)

	repository, err := backup.Init(filepath.Join(t.TempDir(), "backup"))
	require.NoError(t, err)
	_, err = backupapp.Create(t.Context(), repository, "package-supplied-text", catalog, blobs, backup.CreateOptions{Jobs: 1})
	require.NoError(t, err)
	target := filepath.Join(t.TempDir(), "restored")
	_, err = backupapp.Restore(t.Context(), repository, "package-supplied-text", backup.RestoreOptions{TargetDir: target, Jobs: 1})
	require.NoError(t, err)
	restored, err := store.OpenForRestore(filepath.Join(target, "docbank.db"), store.DefaultSQLiteDriver())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	hits, _, err := restored.SearchPage(t.Context(), "quenchwood", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, source.ID, hits[0].Node.CurrentVersionID)
}

func createPackageTestFile(t *testing.T, catalog *store.Store, blobs *blob.Store, name, mediaType string, content []byte) store.ContentVersion {
	t.Helper()
	receipt, err := blobs.WriteDetailedContext(t.Context(), bytes.NewReader(content))
	require.NoError(t, err)
	encoding, err := receipt.EncodingName()
	require.NoError(t, err)
	node, err := catalog.CreateFileWithReceipt(t.Context(), catalog.RootID(), name,
		receipt.Hash, receipt.Size, mediaType, store.BlobPhysical{
			Encoding: encoding, StoredBytes: receipt.StoredSize, PackEligible: receipt.PackEligible,
			MD5: receipt.MD5, Created: receipt.Created,
		})
	require.NoError(t, err)
	return node.Version
}
