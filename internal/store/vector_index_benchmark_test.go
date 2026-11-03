package store

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/vectorindex"
)

// Measure file ingestion while searches acquire, resolve, and release a generation.
func BenchmarkSemanticSearchIngestLatency(b *testing.B) {
	for _, members := range []int{1000, 4000, 16000} {
		b.Run(fmt.Sprintf("members=%d", members), func(b *testing.B) {
			s, first, _ := newPublishedVectorIndexFixture(b)
			publishVectorIndexMembersForTest(b, s, first, members)
			source, err := s.CaptureVectorIndexSource(b.Context(), first.VectorSpace.ID)
			require.NoError(b, err)
			require.Len(b, source.Members, members)
			require.NoError(b, putVectorIndexGenerationForTest(b, s, vectorIndexGenerationFixture("benchmark", source, make([]byte, members*1536), time.Now().UTC())))
			ctx, cancel := context.WithCancel(b.Context())
			done := make(chan error, 1)
			started := make(chan struct{})
			var searches atomic.Int64
			go func() {
				for ctx.Err() == nil {
					now := time.Now().UTC()
					lease, err := s.AcquireVectorIndexGeneration(ctx, source.VectorSpaceID, "benchmark", now, time.Minute)
					if err != nil {
						done <- err
						return
					}
					_, searchErr := s.ResolveSemanticCandidates(ctx, first.ProcessingProfileFingerprint, first.BindingID, first.InputKind, source.VectorSpaceID, source.ManifestChecksum,
						[]vectorindex.Neighbor{{SetID: first.VectorSet.ID, InputKey: first.InputGeneration.Inputs[0].ID, InputChecksum: first.InputGeneration.Inputs[0].RenderedChecksum, Score: 0.8}}, 10, SearchOptions{}, nil)
					// Release must finish even when the benchmark cancels a search.
					cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
					err = s.ReleaseVectorIndexGeneration(cleanupCtx, lease.ID, lease.FencingToken, time.Now().UTC())
					cleanupCancel()
					if err != nil {
						done <- err
						return
					}
					if searchErr != nil {
						done <- searchErr
						return
					}
					if searches.Add(1) == 1 {
						close(started)
					}
				}
				done <- ctx.Err()
			}()
			defer func() {
				cancel()
				err := <-done
				if err != nil && !errors.Is(err, context.Canceled) {
					b.Error(err)
				}
			}()
			select {
			case <-started:
			case err := <-done:
				// Leave the result available for the deferred join.
				done <- err
				b.Fatal(err)
			}
			before := searches.Load()
			i := 0
			for b.Loop() {
				_, err := s.CreateFile(b.Context(), s.RootID(), fmt.Sprintf("ingest-%09d.bin", i), fmt.Sprintf("%064x", members+i+1), 8, "application/octet-stream")
				require.NoError(b, err)
				i++
			}
			completed := searches.Load() - before
			b.ReportMetric(float64(completed)/b.Elapsed().Seconds(), "searches/s")
		})
	}
}

func publishVectorIndexMembersForTest(tb testing.TB, s *Store, first EmbeddingSetRecord, members int) {
	tb.Helper()
	for i := 1; i < members; i++ {
		node, err := s.CreateFile(tb.Context(), s.RootID(), fmt.Sprintf("member-%06d.pdf", i), fmt.Sprintf("%064x", i), 20, "application/pdf")
		require.NoError(tb, err)
		record := embeddingSetFixture(tb.Context(), s, node.CurrentVersionID, first.ProcessingProfileFingerprint, first.InputKind, first.BindingID, "")
		require.NoError(tb, s.StageEmbeddingSet(tb.Context(), record))
		require.NoError(tb, s.PublishEmbeddingHead(tb.Context(), EmbeddingHeadRecord{
			FencingToken: 1, Key: EmbeddingHeadKey{ContentVersionID: node.CurrentVersionID, BindingID: record.BindingID, InputKind: record.InputKind},
			SetID: record.ID, VectorSpaceID: record.VectorSpace.ID, ProcessingProfileFingerprint: record.ProcessingProfileFingerprint, PublishedAt: embeddingCatalogTime,
		}))
	}
}

// ResolveSemanticCandidates includes the source fence and downstream eligibility
// work, so accidentally recapturing during search is included in these timings.
func BenchmarkVectorIndexSourceCapture(b *testing.B) {
	for _, heads := range []int{1000, 4000, 16000, 64000} {
		b.Run(fmt.Sprintf("heads=%d", heads), func(b *testing.B) {
			s, first, _ := newPublishedVectorIndexFixture(b)
			publishVectorIndexMembersForTest(b, s, first, heads)
			source, err := s.CaptureVectorIndexSource(b.Context(), first.VectorSpace.ID)
			require.NoError(b, err)
			now := time.Now().UTC()
			claim, claimed, err := s.ClaimVectorIndexBuild(b.Context(), source.VectorSpaceID, source.ManifestChecksum, "benchmark", now, time.Minute)
			require.NoError(b, err)
			require.True(b, claimed)
			generation := vectorIndexGenerationFixture("benchmark", source, []byte("index"), now)
			require.NoError(b, s.StageVectorIndexGeneration(b.Context(), claim, generation, now))
			require.NoError(b, s.PublishVectorIndexGeneration(b.Context(), claim, generation.ID, now))
			for _, verified := range []bool{true, false} {
				b.Run(fmt.Sprintf("verified=%t", verified), func(b *testing.B) {
					if !verified {
						_, err := s.db.ExecContext(b.Context(), `DELETE FROM vector_index_source_verifications`)
						require.NoError(b, err)
					}
					for b.Loop() {
						_, err := s.ResolveSemanticCandidates(b.Context(), first.ProcessingProfileFingerprint, first.BindingID, first.InputKind, source.VectorSpaceID, source.ManifestChecksum,
							[]vectorindex.Neighbor{{SetID: first.VectorSet.ID, InputKey: first.InputGeneration.Inputs[0].ID, InputChecksum: first.InputGeneration.Inputs[0].RenderedChecksum, Score: 0.8}}, 10, SearchOptions{}, nil)
						require.NoError(b, err)
					}
				})
			}
		})
	}
}
