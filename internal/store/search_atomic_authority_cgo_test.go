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
	for _, mode := range []string{"cancel at lease insert", "authority query failure", "stale before lease"} {
		t.Run(mode, func(t *testing.T) {
			s, profile, records, _ := newSemanticPrefixFixture(t, document.EmbeddingInputOriginalFile, 1)
			opts := SearchOptions{}
			if mode == "authority query failure" {
				tag, err := s.CreateTag(t.Context(), "atomic-authority-failure")
				require.NoError(t, err)
				version, err := s.ContentVersionByID(t.Context(), records[0].ContentVersionID)
				require.NoError(t, err)
				_, err = s.AssignTag(t.Context(), tag.ID, version.NodeID, UnconditionalRev)
				require.NoError(t, err)
				opts.TagID = tag.ID
			}
			source, err := s.CaptureVectorIndexSource(t.Context(), records[0].VectorSpace.ID)
			require.NoError(t, err)
			now := time.Now().UTC()
			stored := vectorIndexGenerationFixture("atomic-rollback", source, []byte("synthetic-index"), now)
			require.NoError(t, putVectorIndexGenerationForTest(t, s, stored))
			if mode == "stale before lease" {
				other := hashVectorIndexTest("atomic-stale-before-coverage")
				_, err := s.db.ExecContext(t.Context(), `UPDATE vector_index_generations SET source_manifest_checksum=? WHERE generation_id=?`, other, stored.ID)
				require.NoError(t, err)
				_, err = s.db.ExecContext(t.Context(), `UPDATE vector_index_heads SET source_manifest_checksum=? WHERE vector_space_id=?`, other, source.VectorSpaceID)
				require.NoError(t, err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var insertReached, authorityRejected atomic.Bool
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
						// The filtered fallback reads fresh embedding authority after
						// lease acquisition. Reject that read to exercise transaction rollback.
						if mode == "authority query failure" && action == sqlite3.SQLITE_READ && a == "embedding_heads" && insertReached.Load() {
							authorityRejected.Store(true)
							return sqlite3.SQLITE_DENY
						}
						return sqlite3.SQLITE_OK
					})
					return nil
				}))
			}
			install(true)
			defer install(false)
			authority, err := s.AcquireSemanticSearchAuthorityHead(ctx, profile.Fingerprint, records[0].BindingID, "atomic-rollback", now, time.Minute, opts)
			install(false)
			require.Error(t, err)
			require.Zero(t, authority, "a rolled back acquisition must return no partial authority")
			switch mode {
			case "cancel at lease insert":
				require.True(t, insertReached.Load(), "cancellation reaches the real lease insertion boundary")
				require.ErrorIs(t, err, context.Canceled)
			case "authority query failure":
				require.True(t, insertReached.Load())
				require.True(t, authorityRejected.Load(), "fresh ANN authority failed after lease insertion")
			case "stale before lease":
				require.ErrorIs(t, err, ErrVectorIndexSourceStale)
				require.False(t, insertReached.Load(), "source mismatch precedes insertion")
				require.False(t, authorityRejected.Load(), "source mismatch precedes ANN authority reads")
			}
			var leases int
			require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM vector_index_reader_leases`).Scan(&leases))
			require.Zero(t, leases, "atomic rollback removes any inserted lease")
		})
	}
}
