package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
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
