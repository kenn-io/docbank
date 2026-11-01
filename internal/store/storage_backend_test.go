package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func TestStorageTransactionsSQLite(t *testing.T) {
	t.Parallel()
	checkStorageTransactions(t, newTestStore(t))
}

func TestStorageTransactionsPostgres(t *testing.T) {
	t.Parallel()
	checkStorageTransactions(t, newPostgresQueryStore(t))
}

func checkStorageTransactions(t *testing.T, s *Store) {
	t.Helper()
	events, unsubscribe := s.SubscribePhotoEvents()
	defer unsubscribe()
	callbackErr := errors.New("discard mutation")
	highWater := s.RootID()
	for _, outcome := range []string{"commit", "callback error", "cancel", "commit error", "commit after failures"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var id int64
			err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
				kind, version := nodeKindDir, (*string)(nil)
				if outcome == "commit error" {
					kind, version = nodeKindFile, new("11111111-1111-4111-8111-111111111111")
				}
				var err error
				id, err = s.insertNodeTx(ctx, tx, s.RootID(), outcome, kind, version, "2026-01-01T00:00:00.000000000Z")
				if err != nil {
					return err
				}
				select {
				case <-events:
					return errors.New("published before commit")
				default:
				}
				switch outcome {
				case "callback error":
					return callbackErr
				case "cancel":
					cancel()
				}
				return nil
			})
			require.Equal(t, highWater+1, id, "failed transactions must not consume node IDs")
			if err == nil {
				highWater = id
			}
			sequence, sequenceErr := s.auditNodeSequence(t.Context(), s.db)
			require.NoError(t, sequenceErr)
			require.Equal(t, highWater, sequence)
			switch outcome {
			case "callback error":
				require.ErrorIs(t, err, callbackErr)
			case "cancel":
				require.ErrorIs(t, err, context.Canceled)
			case "commit error":
				require.ErrorContains(t, err, "committing transaction")
			default:
				require.NoError(t, err)
			}
			if err != nil {
				_, readErr := s.NodeByID(t.Context(), id)
				require.ErrorIs(t, readErr, ErrNotFound)
				select {
				case <-events:
					t.Fatal("failed mutation published a change")
				default:
				}
				return
			}
			select {
			case <-events:
			default:
				t.Fatal("committed mutation did not publish a change")
			}
			node, err := s.NodeByID(t.Context(), id)
			require.NoError(t, err)
			require.Equal(t, outcome, node.Name)
		})
	}
}

func TestStorageWriterAdmissionPostgres(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"commit", "rollback", "cancel waiter"} {
		t.Run(outcome, func(t *testing.T) {
			s := newPostgresQueryStore(t)
			// A server default must not pin a writer's snapshot before admission.
			conn, err := s.db.Conn(t.Context())
			require.NoError(t, err)
			var config *pgx.ConnConfig
			err = conn.Raw(func(raw any) error {
				pgConn, ok := raw.(*stdlib.Conn)
				require.True(t, ok)
				config = pgConn.Conn().Config().Copy()
				return nil
			})
			require.NoError(t, conn.Close())
			require.NoError(t, err)
			config.RuntimeParams["default_transaction_isolation"] = "repeatable read"
			s.writeDB = stdlib.OpenDB(*config)
			t.Cleanup(func() { require.NoError(t, s.writeDB.Close()) })
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			release := make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			firstReady, firstDone := make(chan struct{}), make(chan struct{})
			rollback := errors.New("discard first mutation")
			var firstErr error
			go func() {
				defer close(firstDone)
				firstErr = s.withStorageTx(ctx, func(tx *sql.Tx) error {
					if _, err := tx.ExecContext(ctx, `UPDATE nodes SET revision=2 WHERE id=$1`, s.RootID()); err != nil {
						return err
					}
					close(firstReady)
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
					if outcome == "rollback" {
						return rollback
					}
					return nil
				})
			}()
			t.Cleanup(func() { unblock(); cancel(); <-firstDone })
			select {
			case <-firstReady:
			case <-firstDone:
				t.Fatalf("writer did not reach mutation: %v", firstErr)
			}

			waitCtx, cancelWait := context.WithCancel(ctx)
			defer cancelWait()
			read := make(chan int64, 1)
			secondDone := make(chan struct{})
			var secondErr error
			go func() {
				defer close(secondDone)
				secondErr = s.withStorageTx(waitCtx, func(tx *sql.Tx) error {
					node, err := nodeByIDTx(tx, s.RootID())
					if err != nil {
						return err
					}
					read <- node.Revision
					_, err = tx.ExecContext(waitCtx, `UPDATE nodes SET revision=$1 WHERE id=$2`, node.Revision+1, node.ID)
					return err
				})
			}()
			t.Cleanup(func() { unblock(); cancelWait(); <-secondDone })
			// Observe actual database admission, not a scheduling delay. Without
			// the writer lock the callback reads the old revision immediately.
			early := false
			require.Eventually(t, func() bool {
				select {
				case <-read:
					early = true
					return true
				default:
				}
				var waiting bool
				err := s.db.QueryRowContext(ctx, `SELECT EXISTS (
					SELECT 1 FROM pg_catalog.pg_locks WHERE locktype='advisory' AND NOT granted
					AND database=(SELECT oid FROM pg_catalog.pg_database WHERE datname=current_database()))`).Scan(&waiting)
				return err == nil && waiting
			}, 5*time.Second, 10*time.Millisecond)
			require.False(t, early, "second writer read before the first transaction ended")
			if outcome == "cancel waiter" {
				cancelWait()
				<-secondDone
				require.ErrorIs(t, secondErr, context.Canceled)
				require.Empty(t, read, "cancelled waiter must not enter the mutation")
			}
			unblock()
			<-firstDone
			if outcome == "rollback" {
				require.ErrorIs(t, firstErr, rollback)
			} else {
				require.NoError(t, firstErr)
			}
			if outcome != "cancel waiter" {
				<-secondDone
				require.NoError(t, secondErr)
				want := int64(2)
				if outcome == "rollback" {
					want = 1
				}
				require.Equal(t, want, <-read, "the admitted writer must read the preceding committed state")
			}
			// A cancelled waiter or failed writer must leave admission usable.
			require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `UPDATE nodes SET modified_at=$1 WHERE id=$2`, "2026-01-02T00:00:00.000000000Z", s.RootID())
				return err
			}))
		})
	}
}

func TestStorageWritersSeparateSchemasPostgres(t *testing.T) {
	t.Parallel()
	setup, owner := pgtest.NewDatabase(t)
	first := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "first_vault", SchemaOwner: owner})
	second := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "second_vault", SchemaOwner: owner})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	// The second schema must commit while the first still owns its writer.
	require.NoError(t, first.withStorageTx(ctx, func(tx *sql.Tx) error {
		if _, err := first.insertNodeTx(ctx, tx, first.RootID(), "first", nodeKindDir, nil, "2026-01-01T00:00:00.000000000Z"); err != nil {
			return err
		}
		return second.withStorageTx(ctx, func(tx *sql.Tx) error {
			_, err := second.insertNodeTx(ctx, tx, second.RootID(), "second", nodeKindDir, nil, "2026-01-01T00:00:00.000000000Z")
			return err
		})
	}))
	_, err := first.ChildByName(ctx, first.RootID(), "first")
	require.NoError(t, err)
	_, err = second.ChildByName(ctx, second.RootID(), "second")
	require.NoError(t, err)
	_, err = first.ChildByName(ctx, first.RootID(), "second")
	require.ErrorIs(t, err, ErrNotFound)
}
