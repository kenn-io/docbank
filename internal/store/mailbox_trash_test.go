package store

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTrashEmptyRetainsMailboxReceiptsAndDeletesUnrelatedRoots(t *testing.T) {
	for _, subtree := range []bool{false, true} {
		name := "separate source and attachment"
		if subtree {
			name = "folder subtree"
		}
		t.Run(name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			fixture := newEmailFixture(t, s, "seed.eml")
			source, err := s.ContentVersionByID(ctx, fixture.publication.ContentVersionID)
			require.NoError(t, err)
			folder, err := s.Mkdir(ctx, s.RootID(), "mail")
			require.NoError(t, err)
			require.NoError(t, s.RegisterMailboxArchive(ctx, MailboxArchive{
				ID: "synthetic-mailbox", Owner: "operator", Description: "Synthetic mailbox",
			}))
			ingest, err := s.BeginIngest(ctx, "mailbox", "Synthetic mailbox transfer")
			require.NoError(t, err)
			publication := MailboxTransferPublication{Owner: "operator", Run: ingest, Email: fixture.publication,
				Request: MailboxTransferRequest{ArchiveID: "synthetic-mailbox", Reference: "message-1",
					SHA256: source.BlobHash, Size: source.Size, Settings: "settings-v1",
					DestinationID: folder.ID, Name: "message.eml"}}
			receipt, err := s.PublishMailboxTransfer(ctx, publication)
			require.NoError(t, err)
			children, err := s.EmailDocumentPublication(ctx, receipt.DocumentPublicationID)
			require.NoError(t, err)
			require.Len(t, children.Relations, 1)
			child := children.Relations[0].Child
			require.NotNil(t, child)
			require.ErrorIs(t, s.RemoveEmailDocumentPublication(ctx, children.OperationID, children.RequestDigest), ErrMailboxConflict)
			_, err = s.PurgeDerivatives(ctx, PurgeRequest{ContentVersionIDs: []string{receipt.Target.VersionID}})
			require.ErrorIs(t, err, ErrEmailDocumentConflict)
			require.ErrorContains(t, err, "mailbox import receipt")
			assert.NotContains(t, err.Error(), "docbank email-documents release")
			retained := []int64{receipt.Target.NodeID, child.NodeID}
			if subtree {
				retained = []int64{folder.ID}
			}
			for _, id := range retained {
				_, _, err = s.Trash(ctx, id, UnconditionalRev)
				require.NoError(t, err)
			}
			unrelated, err := s.CreateFile(ctx, s.RootID(), "unrelated.txt", fakeHash("aa"), 1, "text/plain")
			require.NoError(t, err)
			_, _, err = s.Trash(ctx, unrelated.ID, UnconditionalRev)
			require.NoError(t, err)
			preview, err := s.TrashEmpty(ctx, 0, false)
			require.NoError(t, err)
			assert.Equal(t, int64(1), preview.Candidates)
			bounded, err := s.TrashEmptyBounded(ctx, 0, 1, false)
			require.NoError(t, err)
			require.Equal(t, preview, bounded)
			run, err := s.TrashEmpty(ctx, 0, true)
			require.NoError(t, err)
			require.Equal(t, int64(1), run.Deleted)
			_, err = s.NodeByID(ctx, unrelated.ID)
			require.ErrorIs(t, err, ErrNotFound)
			for _, id := range []int64{receipt.Target.NodeID, child.NodeID} {
				node, err := s.NodeByID(ctx, id)
				require.NoError(t, err)
				require.NotNil(t, node.TrashedAt)
			}
			_, err = s.PurgeDerivatives(ctx, PurgeRequest{All: true})
			require.NoError(t, err)
			unreachable, err := s.UnreachableBlobs(ctx)
			require.NoError(t, err)
			for _, blob := range unreachable {
				require.NotContains(t, []string{receipt.Target.SHA256, child.SHA256}, blob.Hash)
			}
			require.NoError(t, s.ValidateMetadata(ctx))
			var backup bytes.Buffer
			require.NoError(t, s.ExportMetadata(ctx, &backup))
			restored := newTestStore(t)
			require.NoError(t, restored.ImportMetadata(ctx, &backup))
			restoredReceipt, err := restored.MailboxTransfer(ctx, publication.Owner, publication.Request.ArchiveID, publication.Request.Reference)
			require.NoError(t, err)
			require.Equal(t, receipt, restoredReceipt)
			restoredChildren, err := restored.EmailDocumentPublication(ctx, children.OperationID)
			require.NoError(t, err)
			require.Equal(t, children, restoredChildren)
		})
	}
}
