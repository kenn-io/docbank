package pdfproduction

import (
	"context"
	"errors"
	"runtime"
	"runtime/metrics"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/klippa-app/go-pdfium"
	"github.com/stretchr/testify/require"
)

func TestNativeEngineRSSMonitorCause(t *testing.T) {
	var over atomic.Bool
	recipe := defaultRecipe()
	recipe.QualifiedPeakRSSBytes = 1024
	engine, err := newPDFium(recipe, func(context.Context) (uint64, error) {
		if over.Load() {
			return 2048, nil
		}
		return 512, nil
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()
	entered := false
	err = engine.withInstanceContext(t.Context(), func(ctx context.Context, _ pdfium.Pdfium) error {
		entered = true
		over.Store(true)
		<-ctx.Done()
		cause, ok := errors.AsType[*RSSLimitError](context.Cause(ctx))
		require.True(t, ok)
		require.Equal(t, uint64(2048), cause.ObservedBytes)
		require.Equal(t, int64(1024), cause.LimitBytes)
		return ctx.Err()
	})
	require.True(t, entered)
	require.NoError(t, t.Context().Err())
	cause, ok := errors.AsType[*RSSLimitError](err)
	require.True(t, ok, "%v", err)
	require.Equal(t, uint64(2048), cause.ObservedBytes)
	require.Equal(t, int64(1024), cause.LimitBytes)
}

func TestNativeEnginePageDeadline(t *testing.T) {
	recipe := defaultRecipe()
	recipe.PageTimeoutSeconds = 1
	engine, err := newPDFium(recipe, func(context.Context) (uint64, error) { return 512, nil })
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()
	entered := false
	err = engine.withInstanceContext(t.Context(), func(ctx context.Context, _ pdfium.Pdfium) error {
		entered = true
		<-ctx.Done()
		return ctx.Err()
	})
	require.True(t, entered)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoError(t, t.Context().Err())
}

func TestRSSAdmissionFreesMemoryBeforeRefusing(t *testing.T) {
	start := forcedGCCount()
	reclaimable := func(context.Context) (uint64, error) {
		if forcedGCCount() == start {
			return 2048, nil
		}
		return 512, nil
	}
	_, stop, err := watchRSS(t.Context(), 1024, reclaimable)
	require.NoError(t, err)
	stop()

	_, _, err = watchRSS(t.Context(), 1024, func(context.Context) (uint64, error) { return 2048, nil })
	cause, ok := errors.AsType[*RSSLimitError](err)
	require.True(t, ok, "%v", err)
	require.Equal(t, uint64(2048), cause.ObservedBytes)
}

// Memory an earlier job used and the runtime has handed back must not count
// against the next job. macOS keeps returned pages in resident_size, so a
// worker that read that value refused every job after one large page.
func TestRSSAdmissionIgnoresMemoryReturnedToOS(t *testing.T) {
	baseline, err := readProcessRSS(t.Context())
	require.NoError(t, err)
	const garbage = 256 << 20
	touched := func() uint64 {
		chunks := make([][]byte, garbage/(8<<20))
		for i := range chunks {
			chunks[i] = make([]byte, 8<<20)
			for j := 0; j < len(chunks[i]); j += 4096 {
				chunks[i][j] = 1
			}
		}
		measured, err := readProcessRSS(t.Context())
		require.NoError(t, err)
		runtime.KeepAlive(chunks)
		return measured
	}()
	require.Greater(t, touched, baseline+garbage*3/4, "the reader must count memory the process is using")
	require.NoError(t, admitRSS(t.Context(), int64(baseline)+garbage/2, readProcessRSS))
}

func TestRSSMonitorFreesMemoryBeforeCancelling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := forcedGCCount()
		var running atomic.Bool
		reclaimable := func(context.Context) (uint64, error) {
			if running.Load() && forcedGCCount() == start {
				return 2048, nil
			}
			return 512, nil
		}
		ctx, stop, err := watchRSS(t.Context(), 1024, reclaimable)
		require.NoError(t, err)
		defer stop()
		running.Store(true)
		time.Sleep(50 * time.Millisecond)
		synctest.Wait()
		require.Greater(t, forcedGCCount(), start)
		require.NoError(t, context.Cause(ctx))
	})
}

func forcedGCCount() uint64 {
	sample := []metrics.Sample{{Name: "/gc/cycles/forced:gc-cycles"}}
	metrics.Read(sample)
	return sample[0].Value.Uint64()
}
