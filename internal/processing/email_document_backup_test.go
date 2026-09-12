package processing

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/maintenance"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/backup"
	"io"
	"path/filepath"
	"testing"
)

func TestEmailDocumentsRealBackupRestoreAndRetry(t *testing.T) {
	f := newEmailPipelineFixture(t)
	target := f.add(t, "mail.eml", attachmentCSVSource, "message/rfc822")
	view, err := EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, target)
	require.NoError(t, err)
	request := pipelineDocumentRequest(t, f, view, "backup-retry")
	receipt, err := PublishEmailDocuments(t.Context(), f.catalog, f.blobs, request)
	require.NoError(t, err)
	repository, err := backup.Init(filepath.Join(t.TempDir(), "repository"))
	require.NoError(t, err)
	_, err = backupapp.Create(t.Context(), repository, "email-documents", f.catalog, f.blobs, backup.CreateOptions{Jobs: 2})
	require.NoError(t, err)
	proof, err := backup.Verify(t.Context(), repository, backupapp.New("email-documents"), backup.VerifyOptions{Jobs: 2})
	require.NoError(t, err)
	require.Empty(t, proof.Problems)
	destination := filepath.Join(t.TempDir(), "restored")
	_, err = backupapp.Restore(t.Context(), repository, "email-documents", backup.RestoreOptions{TargetDir: destination, Jobs: 2})
	require.NoError(t, err)
	restored, err := store.OpenForRestore(filepath.Join(destination, "docbank.db"), store.DefaultSQLiteDriver())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	bs, err := blob.New(store.NewPackCatalog(restored), filepath.Join(destination, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bs.Close()) })
	again, err := PublishEmailDocuments(t.Context(), restored, bs, request)
	require.NoError(t, err)
	require.Equal(t, receipt, again)
	page, err := restored.EmailDocumentRelations(t.Context(), document.EmailDocumentRelationQuery{ChildVersionID: receipt.Relations[0].Child.VersionID})
	require.NoError(t, err)
	require.Equal(t, receipt.Relations[0], page.Items[0].Relation)
	_, err = maintenance.GarbageCollect(t.Context(), restored, bs, maintenance.GCOptions{})
	require.NoError(t, err)
	for _, identity := range []document.EmailDocumentIdentity{request.Parent, *receipt.Relations[0].Child} {
		stream, size, err := bs.OpenStreamContext(t.Context(), identity.SHA256)
		require.NoError(t, err)
		require.Equal(t, identity.Size, size)
		n, err := io.Copy(io.Discard, stream)
		require.NoError(t, err)
		require.NoError(t, stream.Close())
		require.Equal(t, size, n)
	}
	require.NoError(t, restored.VerifyRenditionBlobBytes(t.Context(), bs))
	require.NoError(t, restored.ValidateMetadata(t.Context()))
	f.emptySpool(t)
}
