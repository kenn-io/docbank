package processing

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorworker"
)

type selectedImageProvider struct{ *embeddingWorkerProvider }

func (p selectedImageProvider) VectorIdentity() (string, error) {
	return workerHash("selected-image-space"), nil
}

func TestPhotoEmbeddingPreservesSelectedVectorIdentity(t *testing.T) {
	for _, automatic := range []bool{true, false} {
		name := "explicit-search"
		if automatic {
			name = "automatic-discovery"
		}
		t.Run(name, func(t *testing.T) {
			f := newPhotoDescriptionFixture(t)
			x := newImageEmbeddingService(t, f)
			provider := selectedImageProvider{x.provider}
			service, err := NewService(ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: newTestOperationGate(), SpoolDirectory: t.TempDir(), Lifecycle: t.Context(), Principal: "synthetic-operator", Scope: "document-processing", Profiles: map[string]ProfileConfig{"images": {Profile: x.portable, EmbeddingSimilarityFloors: map[string]float64{x.binding.Name: 0.2}, EmbeddingProviders: map[string]document.EmbeddingProvider{x.binding.Name: provider}}}})
			require.NoError(t, err)
			f.grant(t, service, "images")
			if !automatic {
				version, err := f.catalog.ContentVersionByID(t.Context(), f.node.CurrentVersionID)
				require.NoError(t, err)
				_, err = service.runEmbeddings(t.Context(), version, service.profiles["images"], service.principal, service.scope, "", nil)
				require.NoError(t, err)
			}
			worker, err := NewEmbeddingWorker(EmbeddingWorkerConfig{Catalog: f.catalog, Authority: f.catalog, Blobs: f.blobs, GenerationBlobs: f.blobs, Runtime: service.EmbeddingRuntimes(), Gate: newTestOperationGate(), Owner: "synthetic-images", RetryLimit: 3, RetryBaseDelay: time.Millisecond, MaxRetryDelay: time.Second, AttemptLifetime: 10 * time.Minute, MaxRows: 100, MaxDimensions: 100, MaxVectorBlobBytes: 1 << 20, LeaseDuration: time.Minute, IdleDelay: time.Millisecond, DescriptorFingerprints: []string{x.descriptor.Fingerprint}})
			require.NoError(t, err)
			processed, err := worker.ScanOnce(t.Context())
			require.NoError(t, err)
			if automatic {
				require.Equal(t, 1, processed)
			}
			jobs, err := f.catalog.EmbeddingJobsForVersionProfile(t.Context(), f.node.CurrentVersionID, service.profiles["images"].record.Fingerprint)
			require.NoError(t, err)
			require.Len(t, jobs, 1)
			require.Equal(t, "completed", jobs[0].State)
			if automatic {
				return
			}
			spaces, err := f.catalog.ListVectorIndexSpaces(t.Context())
			require.NoError(t, err)
			require.Len(t, spaces, 1)
			indexer, err := vectorworker.NewIndexWorker(vectorworker.IndexWorkerConfig{Catalog: f.catalog, Owner: "synthetic-image-index", Mutate: newTestOperationGate().MutateContext, BuildLease: time.Minute, ReaderLease: time.Minute, IdleDelay: time.Millisecond, ReadVectorSet: func(ctx context.Context, member store.VectorIndexMember) ([]byte, error) {
				return readExactEmbeddingBlob(ctx, f.blobs, member.PayloadBlobHash, member.PayloadSize)
			}})
			require.NoError(t, err)
			_, err = indexer.Rebuild(t.Context(), spaces[0])
			require.NoError(t, err)
			result, err := service.SearchPhotos(t.Context(), store.PhotoBrowseRequest{Query: query.Query{V: 1, Syntax: "simple", Mode: "semantic", Text: "harbor", Sort: query.Sort{Field: "relevance", Direction: "desc"}}})
			require.NoError(t, err)
			require.Empty(t, result.FallbackReason)
			require.Equal(t, "semantic", result.ActualMode)
			require.Len(t, result.Members, 1)
			require.Equal(t, f.asset.ID, result.Members[0].AssetID)
		})
	}
}
