package processing

import (
	"context"
	"runtime"
	"runtime/pprof"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

// BenchmarkScopedSearchCPUProfile is a bounded query-only diagnostic.
// With -cpuprofile, filter search_scope=explicit4096 or search_scope=all_live
// to exclude fixture setup and compare equal query counts.
func BenchmarkScopedSearchCPUProfile(b *testing.B) {
	const documents = 10000
	fixture, service, _, job := newSearchScopeFixtureDimension(b, document.EmbeddingInputOriginalFile, 1024)
	source, err := fixture.catalog.NodeByPath(b.Context(), "/source.pdf")
	require.NoError(b, err)
	_, _, err = fixture.catalog.Trash(b.Context(), source.ID, source.Revision)
	require.NoError(b, err)
	versions := growSearchScopeWorkload(b, fixture, job, documents)
	for _, allLive := range []bool{false, true} {
		name := "explicit4096"
		if allLive {
			name = "all_live"
		}
		b.Run(name, func(b *testing.B) {
			request := searchScopeRequest(service)
			if !allLive {
				request.Scope = nil
				request.Fence.ContentVersionIDs = versions[:store.MaxSearchSourceFenceIDs]
			}
			labels := pprof.Labels("search_scope", name)
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				runtime.GC()
				b.StartTimer()
				var report SearchReport
				var err error
				pprof.Do(b.Context(), labels, func(ctx context.Context) { report, err = service.Search(ctx, request) })
				b.StopTimer()
				require.NoError(b, err)
				require.Len(b, report.Results, 10)
				if allLive {
					require.Equal(b, documents, report.ObservedScopeCount)
				}
				b.StartTimer()
			}
		})
	}
}
