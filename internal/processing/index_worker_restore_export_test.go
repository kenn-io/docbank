package processing

import (
	"testing"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

// VectorIndexRestoreTestFixture exposes the existing real embedding fixture to
// the external restore integration test without adding test seams to production.
type VectorIndexRestoreTestFixture struct {
	Catalog           *store.Store
	Blobs             *blob.Store
	Worker            *EmbeddingWorker
	ProviderCallCount func() int
}

func NewVectorIndexRestoreTestFixture(
	t *testing.T, kind document.EmbeddingInputKind,
) VectorIndexRestoreTestFixture {
	t.Helper()
	fixture, provider, worker, _ := newRealEmbeddingWorker(t, kind)
	return VectorIndexRestoreTestFixture{
		Catalog: fixture.catalog,
		Blobs:   fixture.blobs,
		Worker:  worker,
		ProviderCallCount: func() int {
			return provider.runtime.calls()
		},
	}
}
