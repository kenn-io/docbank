package processing

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/config"
)

func TestMixedFaceOriginalProfileKeepsBindingEligibility(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	faces := newFaceEmbeddingFixture(t, false)
	f := faces.f
	fake := newEmbeddingWorkerFixture(t)
	var original document.ProcessingProfileV1
	require.NoError(t, jsonUnmarshalProfile(fake.profile.CanonicalProfile, &original))
	direct := original.Embeddings[1]
	faceProfile := faces.service.profiles[config.LocalFacesProfile]
	portable := faceProfile.portable
	portable.Embeddings = append(portable.Embeddings, direct)
	providers := map[string]document.EmbeddingProvider{
		config.LocalFacesProfile: faceProfile.embedders[config.LocalFacesProfile],
		direct.Name:              &embeddingWorkerProvider{runtime: fake.runtime, binding: direct.Name, descriptor: fake.descriptor},
	}
	service, err := NewService(ctx, ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: newTestOperationGate(), SpoolDirectory: t.TempDir(), Lifecycle: ctx, Principal: "synthetic-operator", Scope: "document-processing", Profiles: map[string]ProfileConfig{"mixed": {Profile: portable, EmbeddingProviders: providers}}})
	require.NoError(t, err)
	t.Cleanup(service.Stop)
	f.grant(t, service, "mixed")
	plan, err := service.Plan(ctx, f.selector("mixed"))
	require.NoError(t, err)
	_, err = service.Start(ctx, StartRequest{Selector: f.selector("mixed"), PlanFingerprint: plan.Fingerprint})
	require.NoError(t, err)
	worker, err := NewEmbeddingWorker(EmbeddingWorkerConfig{Catalog: f.catalog, Authority: f.catalog, Blobs: f.blobs, GenerationBlobs: f.blobs, Runtime: service.EmbeddingRuntimes(), Gate: newTestOperationGate(), Owner: "mixed-original-test", RetryLimit: 1, RetryBaseDelay: time.Millisecond, MaxRetryDelay: time.Second, AttemptLifetime: time.Minute, MaxRows: 100, MaxDimensions: 512, MaxVectorBlobBytes: 1 << 20, LeaseDuration: time.Minute, IdleDelay: time.Millisecond, DescriptorFingerprints: []string{fake.descriptor.Fingerprint, providers[config.LocalFacesProfile].Descriptor().Fingerprint}})
	require.NoError(t, err)
	for range 4 {
		worker.reconcileAt = time.Time{}
		_, err = worker.ScanOnce(ctx)
		require.NoError(t, err, "mixed bindings must reconcile only their eligible input generations")
	}
	require.Equal(t, 1, fake.runtime.callCount(direct.Name), "original-file binding must still process the source")
	require.Equal(t, 1, faces.calls, "face binding must still process detected faces")
	sets, err := f.catalog.FaceMatchingInputs(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, sets.Sets, "face vectors must publish for matching")
}
