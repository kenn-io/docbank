package pdfproduction

import (
	"context"
	"image"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNativeVisibilityAmbiguityStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := nativeGlyphAmbiguity(ctx, image.Rect(0, 0, 100, 100), []image.Rectangle{image.Rect(0, 0, 100, 100)})
	require.ErrorIs(t, err, context.Canceled)
}
