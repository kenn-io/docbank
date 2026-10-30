package processing

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/retrieval"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorindex"
	"go.kenn.io/docbank/internal/vectorworker"
	docsqlite "go.kenn.io/docbank/sqlite"
)

func TestServiceGenerationReuseAfterReclaimedIdenticalRebuild(t *testing.T) {
	t.Parallel()
	fixture, service, _, _ := newSearchScopeFixture(t)
	request := searchScopeRequest(service)
	first, err := service.Search(t.Context(), request)
	require.NoError(t, err)
	require.NotEmpty(t, first.Results)
	spaces, err := fixture.catalog.ListVectorIndexSpaces(t.Context())
	require.NoError(t, err)
	require.Len(t, spaces, 1)
	before, err := fixture.catalog.ActiveVectorIndexGeneration(t.Context(), spaces[0])
	require.NoError(t, err)
	cached, err := service.generationSource().Get(t.Context(), cacheHead(before))
	require.NoError(t, err)
	builtAt, err := time.Parse(time.RFC3339Nano, before.BuiltAt)
	require.NoError(t, err)
	rebuiltAt := builtAt.Add(time.Minute)
	worker, err := vectorworker.NewIndexWorker(vectorworker.IndexWorkerConfig{
		Catalog: fixture.catalog, Mutate: newTestOperationGate().MutateContext, Owner: "cache-rebuild-test",
		BuildLease: time.Minute, ReaderLease: time.Minute, IdleDelay: time.Millisecond, Clock: func() time.Time { return rebuiltAt },
		ReadVectorSet: func(ctx context.Context, member store.VectorIndexMember) ([]byte, error) {
			return fixture.catalog.ReadVectorIndexVectorSet(ctx, fixture.blobs, member)
		},
	})
	require.NoError(t, err)
	node, err := fixture.catalog.NodeByPath(t.Context(), "/synthetic.png")
	require.NoError(t, err)
	trashed, _, err := fixture.catalog.Trash(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	_, err = worker.Rebuild(t.Context(), spaces[0])
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = fixture.catalog.LoadVectorIndexGeneration(t.Context(), before.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "retired generation must actually be reclaimed")
	restored, _, err := fixture.catalog.Restore(t.Context(), node.ID, trashed.Revision)
	require.NoError(t, err)
	require.Equal(t, node.CurrentVersionID, restored.CurrentVersionID)
	source, err := fixture.catalog.CaptureVectorIndexSource(t.Context(), spaces[0])
	require.NoError(t, err)
	require.Equal(t, before.SourceManifestChecksum, source.ManifestChecksum)
	rebuilt, err := worker.Rebuild(t.Context(), spaces[0])
	require.NoError(t, err)
	require.Equal(t, before.ID, rebuilt.ID, "the real worker must recreate the same content-addressed ID")
	require.Equal(t, before.Bytes, rebuilt.Bytes)
	require.NotEqual(t, before.BuiltAt, rebuilt.BuiltAt)
	report, err := service.Search(t.Context(), request)
	require.NoError(t, err)
	require.NotEmpty(t, report.Results)
	require.Equal(t, first.Results[0].Document, report.Results[0].Document)
	retained, err := service.generationSource().Get(t.Context(), cacheHead(rebuilt))
	require.NoError(t, err)
	require.Same(t, cached, retained)
	// Ignoring a valid timestamp change in content identity must not bypass the
	// store's fresh timestamp validation on a warm lookup.
	db, err := store.DefaultSQLiteDriver().Open(fixture.databasePath, docsqlite.OpenOptions{Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate})
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	_, err = db.ExecContext(t.Context(), "UPDATE vector_index_generations SET built_at='invalid' WHERE generation_id=?", rebuilt.ID)
	require.NoError(t, err)
	_, err = service.Search(t.Context(), request)
	require.ErrorContains(t, err, "built_at")
	var leases int
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM vector_index_reader_leases").Scan(&leases))
	require.Zero(t, leases)
}

func TestServiceGenerationReuseDrainsAfterStop(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"success", "load failure", "cancellation"} {
		t.Run(outcome, func(t *testing.T) {
			fixture, service, _, _ := newSearchScopeFixture(t)
			lifecycle, stop := context.WithCancel(t.Context())
			service.lifecycle, service.stop, service.drained = lifecycle, stop, make(chan struct{})
			t.Cleanup(service.Stop)
			request := searchScopeRequest(service)
			first, err := service.Search(t.Context(), request)
			require.NoError(t, err)
			require.NotEmpty(t, first.Results)
			cache := service.generationSource()
			cache.mu.Lock()
			var warm *vectorindex.Generation
			for _, space := range cache.spaces {
				warm = space.entry.generation
			}
			cache.mu.Unlock()
			require.NotNil(t, warm)
			originalLoad := cache.load
			var loads atomic.Int32
			loadFailure := errors.New("synthetic generation read failure")
			cache.load = func(ctx context.Context, id string) (store.VectorIndexGenerationRecord, error) {
				loads.Add(1)
				if outcome == "load failure" {
					return store.VectorIndexGenerationRecord{}, loadFailure
				}
				return originalLoad(ctx, id)
			}
			source := &cacheDrainSource{cache: cache, entered: make(chan struct{}), proceed: make(chan struct{})}
			prepared, err := service.prepareSearch(request, service.profiles["private"])
			require.NoError(t, err)
			searcher, err := retrieval.NewSearcher(retrieval.SearcherConfig{Backend: fixture.catalog, Encoders: service.embeddings,
				Owner: "cache-drain-test", LeaseDuration: time.Minute, GenerationSource: source})
			require.NoError(t, err)
			query := retrieval.Query{Text: request.Query, Mode: retrieval.ModeSemantic, Limit: request.Limit, ObserveScope: true,
				ProcessingProfileFingerprint: fixture.profile.Fingerprint, BindingID: prepared.bindingID, Authorization: prepared.authorization}
			queryCtx, cancel := context.WithCancel(t.Context())
			result := make(chan cacheDrainResult, 1)
			var queries sync.WaitGroup
			var proceedOnce sync.Once
			t.Cleanup(func() { cancel(); proceedOnce.Do(func() { close(source.proceed) }); queries.Wait() })
			queries.Go(func() {
				report, err := searcher.Search(queryCtx, query)
				result <- cacheDrainResult{report: report, err: err}
			})
			select {
			case <-source.entered:
			case early := <-result:
				t.Fatalf("search ended before generation lookup: %v", early.err)
			}
			service.Stop()
			count, closed := generationCacheState(cache)
			require.True(t, closed)
			require.Zero(t, count)
			require.True(t, service.stopping)
			if outcome == "cancellation" {
				cancel()
			}
			proceedOnce.Do(func() { close(source.proceed) })
			finished := <-result
			switch outcome {
			case "success":
				require.NoError(t, finished.err)
				require.Equal(t, first.Results, finished.report.Results)
				require.NotSame(t, warm, source.generation, "draining reads must decode without retaining the stopped cache entry")
				require.EqualValues(t, 1, loads.Load())
			case "load failure":
				require.ErrorIs(t, finished.err, loadFailure)
			case "cancellation":
				require.ErrorIs(t, finished.err, context.Canceled)
				require.Zero(t, loads.Load())
			}
			count, closed = generationCacheState(cache)
			require.True(t, closed)
			require.Zero(t, count)
			db, err := store.DefaultSQLiteDriver().Open(fixture.databasePath, docsqlite.OpenOptions{Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate})
			require.NoError(t, err)
			defer func() { require.NoError(t, db.Close()) }()
			var leases int
			require.NoError(t, db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM vector_index_reader_leases").Scan(&leases))
			require.Zero(t, leases, "draining reads must release their own query lease on every outcome")
		})
	}
}

func TestGenerationCacheStoppedReadsValidateMetadata(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"checksum", "size", "bytes"} {
		t.Run(kind, func(t *testing.T) {
			record := cacheGenerationFixture(t, "one", "a")
			loaded := record
			switch kind {
			case "checksum":
				loaded.IndexManifestChecksum = strings.Repeat("f", 64)
			case "size":
				loaded.ByteSize++
			case "bytes":
				loaded.Bytes = []byte("corrupt")
			}
			var calls int
			cache := newGenerationCache(func(_ context.Context, id string) (store.VectorIndexGenerationRecord, error) {
				calls++
				require.Equal(t, record.ID, id)
				return loaded, nil
			}, time.Minute)
			cache.Close()
			_, err := cache.Get(t.Context(), cacheHead(record))
			require.Error(t, err)
			require.Equal(t, 1, calls, "closed reads must still load and validate their exact leased generation")
			count, closed := generationCacheState(cache)
			require.True(t, closed)
			require.Zero(t, count)
		})
	}
}

type cacheDrainSource struct {
	cache            *generationCache
	entered, proceed chan struct{}
	generation       *vectorindex.Generation
}

func (source *cacheDrainSource) Get(ctx context.Context, record store.VectorIndexGenerationRecord) (*vectorindex.Generation, error) {
	close(source.entered)
	<-source.proceed
	generation, err := source.cache.Get(ctx, record)
	source.generation = generation
	return generation, err
}

type cacheDrainResult struct {
	report retrieval.Report
	err    error
}
