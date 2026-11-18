package mailbox

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/store"
)

func TestMailboxWorkerCancellationFencesPendingAndClosesSpools(t *testing.T) {
	now := time.Now()
	synctest.Test(t, func(t *testing.T) {
		// Start the fake clock at the real time the catalog records.
		time.Sleep(time.Until(now))
		f := mailboxFixture(t)
		ctx := t.Context()
		raw := []byte(separator + "Subject: Cancel\nContent-Type: text/plain\n\nSynthetic body.\n")
		c := store.MailboxContainerRequest{ID: "cancel-source", Owner: "one", SHA256: hashBytes(raw), Size: int64(len(raw)), Format: "mbox"}
		_, err := f.Store.BeginMailboxContainer(ctx, c)
		require.NoError(t, err)
		require.NoError(t, f.UploadChunk(ctx, c.Owner, c.ID, 0, c.SHA256, c.Size, bytes.NewReader(raw)))
		_, err = f.Seal(ctx, c.Owner, c.ID)
		require.NoError(t, err)
		request := store.MailboxJobRequest{ID: "cancel-job", ContainerID: c.ID, ContainerSHA256: c.SHA256, Settings: store.MailboxSettings{DestinationID: f.Store.RootID()}}
		_, err = f.Store.BeginMailboxJob(ctx, c.Owner, request)
		require.NoError(t, err)
		pending := make(chan struct{}, 1)
		f.Mutate = func(ctx context.Context, fn func() error) error {
			j, err := f.Store.MailboxJob(ctx, c.Owner, request.ID)
			if err != nil {
				return err
			}
			if j.Pending == 1 {
				select {
				case pending <- struct{}{}:
				default:
				}
				<-ctx.Done()
				return ctx.Err()
			}
			return fn()
		}
		workerCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- f.RunWorker(workerCtx) }()
		select {
		case <-pending:
		case err := <-done:
			t.Fatalf("worker stopped before pending publication: %v", err)
		}
		require.NoError(t, f.Store.CancelMailboxJob(ctx, c.Owner, request.ID))
		// Stop the supervisor only after its per-job watcher (100ms ticks) has
		// noticed cancellation and the job has closed its spools.
		time.Sleep(time.Second)
		entries, err := os.ReadDir(f.Spool)
		require.NoError(t, err)
		for _, entry := range entries {
			require.False(t, strings.HasPrefix(entry.Name(), "docbank-email-"), "spool %s is still open", entry.Name())
		}
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
		j, err := f.Store.MailboxJob(ctx, c.Owner, request.ID)
		require.NoError(t, err)
		require.Equal(t, "canceled", j.State)
		require.Zero(t, j.Checkpoint)
		require.Equal(t, int64(1), j.Canceled)
		require.NoError(t, f.Store.ValidateMetadata(ctx))
	})
}

func TestMailboxCancellationFencesPublicationUntilResume(t *testing.T) {
	checkMailboxCancellationFencesPublicationUntilResume(t, mailboxFixture(t))
}

func checkMailboxCancellationFencesPublicationUntilResume(t *testing.T, f *Service) {
	t.Helper()
	ctx := t.Context()
	raw := []byte(separator + "Subject: Cancel before publication\n\nSynthetic body.\n")
	request := queuedArchive(t, f, raw, "mbox")
	claim, err := f.Store.ClaimMailboxJob(ctx)
	require.NoError(t, err)

	// Cancel after the scanner records a pending occurrence and before the
	// stale worker attempts to commit its decoded message.
	f.Mutate = func(ctx context.Context, fn func() error) error {
		current, err := f.Store.MailboxJob(ctx, claim.Owner, claim.ID)
		if err != nil {
			return err
		}
		if current.Pending == 1 {
			if err := f.Store.CancelMailboxJob(ctx, claim.Owner, claim.ID); err != nil {
				return err
			}
		}
		return fn()
	}
	require.ErrorIs(t, f.RunJob(ctx, claim), store.ErrMailboxConflict)
	canceled, err := f.Store.MailboxJob(ctx, claim.Owner, claim.ID)
	require.NoError(t, err)
	require.Equal(t, "canceled", canceled.State)
	require.Zero(t, canceled.Checkpoint)
	occurrences, err := f.Store.MailboxOccurrences(ctx, claim.Owner, claim.ID, 0, 10)
	require.NoError(t, err)
	require.Len(t, occurrences, 1)
	require.Equal(t, "canceled", occurrences[0].Outcome)
	require.Nil(t, occurrences[0].Target)
	_, err = f.Store.MailboxTransfer(ctx, claim.Owner, "mailbox:"+claim.CollectionID, "0:1")
	require.ErrorIs(t, err, store.ErrNotFound)

	f.Mutate = nil
	_, err = f.Store.ResumeMailboxJob(ctx, claim.Owner, request, false)
	require.NoError(t, err)
	resumed, err := f.Store.ClaimMailboxJob(ctx)
	require.NoError(t, err)
	require.NotEqual(t, claim.Claim, resumed.Claim)
	require.NoError(t, f.RunJob(ctx, resumed))
	complete, err := f.Store.MailboxJob(ctx, claim.Owner, claim.ID)
	require.NoError(t, err)
	require.Equal(t, "complete", complete.State)
	require.Equal(t, int64(1), complete.Imported)
	occurrences, err = f.Store.MailboxOccurrences(ctx, claim.Owner, claim.ID, 0, 10)
	require.NoError(t, err)
	require.Len(t, occurrences, 1)
	require.Equal(t, "imported", occurrences[0].Outcome)
	require.NotNil(t, occurrences[0].Target)
}
