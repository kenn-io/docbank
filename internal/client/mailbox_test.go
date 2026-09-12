package client_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
	"strings"
	"testing"
)

func TestMailboxTypedClientUploadPreviewAndDurableJob(t *testing.T) {
	c, s := newClient(t, serverKey)
	ctx := t.Context()
	raw := []byte("From synthetic@example.test Sat Sep 12 10:00:00 2026\nSubject: Synthetic\n\nBody\n")
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	req := store.MailboxContainerRequest{ID: "client-source", SHA256: hash, Size: int64(len(raw)), Format: "mbox"}
	_, err := c.BeginMailboxContainer(ctx, req)
	require.NoError(t, err)
	require.NoError(t, c.UploadMailboxChunk(ctx, req.ID, 0, hash, req.Size, bytes.NewReader(raw)))
	sealed, err := c.SealMailboxContainer(ctx, req.ID)
	require.NoError(t, err)
	require.Equal(t, "sealed", sealed.State)
	preview, err := c.PreviewMailbox(ctx, req.ID, "mboxrd")
	require.NoError(t, err)
	require.Len(t, preview.Samples, 1)
	request := store.MailboxJobRequest{ID: "client-job", ContainerID: req.ID, ContainerSHA256: hash, Settings: store.MailboxSettings{DestinationID: s.RootID()}}
	job, err := c.BeginMailboxJob(ctx, request)
	require.NoError(t, err)
	require.Equal(t, "queued", job.State)
	require.NoError(t, c.CancelMailboxJob(ctx, job.ID))
	got, err := c.MailboxJob(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, "canceled", got.State)
	var events []store.MailboxJob
	require.NoError(t, c.WatchMailboxJob(ctx, job.ID, func(j store.MailboxJob) error { events = append(events, j); return nil }))
	require.Len(t, events, 1)
	require.Equal(t, "canceled", events[0].State)
}

func TestMailboxClientReadsFullBoundedJobPage(t *testing.T) {
	c, s := newClient(t, serverKey)
	ctx := t.Context()
	owner := "vault:" + s.VaultID()
	request := store.MailboxContainerRequest{ID: "source", Owner: owner, SHA256: strings.Repeat("a", 64), Size: 1, Format: "mbox"}
	_, err := s.BeginMailboxContainer(ctx, request)
	require.NoError(t, err)
	require.NoError(t, s.RecordRenditionBlob(ctx, request.SHA256, 1, store.BlobPhysical{Encoding: "raw", StoredBytes: 1, PackEligible: true, Created: true}))
	require.NoError(t, s.PutMailboxChunk(ctx, owner, request.ID, store.MailboxChunk{Index: 0, SHA256: request.SHA256, Size: 1}))
	_, err = s.SealMailboxContainer(ctx, owner, request.ID, request.SHA256, 1)
	require.NoError(t, err)
	mapping := map[string]string{}
	for i := range 100 {
		mapping[fmt.Sprintf("%03d-%s", i, strings.Repeat("x", 248))] = strings.Repeat("t", 128)
	}
	for i := range 80 {
		id := fmt.Sprintf("job-%03d", i)
		_, err = s.BeginMailboxJob(ctx, owner, store.MailboxJobRequest{ID: id, ContainerID: request.ID, ContainerSHA256: request.SHA256, Settings: store.MailboxSettings{DestinationID: s.RootID(), LabelTags: mapping}})
		require.NoError(t, err)
		require.NoError(t, s.CancelMailboxJob(ctx, owner, id))
	}
	jobs, err := c.MailboxJobs(ctx, "", 100)
	require.NoError(t, err)
	require.Len(t, jobs, 80)
}
