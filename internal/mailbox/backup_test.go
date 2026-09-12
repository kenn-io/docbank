package mailbox

import (
	"bytes"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/maintenance"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/backup"
	"io"
	"path/filepath"
	"testing"
)

func TestMailboxPortableBackupRetainsSourceChildrenAndTombstone(t *testing.T) {
	f := mailboxFixture(t)
	ctx := t.Context()
	raw := []byte(separator + "Subject: Synthetic attachment\nContent-Type: multipart/mixed; boundary=m\n\n--m\nContent-Type: text/plain\n\nSynthetic body.\n--m\nContent-Type: text/csv\nContent-Disposition: attachment; filename=table.csv\n\nname,count\nsynthetic,2\n--m--\n")
	c := store.MailboxContainerRequest{ID: "backup-source", Owner: "one", SHA256: hashBytes(raw), Size: int64(len(raw)), Format: "mbox"}
	_, err := f.Store.BeginMailboxContainer(ctx, c)
	require.NoError(t, err)
	require.NoError(t, f.UploadChunk(ctx, c.Owner, c.ID, 0, c.SHA256, c.Size, bytes.NewReader(raw)))
	_, err = f.Seal(ctx, c.Owner, c.ID)
	require.NoError(t, err)
	_, err = f.Store.BeginMailboxJob(ctx, c.Owner, store.MailboxJobRequest{ID: "backup-job", ContainerID: c.ID, ContainerSHA256: c.SHA256, Settings: store.MailboxSettings{DestinationID: f.Store.RootID()}})
	require.NoError(t, err)
	job, err := f.Store.ClaimMailboxJob(ctx)
	require.NoError(t, err)
	require.NoError(t, f.RunJob(ctx, job))
	receipt, err := f.Store.MailboxTransfer(ctx, c.Owner, "mailbox:"+job.CollectionID, "0:1")
	require.NoError(t, err)
	children, err := f.Store.EmailDocumentPublication(ctx, receipt.DocumentPublicationID)
	require.NoError(t, err)
	require.Len(t, children.Relations, 1)
	_, _, err = f.Store.Trash(ctx, receipt.Target.NodeID, receipt.TargetRevision)
	require.NoError(t, err)
	require.ErrorIs(t, f.Store.RemoveEmailDocumentPublication(ctx, children.OperationID, children.RequestDigest), store.ErrMailboxConflict)
	repository, err := backup.Init(filepath.Join(t.TempDir(), "repository"))
	require.NoError(t, err)
	_, err = backupapp.Create(ctx, repository, "mailbox", f.Store, f.Blobs, backup.CreateOptions{Jobs: 2})
	require.NoError(t, err)
	proof, err := backup.Verify(ctx, repository, backupapp.New("mailbox"), backup.VerifyOptions{Jobs: 2})
	require.NoError(t, err)
	require.Empty(t, proof.Problems)
	destination := filepath.Join(t.TempDir(), "restored")
	_, err = backupapp.Restore(ctx, repository, "mailbox", backup.RestoreOptions{TargetDir: destination, Jobs: 2})
	require.NoError(t, err)
	restored, err := store.OpenForRestore(filepath.Join(destination, "docbank.db"), store.DefaultSQLiteDriver())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	bs, err := blob.New(store.NewPackCatalog(restored), filepath.Join(destination, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bs.Close()) })
	_, err = maintenance.GarbageCollect(ctx, restored, bs, maintenance.GCOptions{})
	require.NoError(t, err)
	service := Service{Store: restored, Blobs: bs, Spool: t.TempDir()}
	sealed, err := restored.MailboxContainer(ctx, c.Owner, c.ID)
	require.NoError(t, err)
	reader, err := service.ReaderAt(ctx, sealed)
	require.NoError(t, err)
	got := make([]byte, len(raw))
	_, err = reader.ReadAt(got, 0)
	require.NoError(t, err)
	require.Equal(t, raw, got)
	for _, hash := range []string{receipt.Target.SHA256, children.Relations[0].Child.SHA256} {
		stream, _, err := bs.OpenStreamContext(ctx, hash)
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, stream)
		require.NoError(t, err)
		require.NoError(t, stream.Close())
	}
	source := raw[len(separator):]
	replay, err := service.Transfer(ctx, c.Owner, receipt.Request, bytes.NewReader(source))
	require.NoError(t, err)
	require.Equal(t, "tombstone", replay.Outcome)
	require.Equal(t, receipt.ID, replay.ID)
	require.NoError(t, restored.ValidateMetadata(ctx))
	occurrences, err := restored.MailboxOccurrences(ctx, c.Owner, job.ID, 0, 100)
	require.NoError(t, err)
	require.Len(t, occurrences, 1)
	require.Equal(t, receipt.ID, occurrences[0].ReceiptID)
}
