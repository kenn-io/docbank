package processing

import (
	"bytes"
	"context"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorindex"
)

func TestVectorIndexFailedReadReleasesBuildClaim(t *testing.T) {
	t.Parallel()
	checkVectorIndexFailedReadReleasesBuildClaim(t, newPublicationFixture)
}

func checkVectorIndexFailedReadReleasesBuildClaim(t *testing.T, makeFixture publicationFixtureFactory) {
	t.Helper()
	fixture, _, embedding, _ := embeddingWorkerWithFixture(t, makeFixture, document.EmbeddingInputOriginalFile)
	_, err := embedding.ScanOnce(t.Context())
	require.NoError(t, err)
	spaces, err := fixture.catalog.ListVectorIndexSpaces(t.Context())
	require.NoError(t, err)
	require.Len(t, spaces, 1)
	failRead := true
	worker, err := NewIndexWorker(IndexWorkerConfig{
		Mutate:  newTestOperationGate().MutateContext,
		Catalog: fixture.catalog, Owner: "index-worker", BuildLease: 30 * time.Minute, ReaderLease: time.Minute, IdleDelay: time.Millisecond,
		ReadVectorSet: func(ctx context.Context, member store.VectorIndexMember) ([]byte, error) {
			if failRead {
				return nil, io.ErrUnexpectedEOF
			}
			return fixture.catalog.ReadVectorIndexVectorSet(ctx, fixture.blobs, member)
		},
	})
	require.NoError(t, err)
	_, err = worker.Rebuild(t.Context(), spaces[0])
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	failRead = false
	_, err = worker.Rebuild(t.Context(), spaces[0])
	require.NoError(t, err, "a known failed attempt must release its claim immediately")
}

func TestVectorIndexRebuildWaitsForMaintenanceAdmission(t *testing.T) {
	t.Parallel()
	fixture, _, embedding, _ := newRealEmbeddingWorker(t, document.EmbeddingInputOriginalFile)
	_, err := embedding.ScanOnce(t.Context())
	require.NoError(t, err)
	spaces, err := fixture.catalog.ListVectorIndexSpaces(t.Context())
	require.NoError(t, err)
	require.Len(t, spaces, 1)
	gate := newTestOperationGate()
	worker, err := NewIndexWorker(IndexWorkerConfig{
		Catalog: fixture.catalog, Mutate: gate.MutateContext, Owner: "index-worker", BuildLease: time.Minute, ReaderLease: time.Minute, IdleDelay: time.Millisecond,
		ReadVectorSet: func(ctx context.Context, member store.VectorIndexMember) ([]byte, error) {
			return fixture.catalog.ReadVectorIndexVectorSet(ctx, fixture.blobs, member)
		},
	})
	require.NoError(t, err)
	require.NoError(t, gate.MaintainContext(t.Context(), func() error {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
			defer cancel()
			_, err := worker.Rebuild(ctx, spaces[0])
			require.ErrorIs(t, err, context.DeadlineExceeded)
		})
		_, err = fixture.catalog.ActiveVectorIndexGeneration(t.Context(), spaces[0])
		require.ErrorIs(t, err, store.ErrNotFound)
		return nil
	}))
	_, err = worker.Rebuild(t.Context(), spaces[0])
	require.NoError(t, err, "rebuild resumes when maintenance releases admission")
}

func TestVectorIndexWorkerRetiresTrashedSource(t *testing.T) {
	t.Parallel()
	checkVectorIndexWorkerRetiresTrashedSource(t, newPublicationFixture)
}

