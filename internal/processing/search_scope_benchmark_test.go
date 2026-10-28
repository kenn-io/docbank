package processing

import (
	"context"
	"runtime"
	"runtime/metrics"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

// Run explicitly with -run '^$' -bench BenchmarkAllLiveSemanticSearch -benchtime=3x.
// Each iteration measures two warm Service.Search calls, alternating their order.
// Standard ns/op and allocation counters are per warm pair. Cold samples use the
// same query counts and order, evicting only the decoded cache before each call;
// they are excluded from standard counters and reported separately. Provider
// work is synthetic/local. All authority checks remain in Service.Search.
//
// resident_cache_heap_B is the post-GC heap increase from an empty cache to one
// warmed generation. It is separate from peak_heap_B (absolute process Go heap)
// and extra_heap_B (sampled query peak minus that query's post-GC baseline).
// Peaks sample heap objects every ms through runtime/metrics; short peaks can be
// missed. Forced collection does not evict a retained decoded generation.
func BenchmarkAllLiveSemanticSearch(b *testing.B) {
	if testing.Short() {
		b.Skip("large synthetic vector-index benchmark")
	}
	b.Logf("driver=%s Go=%s GOMAXPROCS=%d", store.DefaultSQLiteDriver().Name(), runtime.Version(), runtime.GOMAXPROCS(0))
	for _, tc := range []struct {
		name      string
		kind      document.EmbeddingInputKind
		documents int
	}{
		{"50000x1x1024", document.EmbeddingInputOriginalFile, 50000},
		{"chunked_near_512MiB", document.EmbeddingInputRenditionChunk, 20000},
	} {
		b.Run(tc.name, func(b *testing.B) {
			fixture, service, job := newSearchBenchmarkFixture(b, tc.kind, 1024)
			if tc.kind == document.EmbeddingInputOriginalFile {
				source, err := fixture.catalog.NodeByPath(b.Context(), "/source.pdf")
				require.NoError(b, err)
				_, _, err = fixture.catalog.Trash(b.Context(), source.ID, source.Revision)
				require.NoError(b, err)
			}
			versions := growSearchScopeWorkload(b, fixture, job, tc.documents)
			requests := [2]SearchRequest{searchScopeRequest(service), searchScopeRequest(service)}
			requests[0].Scope = nil
			requests[0].Fence.ContentVersionIDs = versions[:store.MaxSearchSourceFenceIDs]
			cache := service.generationSource()
			var loads atomic.Uint64
			load := cache.load
			cache.load = func(ctx context.Context, id string) (store.VectorIndexGenerationRecord, error) {
				loads.Add(1)
				return load(ctx, id)
			}
			// Instrument the production cache's loader, without replacing the
			// cache or disabling its eviction/lifecycle behavior.
			runtime.GC()
			emptyHeap := searchBenchmarkHeap()
			for _, request := range requests {
				report, err := service.Search(b.Context(), request)
				require.NoError(b, err)
				require.Len(b, report.Results, 10)
			}
			require.EqualValues(b, 1, loads.Load(), "warm-up must load/decode exactly once")
			entry := searchBenchmarkEntry(b, cache)
			generationBytes := entry.record.ByteSize
			runtime.GC()
			residentHeap := searchBenchmarkHeap()
			residentCache := int64(residentHeap) - int64(emptyHeap)
			b.Logf("generation_bytes=%d retained_entries=1 retained_blob_bytes=0 empty_heap=%d resident_heap=%d resident_cache_heap=%d bytes", generationBytes, emptyHeap, residentHeap, residentCache)
			b.ReportMetric(float64(residentCache), "resident_cache_heap_B")
			// Verify pointer reuse during warm samples. GC liveness ends after
			// the last warm use, before the cold samples.
			warmGeneration := entry.generation
			var warm, cold [2]searchBenchmarkMeasurements
			pair := 0
			b.ReportAllocs()
			for b.Loop() {
				// AB/BA reduces temporal host drift. Collection and validation
				// stay outside the standard benchmark timer.
				for _, path := range [2]int{pair % 2, 1 - pair%2} {
					b.StopTimer()
					measureSearchBenchmark(b, service, requests[path], tc.documents, path, &warm[path], true)
					require.Same(b, warmGeneration, searchBenchmarkEntry(b, cache).generation)
					b.StartTimer()
				}
				pair++
			}
			b.StopTimer()
			require.EqualValues(b, 1, loads.Load(), "warm searches must perform zero blob loads and decodes")
			b.Logf("warm_pairs=%d warm_blob_loads=0 warm_decodes=0 retained_entries=1 retained_blob_bytes=0", pair)
			for sample := range pair {
				for _, path := range [2]int{sample % 2, 1 - sample%2} {
					// Evict only the cache, leaving the measured catalog/schema and
					// data unchanged. A fresh miss uses the same production loader.
					cache.mu.Lock()
					for _, space := range cache.spaces {
						if space.entry != nil {
							space.entry.timer.Stop()
						}
					}
					clear(cache.spaces)
					cache.mu.Unlock()
					before := loads.Load()
					measureSearchBenchmark(b, service, requests[path], tc.documents, path, &cold[path], false)
					require.Equal(b, before+1, loads.Load(), "each cold search must load/decode once")
					searchBenchmarkEntry(b, cache)
				}
			}
			b.Logf("cold_pairs=%d cold_blob_loads=%d cold_decodes=%d", pair, loads.Load()-1, loads.Load()-1)
			var p50 [2]time.Duration
			for path, name := range [2]string{"fence4096", "all_live"} {
				for phase, measured := range map[string]*searchBenchmarkMeasurements{"warm": &warm[path], "cold": &cold[path]} {
					b.Logf("path=%s phase=%s raw=%v peak_heap=%d extra_heap=%d bytes", name, phase, measured.samples, measured.peak, measured.extra)
					slices.Sort(measured.samples)
					median := measured.samples[len(measured.samples)/2]
					b.ReportMetric(float64(median.Nanoseconds()), name+"_"+phase+"_p50_ns")
					b.ReportMetric(float64(measured.peak), name+"_"+phase+"_peak_heap_B")
					b.ReportMetric(float64(measured.extra), name+"_"+phase+"_extra_heap_B")
					if phase == "warm" {
						p50[path] = median
					}
				}
			}
			extraHeap := int64(warm[1].extra) - int64(warm[0].extra)
			increment := p50[1] - p50[0]
			b.Logf("all_live warm increment p50=%s peak_heap=%d bytes; direct budget=250ms/134217728 bytes; absolute warm budget=300ms", increment, extraHeap)
			if tc.kind == document.EmbeddingInputOriginalFile {
				if increment > 250*time.Millisecond || extraHeap > 128<<20 {
					b.Errorf("all_live increment exceeds direct-search budget: p50=%s heap=%d", increment, extraHeap)
				}
				if p50[1] > 300*time.Millisecond {
					b.Errorf("all_live warm Service.Search exceeds absolute 300ms p50 target: %s", p50[1])
				}
			}
		})
	}
}

type searchBenchmarkMeasurements struct {
	samples     []time.Duration
	peak, extra uint64
}

func searchBenchmarkHeap() uint64 {
	memory := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(memory)
	return memory[0].Value.Uint64()
}

func searchBenchmarkEntry(tb testing.TB, cache *generationCache) *generationCacheEntry {
	tb.Helper()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	require.Len(tb, cache.spaces, 1)
	for _, space := range cache.spaces {
		require.Empty(tb, space.flights)
		require.NotNil(tb, space.entry)
		require.Nil(tb, space.entry.record.Bytes)
		require.NotNil(tb, space.entry.generation)
		return space.entry
	}
	tb.Fatal("missing retained generation")
	return nil
}

func measureSearchBenchmark(b *testing.B, service *Service, request SearchRequest, documents, path int, measured *searchBenchmarkMeasurements, timed bool) {
	b.Helper()
	runtime.GC()
	before := searchBenchmarkHeap()
	var peak atomic.Uint64
	peak.Store(before)
	memory := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	observe := func() {
		metrics.Read(memory)
		observed := memory[0].Value.Uint64()
		for {
			old := peak.Load()
			if observed <= old || peak.CompareAndSwap(old, observed) {
				return
			}
		}
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				observe()
			case <-stop:
				return
			}
		}
	}()
	if timed {
		b.StartTimer()
	}
	started := time.Now()
	report, err := service.Search(b.Context(), request)
	elapsed := time.Since(started)
	if timed {
		b.StopTimer()
	}
	close(stop)
	<-done
	observe()
	require.NoError(b, err)
	require.Len(b, report.Results, 10)
	if path == 1 {
		require.Equal(b, documents, report.ObservedScopeCount)
	}
	observed := peak.Load()
	measured.peak = max(measured.peak, observed)
	measured.extra = max(measured.extra, observed-before)
	measured.samples = append(measured.samples, elapsed)
}
