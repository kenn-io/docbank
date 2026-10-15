package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// BenchmarkSavedQueryRunReceipt excludes fixture creation. Receipt writes include
// validation and insertion, but roll back so every sample sees the same history.
func BenchmarkSavedQueryRunReceipt(b *testing.B) {
	for _, history := range []int{0, 1, 100, 1000} {
		for _, unrelated := range []int{0, 10000} {
			b.Run(fmt.Sprintf("runs=%d/unrelated=%d", history, unrelated), func(b *testing.B) {
				s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
				require.NoError(b, err)
				b.Cleanup(func() { require.NoError(b, s.Close()) })
				ctx := b.Context()
				definition, err := s.CreateSavedQuery(ctx, "Selected", "", SavedQueryKindQuery, []byte(`{}`))
				require.NoError(b, err)
				other, err := s.CreateSavedQuery(ctx, "Unrelated", "", SavedQueryKindQuery, []byte(`{}`))
				require.NoError(b, err)
				at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
				hash := fmt.Sprintf("%064x", 1)
				var head string
				require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
					stmt, err := tx.PrepareContext(ctx, `INSERT INTO saved_query_runs(
						run_id,saved_query_id,saved_query_revision,query_fingerprint,snapshot_id,
						member_hash,total,total_bytes,ran_at,expires_at,previous_run_id,
						previous_member_hash,previous_total,previous_query_fingerprint
					) VALUES(?,?,1,?,?,?,0,0,?,?,?,?,?,?)`)
					if err != nil {
						return err
					}
					defer func() { _ = stmt.Close() }()
					for index, count := range []int{history, unrelated} {
						saved := []SavedQuery{definition, other}[index]
						var previous, previousHash, previousFingerprint any
						var previousTotal any
						for i := range count {
							id := fmt.Sprintf("%08x-1111-4111-8111-%012x", index+1, i+1)
							ranAt := at.Add(time.Duration(i) * time.Second)
							_, err := stmt.ExecContext(ctx, id, saved.ID, saved.Fingerprint,
								fmt.Sprintf("%032x", i+1), hash, formatSavedQueryRunTime(ranAt),
								formatSavedQueryRunTime(ranAt.Add(time.Minute)),
								previous, previousHash, previousTotal, previousFingerprint)
							if err != nil {
								return err
							}
							previous, previousHash, previousTotal, previousFingerprint = id, hash, 0, saved.Fingerprint
							if index == 0 {
								head = id
							}
						}
					}
					return nil
				}))
				require.NoError(b, validateSavedQueryRunMetadataState(ctx, s.db, currentMetadataLayout()))
				b.Run("receipt", func(b *testing.B) {
					projection := SnapshotProjection{QueryFingerprint: definition.Fingerprint, MemberHash: hash}
					page := SnapshotPage{
						SnapshotID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
						CreatedAt:  at.Add(24 * time.Hour), ExpiresAt: at.Add(25 * time.Hour),
					}
					b.ReportAllocs()
					for b.Loop() {
						tx, err := s.writeDB.BeginTx(ctx, nil)
						if err != nil {
							b.Fatal(err)
						}
						run, err := insertSavedQueryRunTx(ctx, tx, definition.ID, definition.Revision, projection, page)
						rollbackErr := tx.Rollback()
						if err != nil || rollbackErr != nil || run.PreviousRunID != head {
							b.Fatalf("previous=%q err=%v rollback=%v", run.PreviousRunID, err, rollbackErr)
						}
					}
				})
			})
		}
	}
}
