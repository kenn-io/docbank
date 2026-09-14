package processing

import (
	"context"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorworker"
)

func TestVectorIndexFailedReadReleasesBuildClaim(t *testing.T) {
	fixture, _, embedding, _ := newRealEmbeddingWorker(t, document.EmbeddingInputOriginalFile)
	_, err := embedding.ScanOnce(t.Context())
	require.NoError(t, err)
	spaces, err := fixture.catalog.ListVectorIndexSpaces(t.Context())
	require.NoError(t, err)
	require.Len(t, spaces, 1)
	failRead := true
	worker, err := vectorworker.NewIndexWorker(vectorworker.IndexWorkerConfig{
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
	fixture, _, embedding, _ := newRealEmbeddingWorker(t, document.EmbeddingInputOriginalFile)
	_, err := embedding.ScanOnce(t.Context())
	require.NoError(t, err)
	spaces, err := fixture.catalog.ListVectorIndexSpaces(t.Context())
	require.NoError(t, err)
	require.Len(t, spaces, 1)
	gate := newTestOperationGate()
	worker, err := vectorworker.NewIndexWorker(vectorworker.IndexWorkerConfig{
		Catalog: fixture.catalog, Mutate: gate.MutateContext, Owner: "index-worker", BuildLease: time.Minute, ReaderLease: time.Minute, IdleDelay: time.Millisecond,
		ReadVectorSet: func(ctx context.Context, member store.VectorIndexMember) ([]byte, error) {
			return fixture.catalog.ReadVectorIndexVectorSet(ctx, fixture.blobs, member)
		},
	})
	require.NoError(t, err)
	require.NoError(t, gate.MaintainContext(t.Context(), func() error {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
		defer cancel()
		_, err := worker.Rebuild(ctx, spaces[0])
		require.ErrorIs(t, err, context.DeadlineExceeded)
		_, err = fixture.catalog.ActiveVectorIndexGeneration(t.Context(), spaces[0])
		require.ErrorIs(t, err, store.ErrNotFound)
		return nil
	}))
	_, err = worker.Rebuild(t.Context(), spaces[0])
	require.NoError(t, err, "rebuild resumes when maintenance releases admission")
}

func TestVectorIndexWorkerRetiresTrashedSource(t *testing.T) {
	fixture, _, embedding, _ := newRealEmbeddingWorker(t, document.EmbeddingInputOriginalFile)
	_, err := embedding.ScanOnce(t.Context())
	require.NoError(t, err)
	spaces, err := fixture.catalog.ListVectorIndexSpaces(t.Context())
	require.NoError(t, err)
	require.Len(t, spaces, 1)
	worker, err := vectorworker.NewIndexWorker(vectorworker.IndexWorkerConfig{
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
	fixture, _, embedding, _ := newRealEmbeddingWorker(t, document.EmbeddingInputOriginalFile)
	_, err := embedding.ScanOnce(t.Context())
	require.NoError(t, err)
	spaces, err := fixture.catalog.ListVectorIndexSpaces(t.Context())
	require.NoError(t, err)
	require.Len(t, spaces, 1)
	gate := newTestOperationGate()
	worker, err := vectorworker.NewIndexWorker(vectorworker.IndexWorkerConfig{
		Catalog: fixture.catalog, Mutate: gate.MutateContext, Owner: "index-worker", BuildLease: time.Minute, ReaderLease: time.Minute, IdleDelay: time.Millisecond,
		ReadVectorSet: func(ctx context.Context, member store.VectorIndexMember) ([]byte, error) {
			maintenanceCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
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
	fixture, _, embedding, _ := newRealEmbeddingWorker(t, document.EmbeddingInputOriginalFile)
	_, err := embedding.ScanOnce(t.Context())
	require.NoError(t, err)
	synctest.Test(t, func(t *testing.T) {
		gate := newTestOperationGate()
		held, done := make(chan struct{}), make(chan error, 1)
		worker, err := vectorworker.NewIndexWorker(vectorworker.IndexWorkerConfig{
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
