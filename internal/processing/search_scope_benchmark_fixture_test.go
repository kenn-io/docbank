package processing

import (
	"context"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorworker"
)

// Publish a constructor-valid profile from the start; stored source authority
// must retain the same fingerprint when NewService configures the benchmark.
func newSearchBenchmarkFixture(tb testing.TB, kind document.EmbeddingInputKind, dimension int) (publicationFixture, *Service, store.EmbeddingJobRequest) {
	tb.Helper()
	descriptor := embeddingWorkerDescriptor(tb)
	descriptor.Dimension, descriptor.Fingerprint = dimension, ""
	descriptor, err := document.NewEmbeddingDescriptor(descriptor)
	require.NoError(tb, err)
	fixture, fake, worker, job, _ := newRealEmbeddingWorkerConfigured(tb, kind, dimension, &descriptor)
	require.Equal(tb, dimension, job.Descriptor.Dimension)
	// An explicit descriptor leaves enqueueing to this fixture. Expand every
	// provider result before the worker can publish either input kind.
	expand := func(result document.EmbeddingResult) document.EmbeddingResult {
		for i := range result.Vectors {
			result.Vectors[i].Values = make([]float32, dimension)
			copy(result.Vectors[i].Values, []float32{1, 2})
		}
		return result
	}
	fake.runtime.mutate["direct"], fake.runtime.mutate["chunk"], fake.runtime.mutate["query"] = expand, expand, expand
	_, err = fixture.catalog.EnqueueEmbeddingJob(tb.Context(), job)
	require.NoError(tb, err)
	processed, err := worker.ScanOnce(tb.Context())
	require.NoError(tb, err)
	require.Equal(tb, 1, processed)
	spaces, err := fixture.catalog.ListVectorIndexSpaces(tb.Context())
	require.NoError(tb, err)
	require.Len(tb, spaces, 1)
	index, err := vectorworker.NewIndexWorker(vectorworker.IndexWorkerConfig{
		Catalog: fixture.catalog, Mutate: newTestOperationGate().MutateContext,
		Owner: "scope-test", BuildLease: time.Minute, ReaderLease: time.Minute, IdleDelay: time.Millisecond,
		ReadVectorSet: func(ctx context.Context, member store.VectorIndexMember) ([]byte, error) {
			return fixture.catalog.ReadVectorIndexVectorSet(ctx, fixture.blobs, member)
		},
	})
	require.NoError(tb, err)
	_, err = index.Rebuild(tb.Context(), spaces[0])
	require.NoError(tb, err)
	var portable document.ProcessingProfileV1
	require.NoError(tb, json.Unmarshal(fixture.profile.CanonicalProfile, &portable))
	provider := &embeddingWorkerProvider{runtime: fake.runtime, binding: "query", descriptor: fake.descriptor}
	profile := ProfileConfig{
		Profile: portable, RenditionProvider: newWorkerProvider(tb),
		EmbeddingProviders:   map[string]document.EmbeddingProvider{job.BindingID: provider},
		EmbeddingClassifiers: map[string]func(error) (EmbeddingProviderFailure, time.Duration){job.BindingID: fake.runtime.Classify},
	}
	if kind == document.EmbeddingInputRenditionChunk {
		profile.Tokenizers = map[string]document.Tokenizer{job.BindingID: embeddingIntegrationTokenizer{}}
	}
	service, err := NewService(ServiceConfig{
		Catalog: fixture.catalog, Blobs: fixture.blobs, Gate: newTestOperationGate(),
		Principal: "scope-test", Scope: "scope-test", SpoolDirectory: tb.TempDir(), Lifecycle: tb.Context(),
		Profiles: map[string]ProfileConfig{"private": profile},
	})
	require.NoError(tb, err)
	tb.Cleanup(service.Stop)
	require.Equal(tb, job.Profile.Fingerprint, fixture.profile.Fingerprint)
	require.Equal(tb, fixture.profile.Fingerprint, service.profiles["private"].record.Fingerprint)
	_, err = fixture.catalog.GrantConsent(tb.Context(), store.ProcessingConsentGrantRequest{
		Principal: service.principal, Scope: service.scope, ProfileFingerprint: fixture.profile.Fingerprint,
		DisclosureFingerprint: portable.Embeddings[0].DisclosureFingerprint,
		InputClasses:          []string{"query_text"}, RetainedArtifactClasses: []string{},
	})
	require.NoError(tb, err)
	tb.Logf("benchmark_fixture constructor=NewService kind=%s dimensions=%d profile=%s", kind, dimension, fixture.profile.Fingerprint)
	return fixture, service, job
}
