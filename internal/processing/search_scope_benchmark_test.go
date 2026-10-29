package processing

import (
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
// Each iteration measures both paths, alternating their order. At 3x, each path
// has three samples. Standard ns/op and allocation counters are per pair.
// Both paths open the same complete exact generation on every query. *_p50_ns
// measures Service.Search; peak_heap_B samples heap objects every ms through
// runtime/metrics, using a reused sample slice rather than ReadMemStats.
// Fixture creation and forced collection before each sample are excluded.
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
			fixture, service, _, job := newSearchScopeFixtureDimension(b, tc.kind, 1024)
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
			var measurements [2]struct {
				samples     []time.Duration
				peak, extra uint64
			}
			pair := 0
			b.ReportAllocs()
			for b.Loop() {
				// Alternate AB/BA to reduce temporal host drift. Standard benchmark
				// timing and allocation counters describe a pair of searches.
				for _, path := range [2]int{pair % 2, 1 - pair%2} {
					b.StopTimer()
					runtime.GC()
					baseline := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
					metrics.Read(baseline)
					require.Equal(b, metrics.KindUint64, baseline[0].Value.Kind())
					before := baseline[0].Value.Uint64()
					var peak atomic.Uint64
					peak.Store(before)
					stop, done := make(chan struct{}), make(chan struct{})
					go func() {
						defer close(done)
						memory := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
						ticker := time.NewTicker(time.Millisecond)
						defer ticker.Stop()
						for {
							select {
							case <-ticker.C:
								metrics.Read(memory)
								observed := memory[0].Value.Uint64()
								for {
									old := peak.Load()
									if observed <= old || peak.CompareAndSwap(old, observed) {
										break
									}
								}
							case <-stop:
								return
							}
						}
					}()
					b.StartTimer()
					started := time.Now()
					report, err := service.Search(b.Context(), requests[path])
					elapsed := time.Since(started)
					b.StopTimer()
					close(stop)
					<-done
					require.NoError(b, err)
					require.Len(b, report.Results, 10)
					if path == 1 {
						require.Equal(b, tc.documents, report.ObservedScopeCount)
					}
					observed := peak.Load()
					measurements[path].peak = max(measurements[path].peak, observed)
					measurements[path].extra = max(measurements[path].extra, observed-before)
					measurements[path].samples = append(measurements[path].samples, elapsed)
					b.StartTimer()
				}
				pair++
			}
			b.StopTimer()
			var p50 [2]time.Duration
			for path, name := range [2]string{"fence4096", "all_live"} {
				measured := &measurements[path]
				b.Logf("path=%s raw=%v peak_heap=%d extra_heap=%d bytes", name, measured.samples, measured.peak, measured.extra)
				slices.Sort(measured.samples)
				p50[path] = measured.samples[len(measured.samples)/2]
				b.ReportMetric(float64(p50[path].Nanoseconds()), name+"_p50_ns")
				b.ReportMetric(float64(measured.peak), name+"_peak_heap_B")
				b.ReportMetric(float64(measured.extra), name+"_extra_heap_B")
			}
			fenceP50, scopeP50 := p50[0], p50[1]
			fencePeak, scopePeak := measurements[0].extra, measurements[1].extra
			extraHeap := int64(scopePeak) - int64(fencePeak)
			increment := scopeP50 - fenceP50
			b.Logf("all_live increment p50=%s peak_heap=%d bytes; direct budget=250ms/134217728 bytes", increment, extraHeap)
			if tc.kind == document.EmbeddingInputOriginalFile && (increment > 250*time.Millisecond || extraHeap > 128<<20) {
				b.Errorf("all_live increment exceeds direct-search budget: p50=%s heap=%d", increment, extraHeap)
			}
		})
	}
}
