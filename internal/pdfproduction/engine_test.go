package pdfproduction

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

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
	err = engine.(*pdfiumEngine).withInstanceContext(t.Context(), func(ctx context.Context, _ pdfium.Pdfium) error {
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
	err = engine.(*pdfiumEngine).withInstanceContext(t.Context(), func(ctx context.Context, _ pdfium.Pdfium) error {
		entered = true
		<-ctx.Done()
		return ctx.Err()
	})
	require.True(t, entered)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoError(t, t.Context().Err())
}
