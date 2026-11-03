package store

import (
	"fmt"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkDeleteBlobRows grows retained references while deleting a fixed batch.
func BenchmarkDeleteBlobRows(b *testing.B) {
	const batch = 100
	for _, retained := range []int{1000, 4000, 16000, 64000} {
		b.Run(fmt.Sprintf("retained=%d", retained), func(b *testing.B) {
			ctx := b.Context()
			s, err := Open(ctx, filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			conn, err := s.writeDB.Conn(ctx)
			require.NoError(b, err)
			defer func() { _ = conn.Close() }()
			_, err = conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`)
			require.NoError(b, err)
			tx, err := conn.BeginTx(ctx, nil)
			require.NoError(b, err)
			for i := range retained {
				hash := fmt.Sprintf("%064x", i+1)
				_, err = tx.ExecContext(ctx, `INSERT INTO blobs(hash, size, created_at) VALUES(?, 1, '2026-01-01T00:00:00Z')`, hash)
				require.NoError(b, err)
				_, err = tx.ExecContext(ctx, `INSERT INTO export_role_roots(plan_id, blob_hash) VALUES(?, ?)`, strconv.Itoa(i%50), hash)
				require.NoError(b, err)
				_, err = tx.ExecContext(ctx, `INSERT INTO rendition_jobs(
					job_id, vault_uid, source_sha256, rendition_request_fingerprint,
					evidence_lexical_fingerprint, captured_artifact_policy_fingerprint,
					execution_identity_fingerprint, execution_identity_json, captured_artifact_policy_json,
					state, phase, available_at, created_at, updated_at
				) VALUES(?, ?, ?, '', '', '', '', '{}', '{}', 'queued', 'queued', '', '', '')`, hash, s.VaultID(), hash)
				require.NoError(b, err)
			}
			require.NoError(b, tx.Commit())
			_, err = conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`)
			require.NoError(b, err)
			require.NoError(b, conn.Close())
			require.NoError(b, s.Checkpoint(ctx))
			hashes := make([]string, batch)
			for i := range hashes {
				hashes[i] = fmt.Sprintf("%064x", retained+i+1)
			}
			for b.Loop() {
				b.StopTimer()
				tx, err := s.writeDB.BeginTx(ctx, nil)
				require.NoError(b, err)
				for _, hash := range hashes {
					_, err = tx.ExecContext(ctx, `INSERT INTO blobs(hash, size, created_at) VALUES(?, 1, '2026-01-01T00:00:00Z')`, hash)
					require.NoError(b, err)
				}
				require.NoError(b, tx.Commit())
				b.StartTimer()
				require.NoError(b, s.DeleteBlobRows(ctx, hashes))
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*batch), "ns/blob")
		})
	}
}
