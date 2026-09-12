package mailbox

import (
	"bytes"
	"context"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMailboxWorkerCancellationFencesPendingAndClosesSpools(t *testing.T) {
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
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not reach pending publication")
	}
	require.NoError(t, f.Store.CancelMailboxJob(ctx, c.Owner, request.ID))
	// Stop the supervisor only after its per-job watcher has noticed cancellation.
	require.Eventually(t, func() bool {
		entries, err := os.ReadDir(f.Spool)
		if err != nil {
			return false
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "docbank-email-") {
				return false
			}
		}
		return true
	}, 5*time.Second, 10*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("worker failed to stop")
	}
	j, err := f.Store.MailboxJob(ctx, c.Owner, request.ID)
	require.NoError(t, err)
	require.Equal(t, "canceled", j.State)
	require.Zero(t, j.Checkpoint)
	require.Equal(t, int64(1), j.Canceled)
	require.NoError(t, f.Store.ValidateMetadata(ctx))
}
