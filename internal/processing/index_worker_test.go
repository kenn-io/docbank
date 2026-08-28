package processing

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorindex"
	"go.kenn.io/docbank/internal/vectorworker"
	"go.kenn.io/kit/backup"
)

func TestVectorIndexRestoreRebuildsRealPublishedEmbeddings(t *testing.T) {
	for _, kind := range []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile, document.EmbeddingInputRenditionChunk} {
		t.Run(string(kind), func(t *testing.T) {
			fixture, fake, worker, _ := newRealEmbeddingWorker(t, kind)
			processed, err := worker.ScanOnce(t.Context())
			require.NoError(t, err)
			require.Equal(t, 1, processed)
			providerCalls := fake.runtime.calls()
			spaces, err := fixture.catalog.ListVectorIndexSpaces(t.Context())
			require.NoError(t, err)
			require.Len(t, spaces, 1)
			source, err := fixture.catalog.CaptureVectorIndexSource(t.Context(), spaces[0])
			require.NoError(t, err)
			repo, err := backup.Init(filepath.Join(t.TempDir(), "backup"))
			require.NoError(t, err)
			_, err = backupapp.Create(t.Context(), repo, "test-version", fixture.catalog, fixture.blobs, backup.CreateOptions{Jobs: 1})
			require.NoError(t, err)
			target := filepath.Join(t.TempDir(), "restored")
			_, err = backupapp.Restore(t.Context(), repo, "test-version", backup.RestoreOptions{TargetDir: target, Jobs: 1})
			require.NoError(t, err)
			restored, err := store.Open(filepath.Join(target, "docbank.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, restored.Close()) })
			active, err := restored.ActiveVectorIndexGeneration(t.Context(), spaces[0])
			require.NoError(t, err)
			require.Equal(t, source.ManifestChecksum, active.SourceManifestChecksum)
			index, err := vectorindex.OpenGeneration(bytes.NewReader(active.Bytes), int64(len(active.Bytes)))
			require.NoError(t, err)
			query := make([]float32, index.Metadata().Dimension)
			query[0] = 1
			hits, err := index.Search(query, 1)
			require.NoError(t, err)
			require.Len(t, hits, 1)
			require.Equal(t, source.Members[0].VectorSetID, hits[0].SetID)
			require.Equal(t, providerCalls, fake.runtime.calls(), "restore uses retained vectors without invoking the provider")
		})
	}
}

func TestVectorIndexFailedReadReleasesBuildClaim(t *testing.T) {
	fixture, _, embedding, _ := newRealEmbeddingWorker(t, document.EmbeddingInputOriginalFile)
	_, err := embedding.ScanOnce(t.Context())
	require.NoError(t, err)
	spaces, err := fixture.catalog.ListVectorIndexSpaces(t.Context())
	require.NoError(t, err)
	require.Len(t, spaces, 1)
	failRead := true
	worker, err := vectorworker.NewIndexWorker(vectorworker.IndexWorkerConfig{
		Mutate:  newWorkerTestGate().MutateContext,
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
	gate := newWorkerTestGate()
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
		Catalog: fixture.catalog, Mutate: newWorkerTestGate().MutateContext, Owner: "index-worker", BuildLease: time.Minute, ReaderLease: time.Minute, IdleDelay: time.Millisecond,
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
	gate := newWorkerTestGate()
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
		gate := newWorkerTestGate()
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
