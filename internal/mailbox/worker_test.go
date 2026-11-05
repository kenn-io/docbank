package mailbox

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
	docsqlite "go.kenn.io/docbank/sqlite"
)

type contentionDriver struct {
	docsqlite.Driver

	dbs []*sql.DB
}

func TestMailboxWorkerLogsJobFailureAndProcessesNextJob(t *testing.T) {
	f := mailboxFixture(t)
	r := queuedArchive(t, f, []byte("not a mailbox"), "mbox")
	r.ID = "second-job"
	_, err := f.Store.BeginMailboxJob(t.Context(), "one", r)
	require.NoError(t, err)
	output, err := os.CreateTemp(t.TempDir(), "worker.log")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, output.Close()) })
	logged := make(chan string, 2)
	f.Logger = slog.New(jobLogHandler{Handler: slog.NewTextHandler(output, nil), jobs: logged})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.RunWorker(ctx) }()
	// The database records failure before the worker writes its error log.
	for job := ""; job != "second-job"; {
		select {
		case job = <-logged:
		case err := <-done:
			t.Fatalf("worker stopped before logging the second job: %v", err)
		}
	}
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	data, err := os.ReadFile(output.Name())
	require.NoError(t, err)
	require.Contains(t, string(data), "mailbox job failed")
	require.Contains(t, string(data), "job=job")
	require.Contains(t, string(data), "job=second-job")
}

// jobLogHandler reports the job named by each record after writing it, so a
// test can wait for a specific log line instead of polling the log file.
type jobLogHandler struct {
	slog.Handler

	jobs chan<- string
}

func (h jobLogHandler) Handle(ctx context.Context, record slog.Record) error {
	if err := h.Handler.Handle(ctx, record); err != nil {
		return fmt.Errorf("write log record: %w", err)
	}
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == "job" {
			h.jobs <- attr.Value.String()
		}
		return true
	})
	return nil
}

func (d *contentionDriver) Open(path string, options docsqlite.OpenOptions) (*sql.DB, error) {
	// A held SQLite lock fails a statement at once, so contention reaches
	// the worker's retry loop without SQLite sleeping on the wall clock.
	options.BusyTimeout = docsqlite.NoBusyWait
	db, err := d.Driver.Open(path, options)
	if err == nil {
		d.dbs = append(d.dbs, db)
	}
	return db, err
}

func TestMailboxWorkerSurvivesCatalogContention(t *testing.T) {
	for _, phase := range []string{"claim", "watch"} {
		t.Run(phase, func(t *testing.T) {
			now := time.Now()
			synctest.Test(t, func(t *testing.T) {
				// Start the fake clock at the real time the catalog records.
				time.Sleep(time.Until(now))
				driver := &contentionDriver{Driver: store.DefaultSQLiteDriver()}
				f := mailboxFixture(t, driver)
				r := queuedArchive(t, f, []byte(separator+"Subject: Synthetic\n\nBody\n"), "mbox")
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				// Only the external locker should cause contention in this fixture.
				for _, db := range driver.dbs {
					db.SetMaxOpenConns(1)
					db.SetMaxIdleConns(0)
				}
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
				case <-done:
					t.Fatalf("worker exited before contention: %v", workerErr)
				}
				tx, err := locker.BeginTx(ctx, nil)
				require.NoError(t, err)
				defer func() { _ = tx.Rollback() }()
				_, err = f.Store.MailboxJob(ctx, "one", r.ID)
				require.True(t, f.Store.RenditionJobErrorRetryable(err), "real read contention: %v", err)
				if phase == "claim" {
					close(resume)
				}
				// Several claim polls (250ms) or job watcher ticks (100ms) meet the lock.
				time.Sleep(time.Second)
				select {
				case <-done:
					t.Fatalf("worker exited under temporary contention: %v", workerErr)
				default:
				}
				require.NoError(t, tx.Rollback())
				require.NoError(t, locker.Close())
				// Reuse connections once the exclusive lock is gone, so cancellation
				// cannot leave a replacement connection opening during cleanup.
				for _, db := range driver.dbs {
					db.SetMaxIdleConns(1)
				}
				if phase == "watch" {
					close(resume)
				}
				// Give the worker time to finish the import and go idle.
				time.Sleep(time.Minute)
				j, err := f.Store.MailboxJob(ctx, "one", r.ID)
				require.NoError(t, err)
				require.Equal(t, "complete", j.State)
				require.Equal(t, int64(1), j.Imported)
			})
		})
	}
}
