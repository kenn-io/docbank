package pdfproduction

import (
	"crypto/sha256"
	"errors"
	"image"
	"image/color"
	"image/draw"

	"go.kenn.io/docbank/document/redaction"
)

type burnProof struct {
	page   redaction.Page
	dpi    int
	pixels [32]byte
}

// PlanPixels converts already padded, source-bound masks to outward-rounded
// pixel rectangles. It never adds padding or clips invalid geometry.
func PlanPixels(page redaction.Page, boxes []redaction.Box, recipe redaction.Recipe) ([]image.Rectangle, error) {
	if err := validateRecipe(recipe); err != nil {
		return nil, err
	}
	if _, _, err := dimensions(page, recipe); err != nil {
		return nil, err
	}
	if len(boxes) > 16384 {
		return nil, errors.New("page mask count exceeds qualified bounds")
	}
	out := make([]image.Rectangle, 0, len(boxes))
	for _, box := range boxes {
		if !validBox(box, page) {
			return nil, errors.New("mask is outside its bound source frame")
		}
		out = append(out, pixelRectangle(box, recipe.DPI))
	}
	return out, nil
}

// Called only after bounded physical dimensions and box containment checks.
func pixelRectangle(box redaction.Box, dpi int) image.Rectangle {
	d := int64(dpi)
	return image.Rect(int(box.X0*d/10000), int(box.Y0*d/10000), int((box.X1*d+9999)/10000), int((box.Y1*d+9999)/10000))
}

func validateRaster(r Raster, recipe redaction.Recipe) error {
	if err := validateRecipe(recipe); err != nil {
		return err
	}
	w, h, err := dimensions(r.Page, recipe)
	if err != nil {
		return err
	}
	if r.DPI != recipe.DPI || r.Pixels == nil || r.Pixels.Rect != image.Rect(0, 0, w, h) || r.Pixels.Stride < w*4 || r.Pixels.Stride > len(r.Pixels.Pix)/h || len(r.Pixels.Pix) < r.Pixels.Stride*(h-1)+w*4 {
		return errors.New("raster pixels do not match bound page dimensions")
	}
	return nil
}

// Burn owns its result, flattens transparency onto white, and destroys the
// selected pixels with an opaque overwrite. Input pixels are never changed.
func Burn(r Raster, boxes []redaction.Box, recipe redaction.Recipe) (Raster, error) {
	if err := validateRaster(r, recipe); err != nil {
		return Raster{}, err
	}
	rectangles, err := PlanPixels(r.Page, boxes, recipe)
	if err != nil {
		return Raster{}, err
	}
	dst := image.NewNRGBA(r.Pixels.Bounds())
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), r.Pixels, image.Point{}, draw.Over)
	for _, rectangle := range rectangles {
		draw.Draw(dst, rectangle, image.NewUniform(color.Black), image.Point{}, draw.Src)
	}
	return Raster{Pixels: dst, Page: r.Page, DPI: r.DPI, burned: &burnProof{page: r.Page, dpi: r.DPI, pixels: sha256.Sum256(dst.Pix)}}, nil
}
