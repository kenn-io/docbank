package retrieval

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorindex"
)

type similarBackendStub struct {
	*retrievalBackendStub

	sources    []vectorindex.RowIdentity
	resolveErr error
	scores     []float64
}

func (b *similarBackendStub) AcquireSimilarSearchAuthority(ctx context.Context, profile, binding, owner string, at time.Time, duration time.Duration, opts store.SearchOptions, _ store.SimilarSource) (store.SimilarSearchAuthority, error) {
	authority, err := b.AcquireSemanticSearchAuthority(ctx, profile, binding, owner, at, duration, opts)
	return store.SimilarSearchAuthority{SemanticSearchAuthority: authority, SourceRows: b.sources}, err
}

func (b *similarBackendStub) ResolveSimilarCandidates(_ context.Context, _, _ string, _ document.EmbeddingInputKind, _, manifest string, neighbors []vectorindex.Neighbor, _ int, _ store.SearchOptions, _ store.SimilarSource) (store.SimilarSearchResolution, error) {
	for _, neighbor := range neighbors {
		b.scores = append(b.scores, neighbor.Score)
	}
	return store.SimilarSearchResolution{SourceManifestChecksum: manifest, ScopedDocuments: 2, CompleteDocuments: 2}, b.resolveErr
}

type forbiddenSimilarEncoder struct{ t *testing.T }

type scoringContext struct {
	context.Context

	remaining int
	scoring   chan struct{}
}

func (ctx *scoringContext) Err() error {
	ctx.remaining--
	if ctx.remaining == 0 {
		close(ctx.scoring)
		<-ctx.Done()
	}
	return ctx.Context.Err()
}

func (p forbiddenSimilarEncoder) ResolveQueryEncoder(context.Context, document.EmbeddingDescriptor) (document.EmbeddingProvider, error) {
	p.t.Fatal("similar search invoked a query encoder")
	return nil, errors.New("forbidden encoder")
}

func TestSimilarStoredMetricsNoEncoderAndLeaseCleanup(t *testing.T) {
	for _, metric := range []string{document.VectorMetricCosine, document.VectorMetricDotProduct, document.VectorMetricL2} {
		t.Run(metric, func(t *testing.T) {
			searcher, backend, provider, descriptor := retrievalSearcherFixture(t, true, 2)
			descriptor.Metric, descriptor.Normalization = metric, document.VectorNormalizationNone
			set := document.VectorSetV1{VectorSpaceFingerprint: strings.Repeat("b", 64), Metric: metric, Normalization: descriptor.Normalization, Dimension: 2,
				InputKeys: []string{"source", "near", "far"}, InputChecksums: []string{strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64)}, Vectors: [][]float32{{1, 0}, {1, 1}, {-1, 0}}}
			_, id, err := document.EncodeVectorSetV1(set)
			require.NoError(t, err)
			manifest, err := vectorindex.NewManifest([]string{id})
			require.NoError(t, err)
			generation, err := vectorindex.BuildGeneration(manifest, []document.VectorSetV1{set}, vectorindex.Options{})
			require.NoError(t, err)
			backend.authority.VectorSpace.Descriptor = descriptor
			backend.authority.Lease.Generation.Bytes = generation.Bytes()
			backend.authority.Lease.Generation.IndexManifestChecksum = manifest.Checksum
			backend.authority.ANNRows = []vectorindex.RowIdentity{{SetID: id, InputKey: "near", InputChecksum: set.InputChecksums[1]}, {SetID: id, InputKey: "far", InputChecksum: set.InputChecksums[2]}}
			b := &similarBackendStub{retrievalBackendStub: backend, sources: []vectorindex.RowIdentity{{SetID: id, InputKey: "source", InputChecksum: set.InputChecksums[0]}}}
			searcher.backend, searcher.encoders = b, forbiddenSimilarEncoder{t: t}
			query := SimilarQuery{Source: DocumentIdentity{VaultID: "vault", NodeID: 1, ContentVersionID: "source-version"}, Limit: 20}
			_, err = searcher.Similar(t.Context(), query)
			require.NoError(t, err)
			assert.Zero(t, provider.calls, "provider_calls=0")
			t.Log("provider_calls=0; stored source rows scored without encoder or consent")
			assert.False(t, backend.releasedAt.IsZero())
			if metric == document.VectorMetricL2 {
				assert.Equal(t, []float64{-1, -2}, b.scores)
			}
			t.Run("cancel_during_scoring", func(t *testing.T) {
				backend.releasedAt = time.Time{}
				b.scores = nil
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				// One precheck, six selection checks, then one check per source row.
				scoring := &scoringContext{Context: ctx, remaining: 8, scoring: make(chan struct{})}
				done := make(chan error, 1)
				go func() {
					_, err := searcher.Similar(scoring, query)
					done <- err
				}()
				select {
				case <-scoring.scoring:
				case err := <-done:
					t.Fatalf("returned before reaching candidate scoring: %v", err)
				case <-time.After(time.Second):
					t.Fatal("did not reach candidate scoring")
				}
				cancel()
				select {
				case err := <-done:
					require.ErrorIs(t, err, context.Canceled)
				case <-time.After(time.Second):
					t.Fatal("scoring did not stop promptly")
				}
				assert.False(t, backend.releasedAt.IsZero(), "scoring cancellation releases lease")
				require.NoError(t, backend.releaseContextErr, "lease release uses an uncancelled context")
				assert.Empty(t, b.scores, "cancelled scoring never reaches candidate resolution")
			})
			backend.releasedAt = time.Time{}
			cancelled, cancel := context.WithCancel(t.Context())
			cancel()
			_, err = searcher.Similar(cancelled, query)
			require.ErrorIs(t, err, context.Canceled)
			assert.False(t, backend.releasedAt.IsZero(), "cancellation releases lease")
			backend.releasedAt = time.Time{}
			b.resolveErr = errors.New("resolve failure")
			_, err = searcher.Similar(t.Context(), query)
			require.ErrorIs(t, err, b.resolveErr)
			assert.False(t, backend.releasedAt.IsZero(), "resolve failure releases lease")
			backend.releasedAt = time.Time{}
			backend.acquireErr = store.ErrSimilarSourceUnavailable
			report, err := searcher.Similar(t.Context(), query)
			require.NoError(t, err)
			require.NotNil(t, report.MissingCoverage)
			assert.True(t, backend.releasedAt.IsZero(), "unavailable acquired no lease")
			backend.acquireErr = store.ErrVectorIndexSourceStale
			_, err = searcher.Similar(t.Context(), query)
			require.ErrorIs(t, err, store.ErrVectorIndexSourceStale)
		})
	}
}
