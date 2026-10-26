package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
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

func holdTestSQLiteWriter(t *testing.T, s *Store, busyTimeout time.Duration) *sql.Tx {
	t.Helper()
	if busyTimeout != 0 {
		require.NoError(t, s.writeDB.Close())
		var err error
		s.writeDB, err = s.driver.Open(s.path, docsqlite.OpenOptions{Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate, BusyTimeout: busyTimeout})
		require.NoError(t, err)
		s.writeDB.SetMaxOpenConns(1)
	}
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
	for _, mode := range []string{"ordinary busy and callback once", "cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			s := newTestStore(t)
			var busyTimeout time.Duration
			if mode == "ordinary busy and callback once" {
				busyTimeout = 10 * time.Millisecond
			}
			lock := holdTestSQLiteWriter(t, s, busyTimeout)
			if mode == "ordinary busy and callback once" {
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				err := s.withStorageTx(ctx, func(*sql.Tx) error {
					t.Error("ordinary callback ran while another writer held the lock")
					return nil
				})
				require.True(t, s.driver.IsBusy(err), "expected busy error, got %v", err)
				require.NoError(t, lock.Rollback())
				busyErr, calls := err, 0
				err = s.withStorageTxRetryBegin(ctx, func(tx *sql.Tx) error {
					calls++
					var timeout int
					require.NoError(t, tx.QueryRow("PRAGMA busy_timeout").Scan(&timeout))
					require.Equal(t, 10, timeout)
					return busyErr
				})
				require.ErrorIs(t, err, busyErr)
				require.Equal(t, 1, calls)
				return
			}
			var ctx context.Context
			var cancel context.CancelFunc
			want := context.Canceled
			if mode == "deadline" {
				ctx, cancel = context.WithTimeout(t.Context(), 50*time.Millisecond) //nolint:kennlint // Exercise cancellation during a real SQLite busy wait.
				want = context.DeadlineExceeded
			} else {
				ctx, cancel = context.WithCancel(t.Context())
				timer := time.AfterFunc(50*time.Millisecond, cancel) //nolint:kennlint // Exercise cancellation during a real SQLite busy wait.
				defer timer.Stop()
			}
			defer cancel()
			start := time.Now()
			err := s.withStorageTxRetryBegin(ctx, func(*sql.Tx) error {
				t.Error("callback ran while another writer held the lock")
				return nil
			})
			require.ErrorIs(t, err, want)
			require.Less(t, time.Since(start), 500*time.Millisecond, "cancellation waited for SQLite's busy timeout")
		})
	}
}

func TestStorageTransactionDiscardsConnectionOnTimeoutRestoreFailure(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"begin succeeds", "begin fails"} {
		t.Run(name, func(t *testing.T) {
			connection := &timeoutRestoreFailureConnection{restoreErr: errors.New("timeout restore failed")}
			if name == "begin fails" {
				connection.beginErr = errors.New("begin failed")
			}
			db := sql.OpenDB(rollbackConnector{connection})
			defer func() { require.NoError(t, db.Close()) }()
			s := &Store{writeDB: db, driver: DefaultSQLiteDriver()}
			err := s.withStorageTxRetryBegin(t.Context(), func(*sql.Tx) error {
				t.Error("callback ran after timeout restoration failed")
				return nil
			})
			require.ErrorIs(t, err, connection.restoreErr)
			require.Equal(t, name == "begin succeeds", connection.rolledBack)
			require.True(t, connection.closed)
			require.Zero(t, db.Stats().OpenConnections)
		})
	}
}

type timeoutRestoreFailureConnection struct {
	restoreErr, beginErr error
	rolledBack, closed   bool
}

func (*timeoutRestoreFailureConnection) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (c *timeoutRestoreFailureConnection) Close() error { c.closed = true; return nil }
func (c *timeoutRestoreFailureConnection) Begin() (driver.Tx, error) {
	if c.beginErr != nil {
		return nil, c.beginErr
	}
	return c, nil
}
func (*timeoutRestoreFailureConnection) Commit() error     { return errors.New("unexpected commit") }
func (c *timeoutRestoreFailureConnection) Rollback() error { c.rolledBack = true; return nil }
func (c *timeoutRestoreFailureConnection) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	if query == "PRAGMA busy_timeout=0" {
		return driver.RowsAffected(0), nil
	}
	if query == "PRAGMA busy_timeout=5000" {
		return nil, c.restoreErr
	}
	return nil, errors.New("unexpected exec: " + query)
}
func (*timeoutRestoreFailureConnection) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if query != "PRAGMA busy_timeout" {
		return nil, errors.New("unexpected query: " + query)
	}
	return &timeoutRows{}, nil
}

type timeoutRows struct{ read bool }

func (*timeoutRows) Columns() []string { return []string{"timeout"} }
func (*timeoutRows) Close() error      { return nil }
func (r *timeoutRows) Next(values []driver.Value) error {
	if r.read {
		return io.EOF
	}
	r.read = true
	values[0] = int64(5000)
	return nil
}

type rollbackConnector struct{ connection driver.Conn }

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
