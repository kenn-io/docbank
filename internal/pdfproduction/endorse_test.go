package pdfproduction

import (
	"image"
	"image/color"
	"image/draw"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"golang.org/x/image/font/opentype"
)

func TestEndorseRejectsFixedPointTextAdvanceOverflow(t *testing.T) {
	r, l, es, recipe := endorsementFixture(t)
	parsed, err := opentype.Parse(fontBytes)
	require.NoError(t, err)
	face, err := opentype.NewFace(parsed, &opentype.FaceOptions{Size: 20, DPI: 300})
	require.NoError(t, err)
	_, advance, ok := face.GlyphBounds('W')
	require.True(t, ok)
	require.NoError(t, face.Close())
	// Choose enough glyphs to wrap the dependency's signed 26.6 advance
	// through 2^32 back into the small mask's apparent width.
	count := ((int64(1) << 32) + int64(advance) - 1) / int64(advance)
	require.Less(t, count, int64(maxTextBytes))
	es[0].Text = strings.Repeat("W", int(count))
	es[0].FontSizeMilliPoints = 20000
	_, err = Endorse(r, l, es[:1], recipe)
	require.ErrorContains(t, err, "endorsement_layout_conflict")
}

func TestEndorseFractionalStripNeverTouchesSourcePixels(t *testing.T) {
	r, recipe := whiteRaster(t)
	r.Page.Height = 10001
	r.Pixels = image.NewNRGBA(image.Rect(0, 0, 300, 301))
	draw.Draw(r.Pixels, r.Pixels.Bounds(), image.NewUniform(color.NRGBA{R: 120, G: 50, B: 20, A: 255}), image.Point{}, draw.Src)
	r, err := Burn(r, nil, recipe)
	require.NoError(t, err)
	output := r.Page
	output.Height += 5000
	output.FrameSHA256 = digest([]byte("fractional strip"))
	l := redaction.PageLayout{Source: r.Page, Output: output, StripHeight: 5000}
	e := redaction.Endorsement{Kind: "number", Text: "HHHHHH", FontSHA256: fontSHA256, FontSizeMilliPoints: 8000, Color: "#000000", Box: redaction.Box{Page: 1, FrameSHA256: output.FrameSHA256, X0: 0, Y0: 10001, X1: 10000, Y1: 15001}}
	out, err := Endorse(r, l, []redaction.Endorsement{e}, recipe)
	require.NoError(t, err)
	for x := range 300 {
		require.Equal(t, r.Pixels.NRGBAAt(x, 300), out.Pixels.NRGBAAt(x, 300))
	}
}

func endorsementFixture(t *testing.T) (Raster, redaction.PageLayout, []redaction.Endorsement, redaction.Recipe) {
	t.Helper()
	r, recipe := whiteRaster(t)
	mask := redaction.Box{Page: 1, FrameSHA256: r.Page.FrameSHA256, X0: 1000, Y0: 1000, X1: 9000, Y1: 4000}
	r, err := Burn(r, []redaction.Box{mask}, recipe)
	require.NoError(t, err)
	output := r.Page
	output.Height += 5000
	output.FrameSHA256 = digest([]byte("endorsement output"))
	labelBox := mask
	labelBox.FrameSHA256 = output.FrameSHA256
	return r, redaction.PageLayout{Source: r.Page, Output: output, StripHeight: 5000}, []redaction.Endorsement{
		{Kind: "label", Text: "PUBLIC", FontSHA256: fontSHA256, Color: "#ffffff", Box: labelBox, FontSizeMilliPoints: 8000},
		{Kind: "number", Text: "ABC-001", FontSHA256: fontSHA256, Color: "#000000", Box: redaction.Box{Page: 1, FrameSHA256: output.FrameSHA256, X0: 1000, Y0: 10500, X1: 9000, Y1: 14500}, FontSizeMilliPoints: 8000},
	}, recipe
}

func TestEndorseBurnsLabelsAndFixedStripWithoutCoveringSource(t *testing.T) {
	r, layout, endorsements, recipe := endorsementFixture(t)
	out, err := Endorse(r, layout, endorsements, recipe)
	require.NoError(t, err)
	require.Equal(t, image.Rect(0, 0, 300, 450), out.Pixels.Bounds())
	require.Equal(t, layout.Output, out.Page)
	whiteLabel, blackNumber := false, false
	for y := range 450 {
		for x := range 300 {
			p := out.Pixels.NRGBAAt(x, y)
			require.Equal(t, uint8(255), p.A)
			if y >= 30 && y < 120 && x >= 30 && x < 270 && p.R > 0 {
				whiteLabel = true
			}
			if y >= 300 && p.R < 255 {
				blackNumber = true
			}
			if y >= 120 && y < 300 {
				require.Equal(t, r.Pixels.NRGBAAt(x, y), p)
			}
		}
	}
	require.True(t, whiteLabel)
	require.True(t, blackNumber)
	require.Equal(t, color.NRGBA{A: 255}, r.Pixels.NRGBAAt(40, 50))
}

func TestEndorseRejectsOverflowCollisionsUnmaskedLabelsAndInvalidStrip(t *testing.T) {
	for name, mutate := range map[string]func(*Raster, *redaction.PageLayout, *[]redaction.Endorsement){
		"too small": func(_ *Raster, _ *redaction.PageLayout, e *[]redaction.Endorsement) { (*e)[0].Box.X1 = 1100 },
		"under minimum": func(_ *Raster, _ *redaction.PageLayout, e *[]redaction.Endorsement) {
			(*e)[0].FontSizeMilliPoints = 7999
		},
		"source stamp": func(_ *Raster, _ *redaction.PageLayout, e *[]redaction.Endorsement) {
			(*e)[1].Box.Y0 = 5000
			(*e)[1].Box.Y1 = 9000
		},
		"collision": func(_ *Raster, _ *redaction.PageLayout, e *[]redaction.Endorsement) { *e = append(*e, (*e)[1]) },
		"unmasked label": func(r *Raster, _ *redaction.PageLayout, _ *[]redaction.Endorsement) {
			r.Pixels.SetNRGBA(35, 35, color.NRGBA{R: 255, A: 255})
		},
		"mutated burned pixel": func(r *Raster, _ *redaction.PageLayout, _ *[]redaction.Endorsement) {
			r.Pixels.SetNRGBA(0, 0, color.NRGBA{A: 255})
		},
		"wrong strip": func(_ *Raster, l *redaction.PageLayout, _ *[]redaction.Endorsement) {
			l.StripHeight = 4000
			l.Output.Height = 14000
		},
		"shifted source": func(_ *Raster, l *redaction.PageLayout, _ *[]redaction.Endorsement) {
			l.Source.FrameSHA256 = digest([]byte("other"))
		},
		"missing glyph": func(_ *Raster, _ *redaction.PageLayout, e *[]redaction.Endorsement) { (*e)[0].Text = "\U0010ffff" },
	} {
		t.Run(name, func(t *testing.T) {
			r, l, e, recipe := endorsementFixture(t)
			mutate(&r, &l, &e)
			_, err := Endorse(r, l, e, recipe)
			require.ErrorContains(t, err, "endorsement_layout_conflict")
		})
	}
}
