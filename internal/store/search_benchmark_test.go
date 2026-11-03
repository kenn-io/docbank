package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
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
// checks. Each original-file set has one exact source input; 100 files share each blob.
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
				_, err := tx.ExecContext(ctx, `CREATE TEMP TABLE bench_members(n INTEGER,hash TEXT,version TEXT,generation TEXT,set_id TEXT,vectors TEXT,payload_hash TEXT,payload_size INTEGER,manifest TEXT)`)
				if err != nil {
					return err
				}
				var samples []EmbeddingSetRecord
				insert, err := tx.PrepareContext(ctx, `INSERT INTO bench_members VALUES (?,?,?,?,?,?,?,?,?)`)
				if err != nil {
					return err
				}
				defer func() { require.NoError(b, insert.Close()) }()
				for i := 1; i < count; i++ {
					version := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
					hash := fmt.Sprintf("%064x", (i-1)/100+1)
					vectors, err := document.NewVectorSetV1(document.VectorSetV1Input{
						VectorSpaceFingerprint: record.VectorSpace.ID, Metric: document.VectorMetricCosine,
						Normalization: document.VectorNormalizationNone, Dimension: 8,
						InputKeys: []string{version}, InputChecksums: []string{hash}, Values: [][]float64{{1, 2, 3, 4, 5, 6, 7, 8}},
					})
					if err != nil {
						return err
					}
					payload, checksum, err := document.EncodeVectorSetV1(vectors)
					if err != nil {
						return err
					}
					if i == 1 || i == count-1 {
						sample := cloneEmbeddingSetRecord(record)
						sample.ID = testSHA256([]byte(version + "set"))
						sample.ContentVersionID = version
						sample.InputGeneration.ID = testSHA256([]byte(version + "generation"))
						sample.InputGeneration.SourceVersionID = version
						sample.InputGeneration.GenerationChecksum = hash
						sample.InputGeneration.Inputs = []EmbeddingInputReference{{ID: version, RenderedChecksum: hash}}
						sample.VectorSet = EmbeddingVectorSetRecord{ID: checksum, ContractVersion: record.VectorSet.ContractVersion,
							VectorSpaceID: record.VectorSpace.ID, PayloadBlobHash: testSHA256(payload), PayloadChecksum: checksum, Payload: payload}
						samples = append(samples, sample)
					}
					manifest := embeddingVectorManifestChecksum([]EmbeddingVectorRowRecord{{RowID: version, InputID: version, Dimensions: 8, Checksum: hash}})
					if _, err := insert.ExecContext(ctx, i, hash, version, testSHA256([]byte(version+"generation")), testSHA256([]byte(version+"set")), checksum, testSHA256(payload), len(payload), manifest); err != nil {
						return err
					}
				}
				statements := []string{
					`INSERT INTO blobs(hash,size,created_at) SELECT DISTINCT hash,20,'2026-01-01' FROM bench_members`,
					`INSERT INTO blobs(hash,size,created_at) SELECT payload_hash,payload_size,'2026-01-01' FROM bench_members`,
					fmt.Sprintf(`INSERT INTO nodes(id,parent_id,name,kind,current_version_id,created_at,modified_at)
      SELECT n+100,%d,version,'file',version,'2026-01-01',CASE WHEN n>%d THEN '2026-02-01' ELSE '2026-01-01' END FROM bench_members`, s.RootID(), count*99/100),
					`INSERT INTO content_versions(version_id,node_id,blob_hash,size,mime_type,recorded_at,node_revision,introduced_operation_id,transition_kind)
      SELECT version,n+100,hash,20,'text/plain','2026-01-01',1,version,'content_create' FROM bench_members`,
					`INSERT INTO embedding_input_generations SELECT generation,NULL,0,hash,version,e.profile_fingerprint,e.evidence_fingerprint,e.tokenizer_fingerprint,e.chunk_policy_fingerprint,e.formatter_fingerprint,e.attachment_context_fingerprint,NULL,1,e.created_at FROM bench_members CROSS JOIN embedding_input_generations e WHERE e.generation_id='` + record.InputGeneration.ID + `'`,
					`INSERT INTO embedding_generation_inputs SELECT generation,version,0,hash FROM bench_members`,
					`INSERT INTO embedding_vector_sets SELECT vectors,e.contract_version,e.vector_space_id,payload_hash,b.payload_size,vectors,manifest,1,e.dimensions FROM bench_members b CROSS JOIN embedding_vector_sets e WHERE e.vector_set_id='` + record.VectorSet.ID + `'`,
					`INSERT INTO embedding_vector_rows SELECT vectors,version,0,version,8,hash FROM bench_members`,
					`INSERT INTO embedding_sets SELECT set_id,e.vault_uid,e.binding_id,e.input_kind,version,e.profile_fingerprint,e.embedding_input_fingerprint,e.vector_space_id,generation,vectors,e.created_at FROM bench_members CROSS JOIN embedding_sets e WHERE e.embedding_set_id='` + record.ID + `'`,
					`INSERT INTO embedding_heads SELECT version,e.binding_id,e.input_kind,set_id,e.vector_space_id,e.profile_fingerprint,e.published_at,e.fencing_token FROM bench_members CROSS JOIN embedding_heads e WHERE e.embedding_set_id='` + record.ID + `'`,
					`DROP TABLE bench_members`,
					`UPDATE nodes SET modified_at='2026-01-01' WHERE name='seed.txt'`,
				}
				for _, statement := range statements {
					if _, err := tx.ExecContext(ctx, statement); err != nil {
						return err
					}
				}
				for _, sample := range samples {
					sample, err := normalizeEmbeddingSetRecord(sample)
					if err != nil {
						return err
					}
					if err := validateEmbeddingSetRecord(sample); err != nil {
						return err
					}
					if err := validateEmbeddingSetFencesTx(ctx, tx, sample); err != nil {
						return err
					}
				}
				return nil
			}))
			// Exercise the same loader as production source capture before timing membership work.
			source, err := s.CaptureVectorIndexSource(ctx, record.VectorSpace.ID)
			require.NoError(b, err)
			require.Len(b, source.Members, count)
			neighbors := make([]vectorindex.Neighbor, count-1)
			rows, err := s.db.QueryContext(ctx, `SELECT es.vector_set_id,es.content_version_id,cv.blob_hash FROM embedding_sets es JOIN content_versions cv ON cv.version_id=es.content_version_id WHERE es.embedding_set_id<>? ORDER BY cv.version_id`, record.ID)
			require.NoError(b, err)
			defer func() { require.NoError(b, rows.Close()) }()
			for i := 0; rows.Next(); i++ {
				require.NoError(b, rows.Scan(&neighbors[i].SetID, &neighbors[i].InputKey, &neighbors[i].InputChecksum))
			}
			require.NoError(b, rows.Err())

			tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			require.NoError(b, err)
			defer func() { require.NoError(b, tx.Rollback()) }()
			for _, scope := range []string{"all", "narrow", "collapse"} {
				b.Run(scope, func(b *testing.B) {
					opts := SearchOptions{}
					ranked := append([]vectorindex.Neighbor(nil), neighbors...)
					wantRows := count
					wantSimilar, similarTruncated := 20, true
					switch scope {
					case "all":
						// Interleave copies so an ordinary page starts with distinct groups.
						ranked = ranked[:0]
						for copy := range 100 {
							for group := range count / 100 {
								if index := group*100 + copy; index < len(neighbors) {
									ranked = append(ranked, neighbors[index])
								}
							}
						}
					case "narrow":
						opts.ModifiedSince = "2026-02-01"
						wantRows = count/100 - 1
						wantSimilar, similarTruncated = min(20, count/10000), count/10000 > 20
					case "collapse":
						lexical := query.Query{Filters: query.Filters{CollapseDuplicates: true}}
						opts.PhotoScope, opts.PhotoLexicalQuery = &PhotoBrowseRequest{}, &lexical
						wantRows = count/100 + 1
					}
					allowed, err := semanticANNRowsTx(ctx, tx, profile.Fingerprint, record.BindingID, record.InputKind, record.VectorSpace.ID, opts)
					require.NoError(b, err)
					require.Len(b, allowed, wantRows)
					allowlist := make(map[vectorindex.RowIdentity]struct{}, len(allowed))
					for _, row := range allowed {
						allowlist[row] = struct{}{}
					}
					ranked = slices.DeleteFunc(ranked, func(neighbor vectorindex.Neighbor) bool {
						_, included := allowlist[vectorindex.RowIdentity{SetID: neighbor.SetID, InputKey: neighbor.InputKey, InputChecksum: neighbor.InputChecksum}]
						return !included
					})
					for i := range ranked {
						ranked[i].Score = 1 - float64(i)/float64(count)
					}
					for _, operation := range []string{"authority", "resolve", "similar"} {
						if scope == "collapse" && operation == "similar" {
							continue
						}
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
									rows, err := semanticANNRowsTx(ctx, tx, profile.Fingerprint, record.BindingID, record.InputKind, record.VectorSpace.ID, opts)
									require.NoError(b, err)
									require.Len(b, rows, wantRows)
								case "resolve":
									var result SemanticSearchResolution
									require.NoError(b, s.semanticCandidatesTx(ctx, tx, profile.Fingerprint, record.BindingID, record.InputKind, record.VectorSpace.ID, ranked, 20, opts, nil, &result))
									require.Len(b, result.Candidates, 20)
									require.True(b, result.Truncated)
								case "similar":
									var result SimilarSearchResolution
									require.NoError(b, s.similarCandidatesTx(ctx, tx, profile.Fingerprint, record.BindingID, record.InputKind, record.VectorSpace.ID, ranked, 20, opts, SimilarSource{NodeID: node.ID}, &result))
									require.Len(b, result.Candidates, wantSimilar)
									require.Equal(b, similarTruncated, result.Truncated)
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
		})
	}
}
