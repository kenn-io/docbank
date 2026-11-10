package processing

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
	docsqlite "go.kenn.io/docbank/sqlite"
)

func BenchmarkEmbeddingWorkerIdleScan(b *testing.B) {
	for _, n := range []int{1000, 4000, 16000} {
		for _, changed := range []bool{false, true} {
			mode := "idle"
			if changed {
				mode = "post-write"
			}
			b.Run(fmt.Sprintf("%s/generations=%d", mode, n), func(b *testing.B) {
				f, _, worker, request := newRealEmbeddingWorker(b, document.EmbeddingInputOriginalFile)
				db, err := store.DefaultSQLiteDriver().Open(f.databasePath, docsqlite.OpenOptions{Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate})
				require.NoError(b, err)
				b.Cleanup(func() { require.NoError(b, db.Close()) })
				// These generation copies only exist to scale the sweep.
				for _, query := range []string{
					`INSERT INTO embedding_input_generations SELECT printf('%064x',seq.i+100000000),generation_blob_hash,generation_encoded_size,generation_checksum,source_version_id,profile_fingerprint,evidence_fingerprint,tokenizer_fingerprint,chunk_policy_fingerprint,formatter_fingerprint,attachment_context_fingerprint,attachment_id,input_count,created_at FROM embedding_input_generations g,seq WHERE g.generation_id=?`,
					`INSERT INTO embedding_generation_inputs SELECT printf('%064x',seq.i+100000000),input_id,input_order,rendered_checksum FROM embedding_generation_inputs g,seq WHERE g.generation_id=?`,
				} {
					_, err = db.ExecContext(b.Context(), `WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM seq WHERE i<?) `+query, n-1, request.InputGeneration.ID)
					require.NoError(b, err)
				}
				catalog := &countedReconcileCatalog{Store: f.catalog, pauseClaims: true}
				worker.catalog = catalog
				now := time.Now().UTC()
				worker.clock = func() time.Time { return now }
				pass := func() {
					for {
						_, err := worker.ScanOnce(b.Context())
						require.NoError(b, err)
						if worker.reconcileAfter == "" && worker.imageAfter == (store.ImageReconcilePosition{}) {
							break
						}
					}
				}
				pass()
				// Retain terminal jobs so timed scans exercise discovery and empty claims.
				_, err = db.ExecContext(b.Context(), `UPDATE embedding_jobs SET state='failed',failure_code='input_rejected'`)
				require.NoError(b, err)
				catalog.pauseClaims = false
				now = now.Add(time.Minute)
				pass()
				require.Equal(b, 2*n, catalog.examined)
				b.ReportAllocs()
				for b.Loop() {
					if changed {
						b.StopTimer()
						// A real committed write invalidates the idle gate.
						_, err := f.catalog.Mkdir(b.Context(), f.catalog.RootID(), fmt.Sprintf("write-%d", now.Unix()))
						require.NoError(b, err)
						b.StartTimer()
					}
					now = now.Add(time.Minute)
					catalog.examined = 0
					pass()
					if changed {
						require.Equal(b, n, catalog.examined)
					} else {
						require.Zero(b, catalog.examined)
					}
				}
				if changed {
					b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(n), "ns/generation")
				}
			})
		}
	}
}
