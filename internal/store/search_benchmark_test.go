package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/vectorindex"
)

// Include the complete final validity check and path refresh for a result page.
func BenchmarkSearchRevalidation(b *testing.B) {
	for _, depth := range []int{0, 16} {
		b.Run(fmt.Sprintf("depth=%d", depth), func(b *testing.B) {
			s, err := Open(b.Context(), filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			ctx := b.Context()
			prefix := strings.Repeat("/folder", depth)
			parent, err := s.MkdirAll(ctx, prefix)
			require.NoError(b, err)
			candidates := make([]SearchCandidateIdentity, 1000)
			paths := make([]string, len(candidates))
			require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				for i := range candidates {
					name := fmt.Sprintf("report-%04d.txt", i)
					node, _, err := s.createFileTx(ctx, tx, parent.ID, name,
						fmt.Sprintf("%064x", i+1), 8, "text/plain")
					if err != nil {
						return err
					}
					candidates[i] = SearchCandidateIdentity{NodeID: node.ID, ContentVersionID: node.CurrentVersionID,
						Evidence: []SearchEvidenceIdentity{{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Kind: "node_name", NodeName: node.Name}}}
					paths[i] = prefix + "/" + name
				}
				return nil
			}))
			for _, count := range []int{1, 50, 1000} {
				b.Run(fmt.Sprintf("candidates=%d", count), func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						result, err := s.RevalidateSearchCandidates(ctx, candidates[:count], SearchOptions{}, "", "")
						if err != nil || len(result.Candidates) != count {
							b.Fatalf("candidates=%d err=%v", len(result.Candidates), err)
						}
						for i, candidate := range result.Candidates {
							if candidate.NodeID != candidates[i].NodeID || candidate.Path != paths[i] {
								b.Fatalf("candidate %d: node=%d path=%q", i, candidate.NodeID, candidate.Path)
							}
						}
					}
				})
			}
		})
	}
}

// BenchmarkNameSearch measures returning 50 ranked results as the number of
// matching filenames grows. Catalog creation is excluded from the timer.
func BenchmarkNameSearch(b *testing.B) {
	for _, files := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("matches=%d", files), func(b *testing.B) {
			s, err := Open(b.Context(), filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			for i := range files {
				_, err := s.CreateFile(b.Context(), s.RootID(), fmt.Sprintf("report-%06d.bin", i),
					fmt.Sprintf("%064x", i+1), 1024, "application/octet-stream")
				require.NoError(b, err)
			}
			b.Run("page", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					hits, truncated, err := s.SearchPage(b.Context(), "report", 50)
					require.NoError(b, err)
					require.Len(b, hits, 50)
					require.True(b, truncated)
				}
			})
			b.Run("explained", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					hits, truncated, err := s.SearchExplainedLexicalCandidates(b.Context(), "report", 50, SearchOptions{}, false)
					require.NoError(b, err)
					require.Len(b, hits, 50)
					require.True(b, truncated)
				}
			})
		})
	}
}

