package store

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTrashEmptyRetainsMailboxReceiptsAndDeletesUnrelatedRoots(t *testing.T) {
	t.Parallel()
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

// Retained parent/child identities must survive, but must not starve an older-
// first bounded deletion batch or roll back deletion of unrelated trash.
func TestMailboxRetainedTrashAllowsBoundedProgress(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"parent", "child", "directory"} {
		t.Run(target, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			f := newEmailFixture(t, s, "seed.eml")
			dir, err := s.Mkdir(ctx, s.RootID(), "imported")
			require.NoError(t, err)
			require.NoError(t, s.RegisterMailboxArchive(ctx, MailboxArchive{ID: "synthetic-trash", Owner: "owner", Description: "Synthetic transfer"}))
			run, err := s.BeginIngest(ctx, "mailbox", "Synthetic transfer")
			require.NoError(t, err)
			source, err := emailVersion(ctx, s.db, f.publication.ContentVersionID)
			require.NoError(t, err)
			publication := MailboxTransferPublication{Owner: "owner", Run: run, Request: MailboxTransferRequest{ArchiveID: "synthetic-trash", Reference: "message-1", SHA256: source.BlobHash, Size: source.Size, Settings: "settings-v1", DestinationID: dir.ID, Name: "message.eml"}, Email: f.publication}
			receipt, err := s.PublishMailboxTransfer(ctx, publication)
			require.NoError(t, err)
			attachments, err := s.EmailDocumentPublication(ctx, receipt.DocumentPublicationID)
			require.NoError(t, err)
			require.Len(t, attachments.Relations, 1)
			require.NotNil(t, attachments.Relations[0].Child)
			retainedID := receipt.Target.NodeID
			switch target {
			case "child":
				retainedID = attachments.Relations[0].Child.NodeID
			case "directory":
				retainedID = dir.ID
			}
			_, _, err = s.Trash(ctx, retainedID, UnconditionalRev)
			require.NoError(t, err)
			var disposable []int64
			for _, name := range []string{"discard-one", "discard-two"} {
				node, err := s.CreateFile(ctx, s.RootID(), name, fakeHash("ab"), 1, "text/plain")
				require.NoError(t, err)
				_, _, err = s.Trash(ctx, node.ID, UnconditionalRev)
				require.NoError(t, err)
				disposable = append(disposable, node.ID)
			}
			dry, err := s.TrashEmptyBounded(ctx, 0, 1, false)
			require.NoError(t, err)
			require.EqualValues(t, 1, dry.Candidates)
			require.Zero(t, dry.Deleted)
			require.EqualValues(t, 1, dry.Retained)
			require.True(t, dry.More)
			for i, id := range disposable {
				result, err := s.TrashEmptyBounded(ctx, 0, 1, true)
				require.NoError(t, err)
				require.EqualValues(t, 1, result.Deleted)
				require.EqualValues(t, 1, result.Retained)
				require.Equal(t, i == 0, result.More)
				_, err = s.NodeByID(ctx, id)
				require.ErrorIs(t, err, ErrNotFound)
			}
			result, err := s.TrashEmpty(ctx, 0, true)
			require.NoError(t, err)
			require.Zero(t, result.Candidates)
			require.Zero(t, result.Deleted)
			require.EqualValues(t, 1, result.Retained)
			require.False(t, result.More)
			_, err = s.NodeByID(ctx, retainedID)
			require.NoError(t, err)
			again, err := s.PublishMailboxTransfer(ctx, publication)
			require.NoError(t, err)
			if target != "child" {
				receipt.Outcome = "tombstone"
			}
			require.Equal(t, receipt, again)
			require.NoError(t, s.ValidateMetadata(ctx))
			var metadata bytes.Buffer
			require.NoError(t, s.ExportMetadata(ctx, &metadata))
			restored := newTestStore(t)
			require.NoError(t, restored.ImportMetadata(ctx, &metadata))
			require.NoError(t, restored.ValidateMetadata(ctx))
		})
	}
}
