package processing

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorindex"
	docsqlite "go.kenn.io/docbank/sqlite"
)

func cacheGenerationFixture(tb testing.TB, space, label string) store.VectorIndexGenerationRecord {
	tb.Helper()
	set := document.VectorSetV1{VectorSpaceFingerprint: workerHash(space), Metric: document.VectorMetricDotProduct,
		Normalization: document.VectorNormalizationNone, Dimension: 2, InputKeys: []string{label},
		InputChecksums: []string{workerHash(label)}, Vectors: [][]float32{{1, 2}}}
	_, setID, err := document.EncodeVectorSetV1(set)
	require.NoError(tb, err)
	manifest, err := vectorindex.NewManifest([]string{setID})
	require.NoError(tb, err)
	generation, err := vectorindex.BuildGeneration(manifest, []document.VectorSetV1{set}, vectorindex.Options{})
	require.NoError(tb, err)
	return store.VectorIndexGenerationRecord{ID: workerHash(space + label), VectorSpaceID: workerHash(space),
		SourceManifestChecksum: workerHash("source" + label), IndexManifestChecksum: manifest.Checksum,
		Bytes: generation.Bytes(), ByteSize: len(generation.Bytes()), RowCount: 1, BuiltAt: "2026-10-28T00:00:00.000000000Z"}
}

func cacheHead(record store.VectorIndexGenerationRecord) store.VectorIndexGenerationRecord {
	record.Bytes = nil
	return record
}

func TestGenerationCacheReuseReplacementAndSpaces(t *testing.T) {
	t.Parallel()
	a, b, c := cacheGenerationFixture(t, "one", "a"), cacheGenerationFixture(t, "one", "b"), cacheGenerationFixture(t, "two", "c")
	records := map[string]store.VectorIndexGenerationRecord{a.ID: a, b.ID: b, c.ID: c}
	var loads int
	cache := newGenerationCache(func(_ context.Context, id string) (store.VectorIndexGenerationRecord, error) {
		loads++
		return records[id], nil
	}, time.Minute)
	defer cache.Close()
	first, err := cache.Get(t.Context(), cacheHead(a))
	require.NoError(t, err)
	again, err := cache.Get(t.Context(), cacheHead(a))
	require.NoError(t, err)
	require.Same(t, first, again)
	require.Equal(t, 1, loads)
	other, err := cache.Get(t.Context(), cacheHead(c))
	require.NoError(t, err)
	require.Equal(t, c.VectorSpaceID, other.Metadata().VectorSpaceID)
	next, err := cache.Get(t.Context(), cacheHead(b))
	require.NoError(t, err)
	require.NotSame(t, first, next)
	again, err = cache.Get(t.Context(), cacheHead(c))
	require.NoError(t, err)
	require.Same(t, other, again)
	firstAgain, err := cache.Get(t.Context(), cacheHead(a))
	require.NoError(t, err)
	require.NotSame(t, first, firstAgain)
	require.Equal(t, 4, loads)
}

func TestGenerationCacheConcurrentColdMissAndWaiterCancellation(t *testing.T) {
	t.Parallel()
	record := cacheGenerationFixture(t, "one", "a")
	started, finish := make(chan struct{}), make(chan struct{})
	var loads atomic.Int32
	cache := newGenerationCache(func(ctx context.Context, id string) (store.VectorIndexGenerationRecord, error) {
		if id != record.ID {
			return store.VectorIndexGenerationRecord{}, fmt.Errorf("loaded wrong ID %s", id)
		}
		loads.Add(1)
		close(started)
		select {
		case <-finish:
			return record, nil
		case <-ctx.Done():
			return store.VectorIndexGenerationRecord{}, ctx.Err()
		}
	}, time.Minute)
	defer cache.Close()
	result := make(chan generationCacheResult, 1)
	go func() {
		g, err := cache.Get(t.Context(), cacheHead(record))
		result <- generationCacheResult{generation: g, err: err}
	}()
	<-started
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := cache.Get(canceled, cacheHead(record))
	require.ErrorIs(t, err, context.Canceled)
	var wg sync.WaitGroup
	pointers := make(chan generationCacheResult, 16)
	for range 16 {
		wg.Go(func() {
			g, err := cache.Get(t.Context(), cacheHead(record))
			pointers <- generationCacheResult{generation: g, err: err}
		})
	}
	close(finish)
	first := <-result
	require.NoError(t, first.err)
	wg.Wait()
	close(pointers)
	for g := range pointers {
		require.NoError(t, g.err)
		require.Same(t, first.generation, g.generation)
	}
	require.EqualValues(t, 1, loads.Load())
}

