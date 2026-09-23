package pdfproduction

import (
	"image"
	"image/color"
	"image/draw"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
)

func whiteRaster(t *testing.T) (Raster, redaction.Recipe) {
	t.Helper()
	recipe, err := QualifiedRecipeForDPI(300)
	require.NoError(t, err)
	pixels := image.NewNRGBA(image.Rect(0, 0, 300, 300))
	draw.Draw(pixels, pixels.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	return Raster{Pixels: pixels, Page: redaction.Page{Number: 1, FrameSHA256: digest([]byte("burn frame")), Width: 10000, Height: 10000}, DPI: 300}, recipe
}

func TestBurnDestroysPixels(t *testing.T) {
	raster, recipe := whiteRaster(t)
	box := redaction.Box{Page: 1, FrameSHA256: raster.Page.FrameSHA256, X0: 2000, Y0: 2000, X1: 4000, Y1: 4000}
	out, err := Burn(raster, []redaction.Box{box}, recipe)
	require.NoError(t, err)
	require.Equal(t, color.NRGBA{A: 255}, out.Pixels.NRGBAAt(75, 75))
	require.Equal(t, color.NRGBA{R: 255, G: 255, B: 255, A: 255}, out.Pixels.NRGBAAt(150, 150))
	require.Equal(t, color.NRGBA{R: 255, G: 255, B: 255, A: 255}, raster.Pixels.NRGBAAt(75, 75))
	require.Equal(t, color.NRGBA{R: 255, G: 255, B: 255, A: 255}, out.Pixels.NRGBAAt(59, 75), "padding was already resolved")
}

func TestPlanPixelsRoundsOutwardAndRejectsInvalidFrames(t *testing.T) {
	raster, recipe := whiteRaster(t)
	box := redaction.Box{Page: 1, FrameSHA256: raster.Page.FrameSHA256, X0: 2001, Y0: 2034, X1: 3999, Y1: 4001}
	rectangles, err := PlanPixels(raster.Page, []redaction.Box{box}, recipe)
	require.NoError(t, err)
	require.Equal(t, []image.Rectangle{image.Rect(60, 61, 120, 121)}, rectangles)
	for _, mutate := range []func(*redaction.Box){
		func(b *redaction.Box) { b.Page = 2 }, func(b *redaction.Box) { b.FrameSHA256 = digest([]byte("stale")) },
		func(b *redaction.Box) { b.X0 = -1 }, func(b *redaction.Box) { b.X1 = 10001 }, func(b *redaction.Box) { b.Y1 = b.Y0 },
	} {
		invalid := box
		mutate(&invalid)
		_, err := PlanPixels(raster.Page, []redaction.Box{invalid}, recipe)
		require.Error(t, err)
	}
}

func TestBurnValidatesRasterAndFlattensAlpha(t *testing.T) {
	raster, recipe := whiteRaster(t)
	raster.Pixels.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 0})
	out, err := Burn(raster, nil, recipe)
	require.NoError(t, err)
	require.Equal(t, color.NRGBA{R: 255, G: 255, B: 255, A: 255}, out.Pixels.NRGBAAt(0, 0))
	for _, mutate := range []func(*Raster){func(r *Raster) { r.Pixels = nil }, func(r *Raster) { r.DPI = 600 }, func(r *Raster) { r.Pixels = image.NewNRGBA(image.Rect(1, 1, 301, 301)) }, func(r *Raster) { r.Pixels.Pix = r.Pixels.Pix[:1] }} {
		r, _ := whiteRaster(t)
		mutate(&r)
		_, err := Burn(r, nil, recipe)
		require.Error(t, err)
	}
	recipe.FontSHA256 = ""
	_, err = Burn(raster, nil, recipe)
	require.Error(t, err)
}
