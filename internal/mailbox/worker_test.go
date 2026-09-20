package mailbox

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
	docsqlite "go.kenn.io/docbank/sqlite"
)

type contentionDriver struct {
	docsqlite.Driver

	db *sql.DB
}

func TestMailboxWorkerLogsJobFailureAndProcessesNextJob(t *testing.T) {
	f := mailboxFixture(t)
	r := queuedArchive(t, f, []byte("not a mailbox"), "mbox")
	r.ID = "second-job"
	_, err := f.Store.BeginMailboxJob(t.Context(), "one", r)
	require.NoError(t, err)
	var output bytes.Buffer
	f.Logger = slog.New(slog.NewTextHandler(&output, nil))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.RunWorker(ctx) }()
	require.Eventually(t, func() bool {
		j, err := f.Store.MailboxJob(t.Context(), "one", r.ID)
		return err == nil && j.State == "failed"
	}, 3*time.Second, 10*time.Millisecond)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.Contains(t, output.String(), "mailbox job failed")
	require.Contains(t, output.String(), "job=job")
	require.Contains(t, output.String(), "job=second-job")
}

func (d *contentionDriver) Open(path string, options docsqlite.OpenOptions) (*sql.DB, error) {
	options.BusyTimeout = time.Millisecond
	db, err := d.Driver.Open(path, options)
	if err == nil {
		d.db = db
	}
	return db, err
}

func TestMailboxWorkerSurvivesCatalogContention(t *testing.T) {
	for _, phase := range []string{"claim", "watch"} {
		t.Run(phase, func(t *testing.T) {
			driver := &contentionDriver{Driver: store.DefaultSQLiteDriver()}
			f := mailboxFixture(t, driver)
			r := queuedArchive(t, f, []byte(separator+"Subject: Synthetic\n\nBody\n"), "mbox")
			// Allow real database and file I/O to finish on slower CI runners.
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			// Only the external locker should cause contention in this fixture.
			driver.db.SetMaxOpenConns(1)
			driver.db.SetMaxIdleConns(0)
			locker, err := driver.Driver.Open(filepath.Join(f.Spool, "docbank.db"), docsqlite.OpenOptions{Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate})
			require.NoError(t, err)
			defer func() { require.NoError(t, locker.Close()) }()
			locker.SetMaxOpenConns(1)
			_, err = locker.ExecContext(ctx, "PRAGMA locking_mode=EXCLUSIVE")
			require.NoError(t, err)
			ready, resume := make(chan struct{}), make(chan struct{})
			calls := 0
			f.Mutate = func(ctx context.Context, fn func() error) error {
				calls++
				target := 1
				if phase == "watch" {
					target = 2
				}
				if calls == target {
					close(ready)
					select {
					case <-resume:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return fn()
			}
			done := make(chan struct{})
			var workerErr error
			go func() {
				defer close(done)
				workerErr = f.RunWorker(ctx)
			}()
			defer func() {
				cancel()
				<-done
				require.ErrorIs(t, workerErr, context.Canceled)
			}()
			select {
			case <-ready:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			tx, err := locker.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback() }()
			_, err = f.Store.MailboxJob(ctx, "one", r.ID)
			require.True(t, f.Store.RenditionJobErrorRetryable(err), "real read contention: %v", err)
			if phase == "claim" {
				close(resume)
			}
			// The watcher ticks every 100ms; the real exclusive lock exceeds that.
			select {
			case <-done:
				t.Fatalf("worker exited under temporary contention: %v", workerErr)
			case <-time.After(350 * time.Millisecond):
			}
			require.NoError(t, tx.Rollback())
			require.NoError(t, locker.Close())
			if phase == "watch" {
				close(resume)
			}
			require.Eventually(t, func() bool {
				j, err := f.Store.MailboxJob(ctx, "one", r.ID)
				return err == nil && j.State == "complete" && j.Imported == 1
			}, 10*time.Second, 10*time.Millisecond)
		})
	}
}
