package processing_test

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/maintenance"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/backup"
)

func TestEmailDocumentsRealBackupRestoreAndRetry(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	catalog, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	spool := filepath.Join(root, "spool")
	require.NoError(t, os.Mkdir(spool, 0700))
	var source strings.Builder
	source.WriteString("Content-Type: multipart/mixed; boundary=m\r\n\r\n")
	const attachments = 64
	for i := range attachments {
		fmt.Fprintf(&source, "--m\r\nContent-Type: text/csv\r\n"+
			"Content-Disposition: attachment; filename=table-%d.csv\r\n\r\nitem,count\r\nsynthetic,%d\r\n", i, i)
	}
	source.WriteString("--m--\r\n")
	written, err := blobs.WriteDetailedContext(t.Context(), strings.NewReader(source.String()))
	require.NoError(t, err)
	encoding, err := written.EncodingName()
	require.NoError(t, err)
	file, err := catalog.CreateFileWithReceipt(t.Context(), catalog.RootID(), "mail.eml",
		written.Hash, written.Size, "message/rfc822", store.BlobPhysical{
			Encoding: encoding, StoredBytes: written.StoredSize, PackEligible: written.PackEligible,
			MD5: written.MD5, Created: written.Created,
		})
	require.NoError(t, err)
	view, err := processing.EnsureEmailTarget(t.Context(), catalog, blobs, spool, store.EmailTarget{Version: file.Version})
	require.NoError(t, err)
	destinationNode, err := catalog.NodeByID(t.Context(), catalog.RootID())
	require.NoError(t, err)
	request := document.EmailDocumentPublicationRequest{
		OperationID: "backup-retry", GenerationID: view.Generation.ID, AttachmentID: view.Attachment.ID,
		Parent: document.EmailDocumentIdentity{NodeID: view.Version.NodeID, VersionID: view.Version.ID,
			SHA256: view.Version.BlobHash, Size: view.Version.Size},
		DestinationID: destinationNode.ID, DestinationRevision: destinationNode.Revision,
	}
	receipt, err := processing.PublishEmailDocuments(t.Context(), catalog, blobs, request)
	require.NoError(t, err)
	require.Len(t, receipt.Relations, attachments)
	repository, err := backup.Init(filepath.Join(t.TempDir(), "repository"))
	require.NoError(t, err)
	_, err = backupapp.Create(t.Context(), repository, "email-documents", catalog, blobs, backup.CreateOptions{Jobs: 2})
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
	again, err := processing.PublishEmailDocuments(t.Context(), restored, bs, request)
	require.NoError(t, err)
	require.Equal(t, receipt, again)
	page, err := restored.EmailDocumentRelations(t.Context(), document.EmailDocumentRelationQuery{ChildVersionID: receipt.Relations[0].Child.VersionID})
	require.NoError(t, err)
	require.Equal(t, receipt.Relations[0], page.Items[0].Relation)
	page, err = restored.EmailDocumentRelations(t.Context(), document.EmailDocumentRelationQuery{
		ParentVersionID: request.Parent.VersionID,
	})
	require.NoError(t, err)
	require.Len(t, page.Items, attachments)
	for i, item := range page.Items {
		require.Equal(t, receipt.Relations[i], item.Relation)
	}
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
	entries, err := os.ReadDir(spool)
	require.NoError(t, err)
	require.Empty(t, entries)
}
