//go:build cgo

package store

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"

	sqlite3 "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func installSingletonAuthorizer(t *testing.T, s *Store, authorize func(int, string, string, string) int) {
	t.Helper()
	conn, err := s.writeDB.Conn(context.Background())
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()
	require.NoError(t, conn.Raw(func(raw any) error {
		actual, ok := raw.(*sqlite3.SQLiteConn)
		require.True(t, ok)
		actual.RegisterAuthorizer(authorize)
		return nil
	}))
}

func TestSemanticSearchSourceSingletonSQLRecovery(t *testing.T) {
	t.Parallel()
	for _, failedCleanup := range []string{"", "ROLLBACK", "RELEASE"} {
		t.Run("deny "+failedCleanup, func(t *testing.T) {
			s, _, records, _ := newSemanticPrefixFixture(t, document.EmbeddingInputOriginalFile, 2, false)
			record := records[0]
			wantCollector := newOriginalSemanticSnapshot(record.ProcessingProfileFingerprint, record.BindingID, record.VectorSpace.ID, record.InputKind, SearchOptions{}, true)
			var want VectorIndexSource
			require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
				var err error
				want, err = captureVectorIndexSourceObservedTx(t.Context(), tx, record.VectorSpace.ID, wantCollector.observe)
				return err
			}))
			var denied, rollbacks, releases atomic.Int64
			installSingletonAuthorizer(t, s, func(action int, a, b, c string) int {
				if action == sqlite3.SQLITE_FUNCTION && b == "typeof" && denied.CompareAndSwap(0, 1) {
					return sqlite3.SQLITE_DENY
				}
				if action == sqlite3.SQLITE_SAVEPOINT {
					if a == "ROLLBACK" {
						rollbacks.Add(1)
					}
					if a == "RELEASE" {
						releases.Add(1)
					}
					if a == failedCleanup {
						return sqlite3.SQLITE_DENY
					}
				}
				return sqlite3.SQLITE_OK
			})
			defer installSingletonAuthorizer(t, s, nil)
			collector := newOriginalSemanticSnapshot(record.ProcessingProfileFingerprint, record.BindingID, record.VectorSpace.ID, record.InputKind, SearchOptions{}, true)
			var got VectorIndexSource
			err := s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
				var err error
				got, err = s.captureSemanticSearchSourceTx(t.Context(), tx, record.VectorSpace.ID, collector)
				return err
			})
			require.EqualValues(t, 1, denied.Load(), "real SQLite speculative statement was denied")
			require.EqualValues(t, 1, rollbacks.Load())
			require.EqualValues(t, 1, releases.Load())
			if failedCleanup == "" {
				require.NoError(t, err)
				require.Equal(t, want, got)
				require.Equal(t, wantCollector, collector, "fallback owns the full collector exactly once")
			} else {
				require.Error(t, err, "failed savepoint recovery must fail closed")
				require.Zero(t, got)
				require.Empty(t, collector.completeNodes)
				require.Empty(t, collector.annRows)
			}
		})
	}
}

func TestSemanticSearchSourceSingletonBackendAndCancellation(t *testing.T) {
	t.Parallel()
	for _, cancelQuery := range []bool{false, true} {
		s, _, records, _ := newSemanticPrefixFixture(t, document.EmbeddingInputOriginalFile, 2, false)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var sqliteFunctions, savepoints atomic.Int64
		installSingletonAuthorizer(t, s, func(action int, a, b, c string) int {
			if action == sqlite3.SQLITE_SAVEPOINT {
				savepoints.Add(1)
			}
			if action == sqlite3.SQLITE_FUNCTION && b == "typeof" {
				sqliteFunctions.Add(1)
				if cancelQuery {
					cancel()
				}
				return sqlite3.SQLITE_DENY
			}
			return sqlite3.SQLITE_OK
		})
		var got VectorIndexSource
		err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
			if !cancelQuery {
				// Exercise the Store-owned backend gate before any SQLite-only SQL.
				s.postgresSchema = "synthetic-postgres-routing-check"
				defer func() { s.postgresSchema = "" }()
			}
			var err error
			got, err = s.captureSemanticSearchSourceTx(ctx, tx, records[0].VectorSpace.ID, nil)
			return err
		})
		installSingletonAuthorizer(t, s, nil)
		if cancelQuery {
			require.ErrorIs(t, err, context.Canceled)
			require.Zero(t, got)
			require.Positive(t, sqliteFunctions.Load())
		} else {
			require.NoError(t, err)
			require.Len(t, got.Members, 2)
			require.Zero(t, sqliteFunctions.Load(), "PostgreSQL delegates before speculative SQLite SQL")
			require.Zero(t, savepoints.Load(), "PostgreSQL never enters speculative savepoint recovery")
		}
	}
}
