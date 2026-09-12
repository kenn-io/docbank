package store

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// Retained parent/child identities must survive, but must not starve an older-
// first bounded deletion batch or roll back deletion of unrelated trash.
func TestMailboxRetainedTrashAllowsBoundedProgress(t *testing.T) {
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
