package processing

import (
	"bytes"
	"context"
	"errors"
	"image/color"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestIndexWorkerPurgedFinalSourceRetiresHeadAndReclaimsGeneration(t *testing.T) {
	fixture, _, embeddingWorker, request := newRealEmbeddingWorker(t, document.EmbeddingInputOriginalFile)
	requireRealEmbeddingPublication(t, embeddingWorker)
	now := indexWorkerNow
	worker, stop := newRealCatalogIndexWorker(t, fixture, &now)
	space := onlyRealVectorSpace(t, fixture.catalog)
	source, err := fixture.catalog.CaptureVectorIndexSource(t.Context(), space)
	require.NoError(t, err)
	generation, err := worker.Rebuild(t.Context(), source.VectorSpaceID)
	require.NoError(t, err)

	report, err := fixture.catalog.PurgeDerivatives(t.Context(), store.PurgeRequest{
		ContentVersionIDs: []string{request.ContentVersionID},
	})
	require.NoError(t, err)
	require.Equal(t, 1, report.RemovedEmbeddingSets)
	_, err = fixture.catalog.ActiveVectorIndexGeneration(t.Context(), source.VectorSpaceID)
	require.ErrorIs(t, err, store.ErrNotFound,
		"purge must immediately revoke the disposable index projection")
	_, err = fixture.catalog.AcquireVectorIndexGeneration(t.Context(), source.VectorSpaceID,
		"post-purge-reader", now, time.Minute)
	require.ErrorIs(t, err, store.ErrNotFound)

	require.ErrorIs(t, worker.Run(t.Context()), stop,
		"one bounded worker pass must reclaim even when no embedding spaces remain")
	_, err = fixture.catalog.LoadVectorIndexGeneration(t.Context(), generation.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestIndexWorkerPurgedGenerationSurvivesOnlyItsLiveReaderLease(t *testing.T) {
	fixture, _, embeddingWorker, request := newRealEmbeddingWorker(t, document.EmbeddingInputOriginalFile)
	requireRealEmbeddingPublication(t, embeddingWorker)
	now := indexWorkerNow
	worker, stop := newRealCatalogIndexWorker(t, fixture, &now)
	space := onlyRealVectorSpace(t, fixture.catalog)
	source, err := fixture.catalog.CaptureVectorIndexSource(t.Context(), space)
	require.NoError(t, err)
	generation, err := worker.Rebuild(t.Context(), source.VectorSpaceID)
	require.NoError(t, err)
	lease, err := worker.Acquire(t.Context(), source.VectorSpaceID, "live-reader")
	require.NoError(t, err)

	_, err = fixture.catalog.PurgeDerivatives(t.Context(), store.PurgeRequest{
		ContentVersionIDs: []string{request.ContentVersionID},
	})
	require.NoError(t, err)
	require.ErrorIs(t, worker.Run(t.Context()), stop)
	retained, err := fixture.catalog.LoadVectorIndexGeneration(t.Context(), generation.ID)
	require.NoError(t, err)
	assert.Equal(t, generation.ID, retained.ID)
	neighbors, err := lease.Search([]float32{1, 2}, 1)
	require.NoError(t, err)
	require.Len(t, neighbors, 1)

	now = now.Add(2 * time.Minute)
	require.ErrorIs(t, worker.Run(t.Context()), stop)
	_, err = fixture.catalog.LoadVectorIndexGeneration(t.Context(), generation.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestIndexWorkerRetiresHeadWhenHistoricalEmbeddingHeadHasNoEligibleSource(t *testing.T) {
	fixture, _, embeddingWorker, _ := newRealEmbeddingWorker(t, document.EmbeddingInputOriginalFile)
	requireRealEmbeddingPublication(t, embeddingWorker)
	now := indexWorkerNow
	worker, stop := newRealCatalogIndexWorker(t, fixture, &now)
	space := onlyRealVectorSpace(t, fixture.catalog)
	source, err := fixture.catalog.CaptureVectorIndexSource(t.Context(), space)
	require.NoError(t, err)
	generation, err := worker.Rebuild(t.Context(), source.VectorSpaceID)
	require.NoError(t, err)
	node, err := fixture.catalog.NodeByPath(t.Context(), "/synthetic.png")
	require.NoError(t, err)
	_, _, err = fixture.catalog.Trash(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	spaces, err := fixture.catalog.ListVectorIndexSpaces(t.Context())
	require.NoError(t, err)
	require.Contains(t, spaces, source.VectorSpaceID,
		"the regression requires a retained historical embedding head")
	_, err = fixture.catalog.CaptureVectorIndexSource(t.Context(), source.VectorSpaceID)
	require.ErrorIs(t, err, store.ErrNotFound,
		"retirement must follow current source eligibility, not embedding head count")

	require.ErrorIs(t, worker.Run(t.Context()), stop)
	_, err = fixture.catalog.ActiveVectorIndexGeneration(t.Context(), source.VectorSpaceID)
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = fixture.catalog.LoadVectorIndexGeneration(t.Context(), generation.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestIndexWorkerPartialSpacePurgeRebuildsRemainingAuthority(t *testing.T) {
	fixture, _, embeddingWorker, request := newRealEmbeddingWorker(t, document.EmbeddingInputOriginalFile)
	requireRealEmbeddingPublication(t, embeddingWorker)
	enqueueSecondRealOriginalEmbedding(t, fixture, request)
	requireRealEmbeddingPublication(t, embeddingWorker)
	now := indexWorkerNow
	worker, stop := newRealCatalogIndexWorker(t, fixture, &now)
	spaces, err := fixture.catalog.ListVectorIndexSpaces(t.Context())
	require.NoError(t, err)
	require.Len(t, spaces, 1)
	space := spaces[0]
	before, err := fixture.catalog.CaptureVectorIndexSource(t.Context(), space)
	require.NoError(t, err)
	require.Len(t, before.Members, 2)
	firstGeneration, err := worker.Rebuild(t.Context(), space)
	require.NoError(t, err)

	_, err = fixture.catalog.PurgeDerivatives(t.Context(), store.PurgeRequest{
		ContentVersionIDs: []string{request.ContentVersionID},
	})
	require.NoError(t, err)
	after, err := fixture.catalog.CaptureVectorIndexSource(t.Context(), space)
	require.NoError(t, err)
	require.Len(t, after.Members, 1)
	active, err := fixture.catalog.ActiveVectorIndexGeneration(t.Context(), space)
	require.NoError(t, err)
	require.Equal(t, firstGeneration.ID, active.ID,
		"partial purge must retain the prior projection until the replacement publishes")

	require.ErrorIs(t, worker.Run(t.Context()), stop)
	rebuilt, err := fixture.catalog.ActiveVectorIndexGeneration(t.Context(), space)
	require.NoError(t, err)
	require.Equal(t, after.ManifestChecksum, rebuilt.SourceManifestChecksum)
	require.NotEqual(t, firstGeneration.ID, rebuilt.ID)
	_, err = fixture.catalog.LoadVectorIndexGeneration(t.Context(), firstGeneration.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func requireRealEmbeddingPublication(t *testing.T, worker *EmbeddingWorker) {
	t.Helper()
	processed, err := worker.ScanOnce(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, processed)
}

func onlyRealVectorSpace(t *testing.T, catalog *store.Store) string {
	t.Helper()
	spaces, err := catalog.ListVectorIndexSpaces(t.Context())
	require.NoError(t, err)
	require.Len(t, spaces, 1)
	return spaces[0]
}

func newRealCatalogIndexWorker(t *testing.T, fixture publicationFixture, now *time.Time) (*IndexWorker, error) {
	t.Helper()
	stop := errors.New("stop after one real catalog pass")
	worker, err := NewIndexWorker(IndexWorkerConfig{
		Catalog: fixture.catalog, Blobs: fixture.blobs, Gate: api.NewOperationGate(),
		Owner: "real-catalog-index-worker", BuildLease: time.Minute, ReaderLease: time.Minute,
		IdleDelay: time.Millisecond, Clock: func() time.Time { return *now },
		Wait: func(context.Context, time.Duration) error { return stop },
	})
	require.NoError(t, err)
	return worker, stop
}

func enqueueSecondRealOriginalEmbedding(t *testing.T, fixture publicationFixture,
	request store.EmbeddingJobRequest,
) string {
	t.Helper()
	data := mediatest.PNG(2, 2, color.Black)
	receipt, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(data))
	require.NoError(t, err)
	node, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), "synthetic-second.png",
		receipt.Hash, receipt.Size, "image/png", processingBlobPhysical(t, receipt))
	require.NoError(t, err)
	request.ContentVersionID = node.CurrentVersionID
	request.InputGeneration.ID = workerHash("integration-generation-second")
	request.InputGeneration.SourceVersionID = node.CurrentVersionID
	request.InputGeneration.GenerationChecksum = receipt.Hash
	request.InputGeneration.Inputs = []store.EmbeddingInputReference{{
		ID: node.CurrentVersionID, RenderedChecksum: receipt.Hash,
	}}
	_, err = fixture.catalog.EnqueueEmbeddingJob(t.Context(), request)
	require.NoError(t, err)
	return node.CurrentVersionID
}
