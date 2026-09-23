package pdfproduction

import (
	"crypto/sha256"
	"image"
	"image/color"
	"image/draw"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document/redaction"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

func endorsementConflict() error { return &redaction.Problem{Code: "endorsement_layout_conflict"} }

func validateLayout(l redaction.PageLayout, recipe redaction.Recipe) error {
	if _, _, err := dimensions(l.Source, recipe); err != nil {
		return endorsementConflict()
	}
	if _, _, err := dimensions(l.Output, recipe); err != nil {
		return endorsementConflict()
	}
	if l.StripHeight != 0 && l.StripHeight != 5000 || l.Output.Number != l.Source.Number || l.Output.Width != l.Source.Width || l.Output.Height != l.Source.Height+l.StripHeight || l.Output.Span != l.Source.Span || l.StripHeight == 0 && l.Source != l.Output {
		return endorsementConflict()
	}
	return nil
}

// paintEndorsements validates even when dst is nil. All glyphs and advances
// must fit; no font substitution, clipping, host fonts, or blank-margin guess.
func paintEndorsements(dst *image.NRGBA, l redaction.PageLayout, es []redaction.Endorsement, recipe redaction.Recipe) error {
	if err := validateLayout(l, recipe); err != nil {
		return err
	}
	if err := preflightEndorsements(es); err != nil {
		return endorsementConflict()
	}
	parsed, err := opentype.Parse(fontBytes)
	if err != nil {
		return endorsementConflict()
	}
	occupied := make([]image.Rectangle, 0, len(es))
	for _, e := range es {
		if !validBox(e.Box, l.Output) || !utf8.ValidString(e.Text) || e.Text == "" || strings.ContainsAny(e.Text, "\r\n\t\f") || e.FontSHA256 != fontSHA256 || e.FontSizeMilliPoints < 8000 || e.FontSizeMilliPoints > 1_000_000 {
			return endorsementConflict()
		}
		switch e.Kind {
		case "label":
			if e.Color != "#ffffff" || e.Box.Y1 > l.Source.Height {
				return endorsementConflict()
			}
		case "legend", "number":
			if e.Color != "#000000" || l.StripHeight != 5000 || e.Box.Y0 < l.Source.Height {
				return endorsementConflict()
			}
		default:
			return endorsementConflict()
		}
		rect := endorsementRectangle(e.Box, recipe.DPI)
		if rect.Empty() {
			return endorsementConflict()
		}
		if slices.ContainsFunc(occupied, rect.Overlaps) {
			return endorsementConflict()
		}
		occupied = append(occupied, rect)
		face, err := opentype.NewFace(parsed, &opentype.FaceOptions{Size: float64(e.FontSizeMilliPoints) / 1000, DPI: float64(recipe.DPI), Hinting: font.HintingNone})
		if err != nil {
			return endorsementConflict()
		}
		err = paintEndorsement(dst, face, e, rect)
		closeErr := face.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

// Endorsement ink stays inward of its physical box, particularly when the
// strip begins between pixels. Mask overwrites use the opposite rounding.
func endorsementRectangle(b redaction.Box, dpi int) image.Rectangle {
	d := int64(dpi)
	return image.Rect(int((b.X0*d+9999)/10000), int((b.Y0*d+9999)/10000), int(b.X1*d/10000), int(b.Y1*d/10000))
}

func paintEndorsement(dst *image.NRGBA, face font.Face, e redaction.Endorsement, rect image.Rectangle) error {
	var measured int64
	var previous rune
	first := true
	for _, ch := range e.Text {
		_, advance, ok := face.GlyphBounds(ch)
		if !ok {
			return endorsementConflict()
		}
		if !first {
			measured += int64(face.Kern(previous, ch))
		}
		measured += int64(advance)
		// x/image accumulates string advances in signed 26.6 fixed point.
		// Reject overlong text with checked wide arithmetic before invoking it.
		if measured < 0 || measured > int64(fixed.I(rect.Dx())) {
			return endorsementConflict()
		}
		previous = ch
		first = false
	}
	bounds, advance := font.BoundString(face, e.Text)
	minX := min(bounds.Min.X, fixed.Int26_6(0))
	maxX := max(bounds.Max.X, advance)
	if (maxX-minX).Ceil() > rect.Dx() || (bounds.Max.Y-bounds.Min.Y).Ceil() > rect.Dy() {
		return endorsementConflict()
	}
	if dst == nil {
		return nil
	}
	ink := color.Black
	if e.Kind == "label" {
		ink = color.White
	}
	dot := fixed.Point26_6{X: fixed.I(rect.Min.X) - minX, Y: fixed.I(rect.Min.Y) - bounds.Min.Y}
	d := font.Drawer{Dst: dst, Src: image.NewUniform(ink), Face: face, Dot: dot}
	d.DrawString(e.Text)
	return nil
}

// Endorse appends the immutable output strip at the unchanged source origin
// and burns only public text. Labels require an entirely opaque black box;
// the document writer additionally binds it to the resolved mask authority.
func Endorse(r Raster, l redaction.PageLayout, es []redaction.Endorsement, recipe redaction.Recipe) (Raster, error) {
	if err := validateRaster(r, recipe); err != nil {
		return Raster{}, err
	}
	if r.burned == nil || r.burned.page != r.Page || r.burned.dpi != r.DPI || r.burned.pixels != sha256.Sum256(r.Pixels.Pix) {
		return Raster{}, endorsementConflict()
	}
	if r.Page != l.Source {
		return Raster{}, endorsementConflict()
	}
	if err := paintEndorsements(nil, l, es, recipe); err != nil {
		return Raster{}, err
	}
	for y := range r.Pixels.Rect.Dy() {
		for x := range r.Pixels.Rect.Dx() {
			if r.Pixels.NRGBAAt(x, y).A != 255 {
				return Raster{}, endorsementConflict()
			}
		}
	}
	for _, e := range es {
		if e.Kind == "label" {
			rect := pixelRectangle(e.Box, recipe.DPI)
			for y := rect.Min.Y; y < rect.Max.Y; y++ {
				for x := rect.Min.X; x < rect.Max.X; x++ {
					if r.Pixels.NRGBAAt(x, y) != (color.NRGBA{A: 255}) {
						return Raster{}, endorsementConflict()
					}
				}
			}
		}
	}
	w, h, err := dimensions(l.Output, recipe)
	if err != nil {
		return Raster{}, err
	}
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(dst, r.Pixels.Bounds(), r.Pixels, image.Point{}, draw.Src)
	if err := paintEndorsements(dst, l, es, recipe); err != nil {
		return Raster{}, err
	}
	return Raster{Pixels: dst, Page: l.Output, DPI: r.DPI}, nil
}
