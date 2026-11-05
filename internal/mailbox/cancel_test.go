package mailbox

import (
	"bytes"
	"context"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"
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
