//go:build cgo

package store

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

// Intercept the owning connection without a production hook. Cancellation at
// INSERT and a later coverage failure must both abort the atomic acquisition.
func TestSemanticSearchAuthorityHeadAtomicRollback(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"cancel at lease insert", "coverage failure", "stale before coverage"} {
		t.Run(mode, func(t *testing.T) {
			s, profile, records, _ := newSemanticPrefixFixture(t, document.EmbeddingInputOriginalFile, 1, false)
			source, err := s.CaptureVectorIndexSource(t.Context(), records[0].VectorSpace.ID)
			require.NoError(t, err)
			now := time.Now().UTC()
			stored := vectorIndexGenerationFixture("atomic-rollback", source, []byte("synthetic-index"), now)
			require.NoError(t, putVectorIndexGenerationForTest(t, s, stored))
			if mode == "stale before coverage" {
				other := hashVectorIndexTest("atomic-stale-before-coverage")
				_, err := s.db.Exec(`UPDATE vector_index_generations SET source_manifest_checksum=? WHERE generation_id=?`, other, stored.ID)
				require.NoError(t, err)
				_, err = s.db.Exec(`UPDATE vector_index_heads SET source_manifest_checksum=? WHERE vector_space_id=?`, other, source.VectorSpaceID)
				require.NoError(t, err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var insertReached, coverageRejected atomic.Bool
			install := func(enabled bool) {
				conn, err := s.writeDB.Conn(context.Background())
				require.NoError(t, err)
				defer func() { require.NoError(t, conn.Close()) }()
				require.NoError(t, conn.Raw(func(raw any) error {
					actual, ok := raw.(*sqlite3.SQLiteConn)
					require.True(t, ok)
					if !enabled {
						actual.RegisterAuthorizer(nil)
						return nil
					}
					actual.RegisterAuthorizer(func(action int, a, b, c string) int {
						if action == sqlite3.SQLITE_INSERT && a == "vector_index_reader_leases" {
							insertReached.Store(true)
							if mode == "cancel at lease insert" {
								cancel()
							}
						}
						// Reject the population COUNT after insertion. For stale
						// source, reject any premature coverage read as well.
						if action == sqlite3.SQLITE_FUNCTION && b == "count" && (insertReached.Load() || mode == "stale before coverage") {
							coverageRejected.Store(true)
							return sqlite3.SQLITE_DENY
						}
						return sqlite3.SQLITE_OK
					})
					return nil
				}))
			}
			install(true)
			defer install(false)
			authority, err := s.AcquireSemanticSearchAuthorityHead(ctx, profile.Fingerprint, records[0].BindingID, "atomic-rollback", now, time.Minute, SearchOptions{})
			install(false)
			require.Error(t, err)
			require.Zero(t, authority, "a rolled back acquisition must return no partial authority")
			switch mode {
			case "cancel at lease insert":
				require.True(t, insertReached.Load(), "cancellation reaches the real lease insertion boundary")
				require.ErrorIs(t, err, context.Canceled)
			case "coverage failure":
				require.True(t, insertReached.Load())
				require.True(t, coverageRejected.Load(), "coverage failed after lease insertion")
			case "stale before coverage":
				require.ErrorIs(t, err, ErrVectorIndexSourceStale)
				require.False(t, insertReached.Load(), "source mismatch precedes insertion")
				require.False(t, coverageRejected.Load(), "source mismatch precedes coverage errors")
			}
			var leases int
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM vector_index_reader_leases`).Scan(&leases))
			require.Zero(t, leases, "atomic rollback removes any inserted lease")
		})
	}
}
