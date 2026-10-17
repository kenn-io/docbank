package mailbox

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
	"io"
	"strings"
	"testing"
)

func TestMailboxImportCompleteDistinctOccurrencesAndContinuation(t *testing.T) {
	f := mailboxFixture(t)
	ctx := t.Context()
	message := separator + "Subject: Synthetic archive\nMessage-ID: <repeated@example.test>\nX-Gmail-Labels: Inbox,Project\nContent-Type: text/plain\n\nSynthetic body.\n"
	raw := []byte(message + message + message)
	c := store.MailboxContainerRequest{ID: "archive", Owner: "one", SHA256: hashBytes(raw), Size: int64(len(raw)), Format: "mbox"}
	_, err := f.Store.BeginMailboxContainer(ctx, c)
	require.NoError(t, err)
	require.NoError(t, f.UploadChunk(ctx, "one", c.ID, 0, c.SHA256, c.Size, bytes.NewReader(raw)))
	_, err = f.Seal(ctx, "one", c.ID)
	require.NoError(t, err)
	request := store.MailboxJobRequest{ID: "import", ContainerID: c.ID, ContainerSHA256: c.SHA256, Settings: store.MailboxSettings{DestinationID: f.Store.RootID()}}
	tag, err := f.Store.CreateTag(ctx, "Mapped project")
	require.NoError(t, err)
	request.Settings.LabelTags = map[string]string{"Project": tag.ID}
	for i := range 99 {
		request.Settings.LabelTags[fmt.Sprintf("%03d-%s", i, strings.Repeat("x", 248))] = tag.ID
	}
	_, err = f.Store.BeginMailboxJob(ctx, "one", request)
	require.NoError(t, err)
	job, err := f.Store.ClaimMailboxJob(ctx)
	require.NoError(t, err)
	require.NoError(t, f.runJob(ctx, job, 2))
	partial, err := f.Store.MailboxJob(ctx, "one", job.ID)
	require.NoError(t, err)
	require.Equal(t, "partial", partial.State)
	require.False(t, partial.ScannedTail)
	require.Equal(t, int64(2), partial.Imported)
	_, err = f.Store.ResumeMailboxJob(ctx, "one", request, true)
	require.NoError(t, err)
	job, err = f.Store.ClaimMailboxJob(ctx)
	require.NoError(t, err)
	require.NoError(t, f.RunJob(ctx, job))
	complete, err := f.Store.MailboxJob(ctx, "one", job.ID)
	require.NoError(t, err)
	require.Equal(t, "complete", complete.State)
	require.True(t, complete.ScannedTail)
	require.Equal(t, int64(3), complete.Imported)
	occurrences, err := f.Store.MailboxOccurrences(ctx, "one", job.ID, 0, 100)
	require.NoError(t, err)
	require.Len(t, occurrences, 3)
	require.Equal(t, []string{"Inbox", "Project"}, occurrences[0].Location.Labels)
	first, err := f.Store.MailboxTransfer(ctx, "one", "mailbox:"+job.CollectionID, "0:1")
	require.NoError(t, err)
	require.Equal(t, first.Target.SHA256, occurrences[0].Location.EMLSHA256)
	require.Equal(t, first.Target.Size, occurrences[0].Location.EMLSize)
	require.Equal(t, first.Target, *occurrences[0].Target)
	second, err := f.Store.MailboxTransfer(ctx, "one", "mailbox:"+job.CollectionID, "0:2")
	require.NoError(t, err)
	require.Equal(t, first.Target.SHA256, second.Target.SHA256)
	require.NotEqual(t, first.Target.NodeID, second.Target.NodeID)
	tags, _, err := f.Store.NodeTags(ctx, first.Target.NodeID, 100, 0)
	require.NoError(t, err)
	require.Len(t, tags, 1)
	require.Equal(t, tag.ID, tags[0].ID)
	require.NoError(t, f.Store.ValidateMetadata(ctx))
}