func TestGenerationCacheLateOldLoadCannotReplaceNewHead(t *testing.T) {
	t.Parallel()
	old, newer := cacheGenerationFixture(t, "one", "old"), cacheGenerationFixture(t, "one", "new")
	started, finish := make(chan struct{}), make(chan struct{})
	var loads atomic.Int32
	cache := newGenerationCache(func(_ context.Context, id string) (store.VectorIndexGenerationRecord, error) {
		loads.Add(1)
		if id == old.ID {
			close(started)
			<-finish
			return old, nil
		}
		return newer, nil
	}, time.Minute)
	defer cache.Close()
	done := make(chan generationCacheResult, 1)
	go func() {
		g, err := cache.Get(t.Context(), cacheHead(old))
		done <- generationCacheResult{generation: g, err: err}
	}()
	<-started
	current, err := cache.Get(t.Context(), cacheHead(newer))
	require.NoError(t, err)
	close(finish)
	prior := <-done
	require.NoError(t, prior.err)
	require.Equal(t, old.IndexManifestChecksum, prior.generation.Metadata().Manifest.Checksum)
	again, err := cache.Get(t.Context(), cacheHead(newer))
	require.NoError(t, err)
	require.Same(t, current, again)
	require.EqualValues(t, 2, loads.Load())
}

func TestGenerationCacheRetryAndAuthorityMismatch(t *testing.T) {
	t.Parallel()
	record := cacheGenerationFixture(t, "one", "a")
	loads := 0
	cache := newGenerationCache(func(_ context.Context, _ string) (store.VectorIndexGenerationRecord, error) {
		loads++
		if loads == 1 {
			return store.VectorIndexGenerationRecord{}, errors.New("load failure")
		}
		return record, nil
	}, time.Minute)
	defer cache.Close()
	_, err := cache.Get(t.Context(), cacheHead(record))
	require.ErrorContains(t, err, "load failure")
	g, err := cache.Get(t.Context(), cacheHead(record))
	require.NoError(t, err)
	head := cacheHead(record)
	head.IndexManifestChecksum = strings.Repeat("f", 64)
	_, err = cache.Get(t.Context(), head)
	require.Error(t, err, "warm hit must compare fresh leased metadata")
	again, err := cache.Get(t.Context(), cacheHead(record))
	require.NoError(t, err)
	require.Same(t, g, again)
	require.Equal(t, 2, loads)
}

func TestGenerationCacheIdleEvictionAndStop(t *testing.T) {
	t.Parallel()
	record := cacheGenerationFixture(t, "one", "a")
	var loads atomic.Int32
	cache := newGenerationCache(func(_ context.Context, _ string) (store.VectorIndexGenerationRecord, error) {
		loads.Add(1)
		return record, nil
	}, 10*time.Millisecond)
	first, err := cache.Get(t.Context(), cacheHead(record))
	require.NoError(t, err)
	require.Eventually(t, func() bool { cache.mu.Lock(); defer cache.mu.Unlock(); return len(cache.spaces) == 0 }, time.Second, time.Millisecond, "idle timers must release references without another query")
	next, err := cache.Get(t.Context(), cacheHead(record))
	require.NoError(t, err)
	require.NotSame(t, first, next)
	require.EqualValues(t, 2, loads.Load())
	cache.Close()
	cache.Close()
	drained, err := cache.Get(t.Context(), cacheHead(record))
	require.NoError(t, err)
	require.NotSame(t, next, drained)
	require.EqualValues(t, 3, loads.Load())
	count, _ := generationCacheState(cache)
	require.Zero(t, count)
}

