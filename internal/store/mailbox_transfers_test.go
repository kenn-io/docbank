package store

import (
	"bytes"
	"github.com/stretchr/testify/require"
	"sync"
	"testing"
)

func TestMailboxTransferAtomicPublicationAndConcurrentRetry(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	f := newEmailFixture(t, s, "seed.eml")
	require.NoError(t, s.RegisterMailboxArchive(ctx, MailboxArchive{ID: "external-synthetic", Owner: "one", Description: "Synthetic exported EML"}))
	run, err := s.BeginIngest(ctx, "mailbox", "Synthetic EML transfer")
	require.NoError(t, err)
	p := MailboxTransferPublication{Owner: "one", Run: run, Request: MailboxTransferRequest{ArchiveID: "external-synthetic", Reference: "message-1", SHA256: fakeHash("00"), Size: 1, Settings: "settings-v1", DestinationID: s.RootID(), Name: "message.eml"}, Email: f.publication}
	source, err := emailVersion(ctx, s.db, f.publication.ContentVersionID)
	require.NoError(t, err)
	p.Request.SHA256 = source.BlobHash
	p.Request.Size = source.Size
	var before int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM nodes`).Scan(&before))
	_, err = s.db.Exec(`CREATE TRIGGER fail_mailbox_receipt BEFORE INSERT ON mailbox_transfer_receipts BEGIN SELECT RAISE(ABORT,'synthetic rollback between message and receipt'); END`)
	require.NoError(t, err)
	_, err = s.PublishMailboxTransfer(ctx, p)
	require.Error(t, err)
	var after int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM nodes`).Scan(&after))
	require.Equal(t, before, after)
	_, err = s.db.Exec(`DROP TRIGGER fail_mailbox_receipt`)
	require.NoError(t, err)
	var wg sync.WaitGroup
	results := make([]MailboxTransferReceipt, 4)
	errs := make([]error, 4)
	for i := range 4 {
		wg.Go(func() { results[i], errs[i] = s.PublishMailboxTransfer(ctx, p) })
	}
	wg.Wait()
	for i := range 4 {
		require.NoError(t, errs[i])
		require.Equal(t, results[0], results[i])
	}
	receipt := results[0]
	require.NotEmpty(t, receipt.EmailAttachmentID)
	require.NotEmpty(t, receipt.DocumentPublicationID)
	children, err := s.EmailDocumentPublication(ctx, receipt.DocumentPublicationID)
	require.NoError(t, err)
	require.Len(t, children.Relations, 1)
	require.NoError(t, s.ValidateMetadata(ctx))
	var out bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &out))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(out.Bytes())))
	again, err := restored.PublishMailboxTransfer(ctx, p)
	require.NoError(t, err)
	require.Equal(t, receipt, again)
	p.Request.Reference = "message-2"
	second, err := s.PublishMailboxTransfer(ctx, p)
	require.NoError(t, err)
	require.NotEqual(t, receipt.Target.NodeID, second.Target.NodeID)
	require.Equal(t, receipt.Target.SHA256, second.Target.SHA256)
}
