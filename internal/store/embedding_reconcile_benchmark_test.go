package store

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// BenchmarkEmbeddingReconcilePass measures a complete idle catalog sweep.
func BenchmarkEmbeddingReconcilePass(b *testing.B) {
	for _, n := range []int{1000, 4000, 16000} {
		b.Run(fmt.Sprintf("generations=%d", n), func(b *testing.B) {
			s, err := Open(b.Context(), filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			s, version, profile, _ := seedEmbeddingCatalog(b, s)
			request := embeddingJobTestRequest(b, s, version, profile, "benchmark")
			_, err = s.EnqueueEmbeddingJob(b.Context(), request)
			require.NoError(b, err)
			for _, table := range []string{"embedding_input_generations", "embedding_generation_inputs"} {
				var query string
				if table == "embedding_input_generations" {
					query = `INSERT INTO embedding_input_generations SELECT printf('%064x',seq.i+100000000),generation_blob_hash,generation_encoded_size,generation_checksum,source_version_id,profile_fingerprint,evidence_fingerprint,tokenizer_fingerprint,chunk_policy_fingerprint,formatter_fingerprint,attachment_context_fingerprint,attachment_id,input_count,created_at FROM embedding_input_generations g,seq WHERE g.generation_id=?`
				} else {
					query = `INSERT INTO embedding_generation_inputs SELECT printf('%064x',seq.i+100000000),input_id,input_order,rendered_checksum FROM embedding_generation_inputs g,seq WHERE g.generation_id=?`
				}
				_, err = s.writeDB.ExecContext(b.Context(), `WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM seq WHERE i<?) `+query, n-1, request.InputGeneration.ID)
				require.NoError(b, err)
			}
			pass := func(steady bool) {
				examined := 0
				for after := ""; ; {
					result, err := s.ReconcileEmbeddingJobs(b.Context(), EmbeddingReconcileRequest{Mutate: embeddingTestMutation, At: time.Now().UTC(), Limit: 100, After: after, SkipImages: true, DescriptorFingerprints: []string{request.Descriptor.Fingerprint}})
					require.NoError(b, err)
					if steady {
						require.Zero(b, result.Enqueued)
					}
					examined += result.Examined
					if result.Next == "" {
						break
					}
					after = result.Next
				}
				require.Equal(b, n, examined)
			}
			pass(false)
			b.ReportAllocs()
			for b.Loop() {
				pass(true)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(n), "ns/generation")
		})
	}
}