func TestGenerationCacheStopDuringLoad(t *testing.T) {
	t.Parallel()
	record := cacheGenerationFixture(t, "one", "a")
	started, finish := make(chan struct{}), make(chan struct{})
	var began sync.Once
	cache := newGenerationCache(func(_ context.Context, _ string) (store.VectorIndexGenerationRecord, error) {
		began.Do(func() { close(started) })
		<-finish
		return record, nil
	}, time.Minute)
	done := make(chan struct{})
	go func() { defer close(done); _, _ = cache.Get(t.Context(), cacheHead(record)) }()
	<-started
	cache.Close()
	close(finish)
	<-done
	count, _ := generationCacheState(cache)
	require.Zero(t, count)
	drained, err := cache.Get(t.Context(), cacheHead(record))
	require.NoError(t, err)
	require.Equal(t, record.IndexManifestChecksum, drained.Metadata().Manifest.Checksum)
	count, _ = generationCacheState(cache)
	require.Zero(t, count)
}

func TestGenerationCacheCanceledLoaderCanRetry(t *testing.T) {
	t.Parallel()
	record := cacheGenerationFixture(t, "one", "a")
	started := make(chan struct{})
	var loads atomic.Int32
	cache := newGenerationCache(func(ctx context.Context, _ string) (store.VectorIndexGenerationRecord, error) {
		if loads.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			return store.VectorIndexGenerationRecord{}, ctx.Err()
		}
		return record, nil
	}, time.Minute)
	defer cache.Close()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := cache.Get(ctx, cacheHead(record)); done <- err }()
	<-started
	waiter := make(chan error, 1)
	go func() { _, err := cache.Get(t.Context(), cacheHead(record)); waiter <- err }()
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.NoError(t, <-waiter)
	require.EqualValues(t, 2, loads.Load())
}

func FuzzGenerationCacheIdentity(f *testing.F) {
	f.Add([]byte{0, 1, 6, 2, 3, 4, 5, 0, 1})
	f.Fuzz(func(t *testing.T, sequence []byte) {
		if len(sequence) > 64 {
			sequence = sequence[:64]
		}
		records := make([]store.VectorIndexGenerationRecord, 4)
		byID := make(map[string]store.VectorIndexGenerationRecord)
		for i := range records {
			records[i] = cacheGenerationFixture(t, strconv.Itoa(i/2), strconv.Itoa(i))
			byID[records[i].ID] = records[i]
		}
		cache := newGenerationCache(func(_ context.Context, id string) (store.VectorIndexGenerationRecord, error) { return byID[id], nil }, time.Minute)
		defer func() { cache.Close() }()
		for _, op := range sequence {
			if op%7 == 6 {
				cache.Close()
				requested := records[int(op)%len(records)]
				transient, err := cache.Get(t.Context(), cacheHead(requested))
				require.NoError(t, err)
				require.Equal(t, requested.IndexManifestChecksum, transient.Metadata().Manifest.Checksum)
				count, closed := generationCacheState(cache)
				require.True(t, closed)
				require.Zero(t, count)
				cache = newGenerationCache(func(_ context.Context, id string) (store.VectorIndexGenerationRecord, error) { return byID[id], nil }, time.Minute)
				continue
			}
			requested := records[int(op)%len(records)]
			g, err := cache.Get(t.Context(), cacheHead(requested))
			require.NoError(t, err)
			require.Equal(t, requested.VectorSpaceID, g.Metadata().VectorSpaceID)
			require.Equal(t, requested.IndexManifestChecksum, g.Metadata().Manifest.Checksum)
		}
	})
}