func checkVectorIndexWorkerRetiresTrashedSource(t *testing.T, makeFixture publicationFixtureFactory) {
	t.Helper()
	fixture, _, embedding, _ := embeddingWorkerWithFixture(t, makeFixture, document.EmbeddingInputOriginalFile)
	_, err := embedding.ScanOnce(t.Context())
	require.NoError(t, err)
	spaces, err := fixture.catalog.ListVectorIndexSpaces(t.Context())
	require.NoError(t, err)
	require.Len(t, spaces, 1)
	worker, err := NewIndexWorker(IndexWorkerConfig{
		Catalog: fixture.catalog, Mutate: newTestOperationGate().MutateContext, Owner: "index-worker", BuildLease: time.Minute, ReaderLease: time.Minute, IdleDelay: time.Millisecond,
		ReadVectorSet: func(ctx context.Context, member store.VectorIndexMember) ([]byte, error) {
			return fixture.catalog.ReadVectorIndexVectorSet(ctx, fixture.blobs, member)
		},
	})
	require.NoError(t, err)
	active, err := worker.Rebuild(t.Context(), spaces[0])
	require.NoError(t, err)
	_, _, err = fixture.catalog.TrashPath(t.Context(), "/synthetic.png")
	require.NoError(t, err)
	_, err = worker.Rebuild(t.Context(), spaces[0])
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = fixture.catalog.ActiveVectorIndexGeneration(t.Context(), spaces[0])
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = fixture.catalog.LoadVectorIndexGeneration(t.Context(), active.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestVectorIndexMaintenanceCanRunDuringPayloadRead(t *testing.T) {
	t.Parallel()
	checkVectorIndexMaintenanceCanRunDuringPayloadRead(t, newPublicationFixture)
}

func checkVectorIndexMaintenanceCanRunDuringPayloadRead(t *testing.T, makeFixture publicationFixtureFactory) {
	t.Helper()
	fixture, _, embedding, _ := embeddingWorkerWithFixture(t, makeFixture, document.EmbeddingInputOriginalFile)
	_, err := embedding.ScanOnce(t.Context())
	require.NoError(t, err)
	spaces, err := fixture.catalog.ListVectorIndexSpaces(t.Context())
	require.NoError(t, err)
	require.Len(t, spaces, 1)
	gate := newTestOperationGate()
	worker, err := NewIndexWorker(IndexWorkerConfig{
		Catalog: fixture.catalog, Mutate: gate.MutateContext, Owner: "index-worker", BuildLease: time.Minute, ReaderLease: time.Minute, IdleDelay: time.Millisecond,
		ReadVectorSet: func(ctx context.Context, member store.VectorIndexMember) ([]byte, error) {
			maintenanceCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if err := gate.MaintainContext(maintenanceCtx, func() error { return nil }); err != nil {
				return nil, err
			}
			return fixture.catalog.ReadVectorIndexVectorSet(ctx, fixture.blobs, member)
		},
	})
	require.NoError(t, err)
	_, err = worker.Rebuild(t.Context(), spaces[0])
	require.NoError(t, err, "payload reads must not hold maintenance admission")
}

func TestVectorIndexMissingPayloadWaitsForMaintenanceBeforeAbandoning(t *testing.T) {
	t.Parallel()
	fixture, _, embedding, _ := newRealEmbeddingWorker(t, document.EmbeddingInputOriginalFile)
	_, err := embedding.ScanOnce(t.Context())
	require.NoError(t, err)
	synctest.Test(t, func(t *testing.T) {
		gate := newTestOperationGate()
		held, done := make(chan struct{}), make(chan error, 1)
		worker, err := NewIndexWorker(IndexWorkerConfig{
			Catalog: fixture.catalog, Mutate: gate.MutateContext, Owner: "index-worker", BuildLease: time.Minute, ReaderLease: time.Minute, IdleDelay: time.Millisecond,
			ReadVectorSet: func(ctx context.Context, _ store.VectorIndexMember) ([]byte, error) {
				go func() {
					done <- gate.MaintainContext(ctx, func() error {
						close(held)
						time.Sleep(6 * time.Second)
						return nil
					})
				}()
				<-held
				return nil, store.ErrVectorSetUnavailable
			},
		})
		require.NoError(t, err)
		report, restoreErr := worker.Restore(t.Context())
		require.NoError(t, <-done)
		require.NoError(t, restoreErr, "maintenance admission must not turn unavailable coverage into a terminal cleanup timeout")
		require.Len(t, report.Unavailable, 1)
	})
}

func TestVectorIndexRestoresRetainedVectors(t *testing.T) {
	t.Parallel()
	checkVectorIndexRestoresRetainedVectors(t, newPublicationFixture)
}

func checkVectorIndexRestoresRetainedVectors(t *testing.T, makeFixture publicationFixtureFactory) {
	t.Helper()
	for _, kind := range []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile, document.EmbeddingInputRenditionChunk} {
		t.Run(string(kind), func(t *testing.T) {
			fixture, provider, embedding, _ := embeddingWorkerWithFixture(t, makeFixture, kind)
			count, err := embedding.ScanOnce(t.Context())
			require.NoError(t, err)
			require.Equal(t, 1, count)
			calls := provider.runtime.calls()
			spaces, err := fixture.catalog.ListVectorIndexSpaces(t.Context())
			require.NoError(t, err)
			require.Len(t, spaces, 1)
			space := spaces[0]
			source, err := fixture.catalog.CaptureVectorIndexSource(t.Context(), space)
			require.NoError(t, err)
			require.Len(t, source.Members, 1)
			member := source.Members[0]
			payload, err := fixture.catalog.ReadVectorIndexVectorSet(t.Context(), fixture.blobs, member)
			require.NoError(t, err)
			require.NoError(t, fixture.blobs.Remove(member.PayloadBlobHash))
			indexer, err := NewIndexWorker(IndexWorkerConfig{
				Catalog: fixture.catalog, Mutate: newTestOperationGate().MutateContext, Owner: "restore-index",
				BuildLease: time.Minute, ReaderLease: time.Minute, IdleDelay: time.Millisecond,
				ReadVectorSet: func(ctx context.Context, member store.VectorIndexMember) ([]byte, error) {
					return fixture.catalog.ReadVectorIndexVectorSet(ctx, fixture.blobs, member)
				},
			})
			require.NoError(t, err)
			report, err := indexer.Restore(t.Context())
			require.NoError(t, err)
			require.Empty(t, report.Rebuilt)
			require.Len(t, report.Unavailable, 1)
			require.Equal(t, []store.VectorIndexUnavailableSet{{EmbeddingSetID: member.EmbeddingSetID, VectorSetID: member.VectorSetID, PayloadBlobHash: member.PayloadBlobHash}}, report.Unavailable[0].Missing)
			require.True(t, report.Unavailable[0].ExternalReembeddingRequired)
			pending, err := fixture.catalog.VectorIndexPublicationPending(t.Context(), space)
			require.NoError(t, err)
			require.False(t, pending)
			_, err = fixture.catalog.ActiveVectorIndexGeneration(t.Context(), space)
			require.ErrorIs(t, err, store.ErrNotFound)
			receipt, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(payload))
			require.NoError(t, err)
			require.Equal(t, member.PayloadBlobHash, receipt.Hash)
			report, err = indexer.Restore(t.Context())
			require.NoError(t, err)
			require.Empty(t, report.Unavailable)
			require.Len(t, report.Rebuilt, 1)
			active, err := fixture.catalog.ActiveVectorIndexGeneration(t.Context(), space)
			require.NoError(t, err)
			require.Equal(t, report.Rebuilt[0], active)
			index, err := vectorindex.OpenGeneration(bytes.NewReader(active.Bytes), int64(len(active.Bytes)))
			require.NoError(t, err)
			query := make([]float32, index.Metadata().Dimension)
			query[0] = 1
			hits, err := index.Search(query, 1)
			require.NoError(t, err)
			require.Len(t, hits, 1)
			require.Equal(t, member.VectorSetID, hits[0].SetID)
			require.Equal(t, calls, provider.runtime.calls(), "index recovery must not invoke the provider")
		})
	}
}