func TestMailboxPreviewBoundsEntryResults(t *testing.T) {
	f := mailboxFixture(t)
	ctx := t.Context()
	var raw bytes.Buffer
	z := zip.NewWriter(&raw)
	for i := range 101 {
		w, err := z.Create(fmt.Sprintf("Takeout/Mail/%03d.mbox", i))
		require.NoError(t, err)
		_, err = io.WriteString(w, separator+"Subject: Synthetic\n\nBody\n")
		require.NoError(t, err)
	}
	require.NoError(t, z.Close())
	c := store.MailboxContainerRequest{ID: "preview", Owner: "one", SHA256: hashBytes(raw.Bytes()), Size: int64(raw.Len()), Format: "zip"}
	_, err := f.Store.BeginMailboxContainer(ctx, c)
	require.NoError(t, err)
	require.NoError(t, f.UploadChunk(ctx, c.Owner, c.ID, 0, c.SHA256, c.Size, bytes.NewReader(raw.Bytes())))
	_, err = f.Seal(ctx, c.Owner, c.ID)
	require.NoError(t, err)
	preview, err := f.Preview(ctx, c.Owner, c.ID, "mboxrd")
	require.NoError(t, err)
	require.Len(t, preview.Entries, 100)
	require.Equal(t, 101, preview.EntryCount)
	require.Len(t, preview.Samples, 3)
	require.True(t, preview.HasMore)
}

func TestMailboxMalformedEntryReportsPartialCommittedWork(t *testing.T) {
	f := mailboxFixture(t)
	ctx := t.Context()
	var raw bytes.Buffer
	z := zip.NewWriter(&raw)
	for i, content := range []string{separator + "Subject: Synthetic\n\nBody\n", "not a mailbox"} {
		w, err := z.Create(fmt.Sprintf("%d.mbox", i))
		require.NoError(t, err)
		_, err = io.WriteString(w, content)
		require.NoError(t, err)
	}
	require.NoError(t, z.Close())
	c := store.MailboxContainerRequest{ID: "partial-source", Owner: "one", SHA256: hashBytes(raw.Bytes()), Size: int64(raw.Len()), Format: "zip"}
	_, err := f.Store.BeginMailboxContainer(ctx, c)
	require.NoError(t, err)
	require.NoError(t, f.UploadChunk(ctx, c.Owner, c.ID, 0, c.SHA256, c.Size, bytes.NewReader(raw.Bytes())))
	_, err = f.Seal(ctx, c.Owner, c.ID)
	require.NoError(t, err)
	_, err = f.Store.BeginMailboxJob(ctx, c.Owner, store.MailboxJobRequest{ID: "partial-job", ContainerID: c.ID, ContainerSHA256: c.SHA256, Settings: store.MailboxSettings{DestinationID: f.Store.RootID()}})
	require.NoError(t, err)
	j, err := f.Store.ClaimMailboxJob(ctx)
	require.NoError(t, err)
	require.Error(t, f.RunJob(ctx, j))
	got, err := f.Store.MailboxJob(ctx, c.Owner, j.ID)
	require.NoError(t, err)
	require.Equal(t, "partial", got.State)
	require.Equal(t, int64(1), got.Imported)
	require.False(t, got.ScannedTail)
}

func TestMailboxTransferMaintenanceFence(t *testing.T) {
	f := mailboxFixture(t)
	ctx := t.Context()
	require.NoError(t, f.Store.RegisterMailboxArchive(ctx, store.MailboxArchive{ID: "external", Owner: "one", Description: "Synthetic"}))
	raw := []byte("Subject: Synthetic\nContent-Type: text/plain\n\nBody.\n")
	blocked := errors.New("maintenance active")
	f.Mutate = func(context.Context, func() error) error { return blocked }
	_, err := f.Transfer(ctx, "one", store.MailboxTransferRequest{ArchiveID: "external", Reference: "one", SHA256: hashBytes(raw), Size: int64(len(raw)), Settings: "v1", DestinationID: f.Store.RootID(), Name: "message.eml"}, bytes.NewReader(raw))
	require.ErrorIs(t, err, blocked)
	_, err = f.Store.MailboxTransfer(ctx, "one", "external", "one")
	require.ErrorIs(t, err, store.ErrNotFound)
}