func TestServiceGenerationReuseFreshAuthorityAndStop(t *testing.T) {
	t.Parallel()
	fixture, service, _, _ := newSearchScopeFixture(t)
	// Directly constructed services must use the same cache and lifecycle as NewService.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	service.lifecycle, service.stop, service.drained = ctx, cancel, make(chan struct{})
	t.Cleanup(service.Stop)
	request := SearchRequest{Query: "synthetic", Mode: "semantic", Profile: "private", Limit: 10, Fence: SourceFence{VaultUID: fixture.catalog.VaultID()}, Scope: &SearchScope{Kind: "all_live"}}
	first, err := service.Search(t.Context(), request)
	require.NoError(t, err)
	require.NotEmpty(t, first.Results)
	cache := service.generationSource()
	spaces, err := fixture.catalog.ListVectorIndexSpaces(t.Context())
	require.NoError(t, err)
	space := spaces[0]
	head, err := fixture.catalog.AcquireVectorIndexGenerationHead(t.Context(), space, "test", time.Now().UTC(), time.Minute)
	require.NoError(t, err)
	require.Nil(t, head.Generation.Bytes)
	require.NoError(t, fixture.catalog.ReleaseVectorIndexGeneration(t.Context(), head.ID, head.FencingToken, time.Now().UTC()))
	decoded, err := cache.Get(t.Context(), head.Generation)
	require.NoError(t, err)
	db, err := store.DefaultSQLiteDriver().Open(fixture.databasePath, docsqlite.OpenOptions{Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate})
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	// Warm searches must not read or reopen serialized generation data.
	_, err = db.ExecContext(t.Context(), "UPDATE vector_index_generations SET generation_bytes=zeroblob(byte_size) WHERE generation_id=?", head.Generation.ID)
	require.NoError(t, err)
	warm, err := service.Search(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, first.Results, warm.Results)
	again, err := cache.Get(t.Context(), head.Generation)
	require.NoError(t, err)
	require.Same(t, decoded, again)
	// A warm hit must not hide a valid-but-changed declared byte size.
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = conn.ExecContext(t.Context(), "PRAGMA ignore_check_constraints=ON")
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "UPDATE vector_index_generations SET byte_size=byte_size+1 WHERE generation_id=?", head.Generation.ID)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "PRAGMA ignore_check_constraints=OFF")
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	_, err = service.Search(t.Context(), request)
	require.Error(t, err, "warm hit must reject changed declared byte size")
	_, err = db.ExecContext(t.Context(), "UPDATE vector_index_generations SET byte_size=byte_size-1 WHERE generation_id=?", head.Generation.ID)
	require.NoError(t, err)
	// Coverage and filters remain fresh while immutable decoded vectors are reused.
	_, err = fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), "synthetic-unembedded.txt", workerHash("unembedded"), 1, "text/plain")
	require.NoError(t, err)
	changed, err := service.Search(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, first.Coverage.ScopedDocuments+1, changed.Coverage.ScopedDocuments)
	request.Scope.Filters = &store.SearchOptions{MIMEType: "text/plain"}
	filtered, err := service.Search(t.Context(), request)
	require.NoError(t, err)
	require.Empty(t, filtered.Results)
	request.Scope.Filters = nil
	_, err = db.ExecContext(t.Context(), "UPDATE vector_index_generations SET index_manifest_checksum=? WHERE generation_id=?", strings.Repeat("f", 64), head.Generation.ID)
	require.NoError(t, err)
	_, err = service.Search(t.Context(), request)
	require.Error(t, err, "warm hit must reject incompatible fresh leased metadata")
	_, err = db.ExecContext(t.Context(), "UPDATE vector_index_generations SET index_manifest_checksum=? WHERE generation_id=?", head.Generation.IndexManifestChecksum, head.Generation.ID)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), "DELETE FROM embedding_heads WHERE vector_space_id=?", space)
	require.NoError(t, err)
	_, err = service.Search(t.Context(), request)
	require.Error(t, err, "warm hit must reject stale source membership")
	var leases int
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM vector_index_reader_leases").Scan(&leases))
	require.Zero(t, leases)
	service.Stop()
	count, closed := generationCacheState(cache)
	require.True(t, closed)
	require.Zero(t, count)
	_, err = cache.Get(t.Context(), head.Generation)
	require.Error(t, err)
}

func TestServiceGenerationReuseConcurrentSearches(t *testing.T) {
	t.Parallel()
	fixture, service, _, _ := newSearchScopeFixture(t)
	request := SearchRequest{Query: "synthetic", Mode: "semantic", Profile: "private", Limit: 10, Fence: SourceFence{VaultUID: fixture.catalog.VaultID()}, Scope: &SearchScope{Kind: "all_live"}}
	var wg sync.WaitGroup
	results := make(chan error, 6)
	for range 6 {
		wg.Go(func() {
			report, err := service.Search(t.Context(), request)
			if err == nil && len(report.Results) == 0 {
				err = errors.New("missing semantic results")
			}
			results <- err
		})
	}
	wg.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
}

