package processing

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/extract"
	"go.kenn.io/docbank/internal/retrieval"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorworker"
)

func TestSearchExcerptFiltersControls(t *testing.T) {
	t.Parallel()
	fixture, fake, worker, request := newRealEmbeddingWorker(t, document.EmbeddingInputOriginalFile)
	raw := []byte("plain text \x1b[2J\tstill here\n")
	blob, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(raw))
	require.NoError(t, err)
	node, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), "filenameonly.txt", blob.Hash, blob.Size, "text/plain", processingBlobPhysical(t, blob))
	require.NoError(t, err)
	extractor, err := extract.New(fixture.catalog, fixture.blobs, nil)
	require.NoError(t, err)
	_, err = extractor.ScanOnce(t.Context())
	require.NoError(t, err)
	request.ContentVersionID = node.CurrentVersionID
	request.InputGeneration.ID = workerHash("control-text-generation")
	request.InputGeneration.SourceVersionID = node.CurrentVersionID
	request.InputGeneration.GenerationChecksum = blob.Hash
	request.InputGeneration.Inputs = []store.EmbeddingInputReference{{ID: node.CurrentVersionID, RenderedChecksum: blob.Hash}}
	_, err = fixture.catalog.EnqueueEmbeddingJob(t.Context(), request)
	require.NoError(t, err)
	_, err = worker.ScanOnce(t.Context())
	require.NoError(t, err)
	_, err = worker.ScanOnce(t.Context())
	require.NoError(t, err)
	spaces, err := fixture.catalog.ListVectorIndexSpaces(t.Context())
	require.NoError(t, err)
	require.Len(t, spaces, 1)
	index, err := vectorworker.NewIndexWorker(vectorworker.IndexWorkerConfig{Catalog: fixture.catalog, Mutate: newTestOperationGate().MutateContext, Owner: "excerpt-repro", BuildLease: time.Minute, ReaderLease: time.Minute, IdleDelay: time.Millisecond, ReadVectorSet: func(ctx context.Context, member store.VectorIndexMember) ([]byte, error) {
		return fixture.catalog.ReadVectorIndexVectorSet(ctx, fixture.blobs, member)
	}})
	require.NoError(t, err)
	_, err = index.Rebuild(t.Context(), spaces[0])
	require.NoError(t, err)
	provider := &embeddingWorkerProvider{runtime: fake.runtime, binding: "query", descriptor: fake.descriptor}
	searcher, err := retrieval.NewSearcher(retrieval.SearcherConfig{Backend: fixture.catalog, Encoders: provider, Owner: "excerpt-repro", LeaseDuration: time.Minute})
	require.NoError(t, err)
	for _, mode := range []retrieval.Mode{retrieval.ModeSemantic, retrieval.ModeHybrid} {
		t.Run(string(mode), func(t *testing.T) {
			report, err := searcher.Search(t.Context(), retrieval.Query{Text: "filenameonly", Mode: mode, Limit: 1, Scope: store.SearchOptions{ContentVersionIDs: []string{node.CurrentVersionID}}, ProcessingProfileFingerprint: request.Profile.Fingerprint, BindingID: request.BindingID, Authorization: document.EmbeddingAuthorization{ProviderID: fake.descriptor.ID, DescriptorFingerprint: fake.descriptor.Fingerprint, PolicyFingerprint: fake.descriptor.PolicyFingerprint, MaxBatchItems: 1, MaxInputBytes: 1 << 20, MaxResponseBytes: 1 << 20}})
			require.NoError(t, err)
			require.Len(t, report.Results, 1)
			require.Equal(t, 1, report.Results[0].SemanticRank)
			if mode == retrieval.ModeHybrid {
				require.Equal(t, 1, report.Results[0].LexicalRank)
			}
			require.Equal(t, "plain text [2J\tstill here\n", report.Results[0].Excerpt)
		})
	}
}