// BenchmarkSemanticMemberships isolates membership work after source and coverage
// checks. Each set has four inputs, and every document has distinct content.
func BenchmarkSemanticMemberships(b *testing.B) {
	for _, count := range []int{10_000, 100_000, 1_000_000} {
		b.Run(fmt.Sprintf("rows=%d", count), func(b *testing.B) {
			ctx := b.Context()
			s, err := Open(ctx, filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			profile := embeddingCatalogProfile(b)
			node, err := s.CreateFile(ctx, s.RootID(), "seed.txt", catalogSourceHash, 20, "text/plain")
			require.NoError(b, err)
			require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error { return ensureProcessingProfileTx(ctx, tx, profile) }))
			record := embeddingSetFixture(ctx, s, node.CurrentVersionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
			require.NoError(b, s.StageEmbeddingSet(ctx, record))
			require.NoError(b, s.PublishEmbeddingHead(ctx, EmbeddingHeadRecord{FencingToken: 1,
				Key:   EmbeddingHeadKey{ContentVersionID: node.CurrentVersionID, BindingID: record.BindingID, InputKind: record.InputKind},
				SetID: record.ID, VectorSpaceID: record.VectorSpace.ID, ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime}))
			require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				statements := []string{
					fmt.Sprintf(`CREATE TEMP TABLE bench_members AS WITH RECURSIVE i(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM i WHERE n<%d)
      SELECT n,printf('%%064x',n) hash,printf('version-%%d',n) version,printf('generation-%%d',n) generation,printf('set-%%d',n) set_id,printf('vectors-%%d',n) vectors FROM i`, count/4),
					`INSERT INTO blobs(hash,size,created_at) SELECT hash,20,'2026-01-01' FROM bench_members`,
					fmt.Sprintf(`INSERT INTO nodes(id,parent_id,name,kind,current_version_id,created_at,modified_at)
      SELECT n+100,%d,version,'file',version,'2026-01-01','2026-01-01' FROM bench_members`, s.RootID()),
					`INSERT INTO content_versions(version_id,node_id,blob_hash,size,mime_type,recorded_at,node_revision,introduced_operation_id,transition_kind)
      SELECT version,n+100,hash,20,'text/plain','2026-01-01',1,version,'content_create' FROM bench_members`,
					`INSERT INTO embedding_input_generations SELECT generation,NULL,0,e.generation_checksum,version,e.profile_fingerprint,e.evidence_fingerprint,e.tokenizer_fingerprint,e.chunk_policy_fingerprint,e.formatter_fingerprint,e.attachment_context_fingerprint,NULL,4,e.created_at FROM bench_members CROSS JOIN embedding_input_generations e WHERE e.generation_id='` + record.InputGeneration.ID + `'`,
					`INSERT INTO embedding_generation_inputs SELECT generation,printf('input-%d',j),j,hash FROM bench_members CROSS JOIN (SELECT 0 j UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3)`,
					`INSERT INTO embedding_vector_sets SELECT vectors,e.contract_version,e.vector_space_id,e.payload_blob_hash,e.payload_size,e.payload_checksum,e.manifest_checksum,4,e.dimensions FROM bench_members CROSS JOIN embedding_vector_sets e WHERE e.vector_set_id='` + record.VectorSet.ID + `'`,
					`INSERT INTO embedding_vector_rows SELECT vectors,printf('input-%d',j),j,printf('input-%d',j),8,hash FROM bench_members CROSS JOIN (SELECT 0 j UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3)`,
					`INSERT INTO embedding_sets SELECT set_id,e.vault_uid,e.binding_id,e.input_kind,version,e.profile_fingerprint,e.embedding_input_fingerprint,e.vector_space_id,generation,vectors,e.created_at FROM bench_members CROSS JOIN embedding_sets e WHERE e.embedding_set_id='` + record.ID + `'`,
					`INSERT INTO embedding_heads SELECT version,e.binding_id,e.input_kind,set_id,e.vector_space_id,e.profile_fingerprint,e.published_at,e.fencing_token FROM bench_members CROSS JOIN embedding_heads e WHERE e.embedding_set_id='` + record.ID + `'`,
				}
				for _, statement := range statements {
					if _, err := tx.ExecContext(ctx, statement); err != nil {
						return err
					}
				}
				return nil
			}))
			neighbors := make([]vectorindex.Neighbor, count)
			for i := range neighbors {
				neighbors[i] = vectorindex.Neighbor{SetID: fmt.Sprintf("vectors-%d", i/4+1), InputKey: fmt.Sprintf("input-%d", i%4), InputChecksum: fmt.Sprintf("%064x", i/4+1), Score: 1 - float64(i)/float64(count)}
			}
			tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			require.NoError(b, err)
			defer func() { require.NoError(b, tx.Rollback()) }()
			for _, operation := range []string{"authority", "resolve", "similar"} {
				b.Run(operation, func(b *testing.B) {
					b.ReportAllocs()
					b.StopTimer()
					var heap uint64
					for range b.N {
						runtime.GC()
						var start runtime.MemStats
						runtime.ReadMemStats(&start)
						// Sample live Go heap while the loaders own their intermediate maps.
						sampleCtx, cancel := context.WithCancel(ctx)
						sampled := make(chan uint64)
						go func() {
							ticker := time.NewTicker(time.Millisecond)
							defer ticker.Stop()
							peak := start.HeapAlloc
							for {
								var mem runtime.MemStats
								runtime.ReadMemStats(&mem)
								peak = max(peak, mem.HeapAlloc)
								select {
								case <-sampleCtx.Done():
									sampled <- peak - start.HeapAlloc
									return
								case <-ticker.C:
								}
							}
						}()
						b.StartTimer()
						switch operation {
						case "authority":
							rows, err := semanticANNRowsTx(ctx, tx, profile.Fingerprint, record.BindingID, record.InputKind, record.VectorSpace.ID, SearchOptions{})
							require.NoError(b, err)
							require.Len(b, rows, count+1)
						case "resolve":
							var result SemanticSearchResolution
							require.NoError(b, s.semanticCandidatesTx(ctx, tx, profile.Fingerprint, record.BindingID, record.InputKind, record.VectorSpace.ID, neighbors, 20, SearchOptions{}, nil, &result))
							require.Len(b, result.Candidates, 20)
							require.True(b, result.Truncated)
						case "similar":
							var result SimilarSearchResolution
							require.NoError(b, s.similarCandidatesTx(ctx, tx, profile.Fingerprint, record.BindingID, record.InputKind, record.VectorSpace.ID, neighbors, 20, SearchOptions{}, SimilarSource{NodeID: node.ID}, &result))
							require.Len(b, result.Candidates, 20)
							require.True(b, result.Truncated)
						}
						b.StopTimer()
						cancel()
						heap = max(heap, <-sampled)
					}
					b.ReportMetric(float64(heap), "heap-B")
				})
			}
		})
	}
}