// generationCacheResult keeps failures in the test goroutine, including when
// a loader fails before publishing a result to its waiters.
type generationCacheResult struct {
	generation *vectorindex.Generation
	err        error
}

func generationCacheState(cache *generationCache) (int, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return len(cache.spaces), cache.closed
}

func TestGenerationCacheOldWaiterKeepsNewHead(t *testing.T) {
	t.Parallel()
	for _, lateJoin := range []bool{false, true} {
		name := "canceled leader retry"
		if lateJoin {
			name = "late old flight join"
		}
		t.Run(name, func(t *testing.T) {
			old, newer := cacheGenerationFixture(t, "one", "old"), cacheGenerationFixture(t, "one", "new")
			started := make(chan struct{})
			var oldLoads, newLoads atomic.Int32
			cache := newGenerationCache(func(ctx context.Context, id string) (store.VectorIndexGenerationRecord, error) {
				switch id {
				case old.ID:
					if oldLoads.Add(1) == 1 {
						close(started)
						<-ctx.Done()
						return store.VectorIndexGenerationRecord{}, ctx.Err()
					}
					return old, nil
				case newer.ID:
					newLoads.Add(1)
					return newer, nil
				default:
					return store.VectorIndexGenerationRecord{}, fmt.Errorf("unexpected generation %s", id)
				}
			}, time.Minute)
			leaderCtx, cancel := context.WithCancel(t.Context())
			var queries sync.WaitGroup
			t.Cleanup(func() { cancel(); cache.Close(); queries.Wait() })
			start := func(ctx context.Context, record store.VectorIndexGenerationRecord) <-chan generationCacheResult {
				result := make(chan generationCacheResult, 1)
				queries.Go(func() {
					g, err := cache.Get(ctx, cacheHead(record))
					result <- generationCacheResult{generation: g, err: err}
				})
				return result
			}
			leader := start(leaderCtx, old)
			select {
			case <-started:
			case result := <-leader:
				t.Fatalf("leader returned before loading: %v", result.err)
			}
			earlyCtx := &cacheFlightWaitContext{Context: t.Context(), waiting: make(chan struct{})}
			waiter := start(earlyCtx, old)
			waitForCacheFlight(t, earlyCtx, waiter)
			current, err := cache.Get(t.Context(), cacheHead(newer))
			require.NoError(t, err)
			var late <-chan generationCacheResult
			if lateJoin {
				lateCtx := &cacheFlightWaitContext{Context: t.Context(), waiting: make(chan struct{})}
				late = start(lateCtx, old)
				waitForCacheFlight(t, lateCtx, late)
				retained, err := cache.Get(t.Context(), cacheHead(newer))
				require.NoError(t, err)
				require.Same(t, current, retained, "joining older work must not evict the newer head")
			}
			cancel()
			require.ErrorIs(t, (<-leader).err, context.Canceled)
			result := <-waiter
			require.NoError(t, result.err)
			require.Equal(t, old.IndexManifestChecksum, result.generation.Metadata().Manifest.Checksum, "older waiter must still receive its own leased generation")
			if late != nil {
				result := <-late
				require.NoError(t, result.err)
				require.Equal(t, old.IndexManifestChecksum, result.generation.Metadata().Manifest.Checksum)
			}
			retained, err := cache.Get(t.Context(), cacheHead(newer))
			require.NoError(t, err)
			require.Same(t, current, retained, "retrying older work must not replace the newer head")
			require.EqualValues(t, 1, newLoads.Load())
			require.GreaterOrEqual(t, oldLoads.Load(), int32(2))
		})
	}
}

// Done is evaluated when Get enters the flight wait. This lets the regression
// establish a waiter without sleeps or production-only test hooks.
type cacheFlightWaitContext struct {
	context.Context

	once    sync.Once
	waiting chan struct{}
}

func (ctx *cacheFlightWaitContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.waiting) })
	return ctx.Context.Done()
}
func waitForCacheFlight(t *testing.T, ctx *cacheFlightWaitContext, result <-chan generationCacheResult) {
	t.Helper()
	select {
	case <-ctx.waiting:
	case returned := <-result:
		t.Fatalf("query returned without joining the old flight: %v", returned.err)
	}
}
