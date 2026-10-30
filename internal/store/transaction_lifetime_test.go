package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	docsqlite "go.kenn.io/docbank/sqlite"
)

func TestStorageTransactionWaitsForCancelledRollback(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		rollbackErr error
	}{
		{name: "rollback succeeds"},
		{name: "rollback fails", rollbackErr: errors.New("rollback failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				connection := &blockedRollbackConnection{started: make(chan struct{}), release: make(chan struct{}), rollbackErr: test.rollbackErr}
				db := sql.OpenDB(rollbackConnector{connection})
				db.SetMaxOpenConns(1)
				defer func() { require.NoError(t, db.Close()) }()
				catalog := &Store{writeDB: db}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan struct{})
				var mutationErr error
				go func() {
					mutationErr = catalog.withStorageTx(ctx, func(*sql.Tx) error {
						cancel()
						<-connection.started
						return ctx.Err()
					})
					close(done)
				}()
				<-connection.started
				synctest.Wait()
				select {
				case <-done:
					t.Error("storage mutation returned while its transaction was still rolling back")
				default:
				}
				close(connection.release)
				<-done
				require.ErrorIs(t, mutationErr, context.Canceled)
				if test.rollbackErr != nil {
					require.ErrorIs(t, mutationErr, test.rollbackErr)
				}
			})
		})
	}
}

func TestStorageTransactionCancelsBegin(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		connection := &blockedBeginConnection{started: make(chan struct{}), release: make(chan struct{})}
		db := sql.OpenDB(beginConnector{connection})
		defer func() { require.NoError(t, db.Close()) }()
		catalog := &Store{writeDB: db}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		var mutationErr error
		go func() {
			mutationErr = catalog.withStorageTx(ctx, func(*sql.Tx) error {
				return errors.New("unexpected mutation")
			})
			close(done)
		}()
		<-connection.started
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Error("cancelled mutation is still waiting for BEGIN")
		}
		close(connection.release)
		<-done
		require.ErrorIs(t, mutationErr, context.Canceled)
	})
}

type beginConnector struct{ connection *blockedBeginConnection }

func (c beginConnector) Connect(context.Context) (driver.Conn, error) { return c.connection, nil }
func (c beginConnector) Driver() driver.Driver                        { return c }
func (c beginConnector) Open(string) (driver.Conn, error)             { return c.connection, nil }

type blockedBeginConnection struct{ started, release chan struct{} }

func (*blockedBeginConnection) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (*blockedBeginConnection) Begin() (driver.Tx, error) {
	return nil, errors.New("unexpected Begin without context")
}
func (*blockedBeginConnection) Close() error { return nil }
func (c *blockedBeginConnection) BeginTx(ctx context.Context, _ driver.TxOptions) (driver.Tx, error) {
	close(c.started)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.release:
		return nil, errors.New("BEGIN was not cancelled")
	}
}

func holdTestSQLiteWriter(t *testing.T, s *Store) *sql.Tx {
	t.Helper()
	require.NoError(t, s.writeDB.Close())
	var err error
	s.writeDB, err = s.driver.Open(s.path, docsqlite.OpenOptions{Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate, BusyTimeout: 10 * time.Millisecond}) //nolint:kennlint // deadlinetest: the held writer lock must outlast busy attempts to exercise transaction retry and cancellation
	require.NoError(t, err)
	s.writeDB.SetMaxOpenConns(1)
	writer, err := s.driver.Open(s.path, docsqlite.OpenOptions{Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, writer.Close()) })
	lock, err := writer.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := lock.Rollback(); err != nil {
			require.ErrorIs(t, err, sql.ErrTxDone)
		}
	})
	return lock
}

func TestStorageTransactionRetryBegin(t *testing.T) {
	t.Parallel()
	t.Run("blocks until the writer lock clears", func(t *testing.T) {
		s := newTestStore(t)
		lock := holdTestSQLiteWriter(t, s)
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			done <- s.withStorageTxRetryBegin(ctx, func(*sql.Tx) error {
				return nil
			})
		}()
		select {
		case err := <-done:
			t.Fatalf("transaction returned while the writer lock was held: %v", err)
		case <-time.After(100 * time.Millisecond): //nolint:kennlint // deadlinetest: Real SQLite contention must outlast the driver's wall-clock busy timeout.
		}
		require.NoError(t, lock.Rollback())
		require.NoError(t, <-done)
	})
	t.Run("callback busy error is not retried", func(t *testing.T) {
		s := newTestStore(t)
		lock := holdTestSQLiteWriter(t, s)
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		// A callback can return a busy error from a statement inside its transaction.
		_, busyErr := s.writeDB.BeginTx(ctx, nil)
		require.True(t, s.driver.IsBusy(busyErr), "expected busy error, got %v", busyErr)
		require.NoError(t, lock.Rollback())
		calls := 0
		err := s.withStorageTxRetryBegin(ctx, func(*sql.Tx) error {
			calls++
			return busyErr
		})
		require.ErrorIs(t, err, busyErr)
		require.Equal(t, 1, calls)
	})
	t.Run("cancel", func(t *testing.T) {
		s := newTestStore(t)
		holdTestSQLiteWriter(t, s)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		timer := time.AfterFunc(50*time.Millisecond, cancel) //nolint:kennlint // deadlinetest: Exercise cancellation during a real SQLite busy wait.
		defer timer.Stop()
		start := time.Now()
		err := s.withStorageTxRetryBegin(ctx, func(*sql.Tx) error {
			t.Error("callback ran while another writer held the lock")
			return nil
		})
		require.ErrorIs(t, err, context.Canceled)
		require.Less(t, time.Since(start), 500*time.Millisecond, "retry loop kept running after cancellation")
	})
}

type rollbackConnector struct{ connection *blockedRollbackConnection }

func (c rollbackConnector) Connect(context.Context) (driver.Conn, error) { return c.connection, nil }
func (c rollbackConnector) Driver() driver.Driver                        { return c }
func (c rollbackConnector) Open(string) (driver.Conn, error)             { return c.connection, nil }

type blockedRollbackConnection struct {
	started, release chan struct{}
	rollbackErr      error
}

func (*blockedRollbackConnection) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (c *blockedRollbackConnection) Begin() (driver.Tx, error) { return blockedRollback{c}, nil }
func (*blockedRollbackConnection) Close() error                { return nil }

type blockedRollback struct{ connection *blockedRollbackConnection }

func (blockedRollback) Commit() error { return errors.New("unexpected commit") }
func (tx blockedRollback) Rollback() error {
	close(tx.connection.started)
	<-tx.connection.release
	return tx.connection.rollbackErr
}
