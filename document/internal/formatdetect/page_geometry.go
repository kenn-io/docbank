package formatdetect

import (
	"errors"
	"fmt"
	"math"

	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

// PDFPageGeometry is effective page-tree geometry in PDF points. Call only
// from the resource-limited page inspection executable.
type PDFPageGeometry struct {
	MediaBox, CropBox [4]float64
	Rotation          int
}

func ReadPDFPageGeometry(data []byte, maxPages int) ([]PDFPageGeometry, error) {
	if len(data) == 0 || len(data) > 64<<20 || maxPages < 1 || maxPages > 1000 {
		return nil, errors.New("PDF geometry exceeds source/page limits")
	}
	ctx, err := validatePDFObjectGraph(data, PDFLimits{MaxExpandedBytes: 256 << 20, MaxEntryBytes: 64 << 20, MaxEntries: 100000})
	if err != nil {
		return nil, err
	}
	if ctx.PageCount < 1 || ctx.PageCount > maxPages {
		return nil, errors.New("PDF page count exceeds limit")
	}
	frames := make([]PDFPageGeometry, ctx.PageCount)
	for page := 1; page <= ctx.PageCount; page++ {
		dict, _, attrs, err := ctx.PageDict(page, false)
		if err != nil {
			return nil, fmt.Errorf("reading PDF page geometry: %w", err)
		}
		if attrs.MediaBox == nil {
			return nil, errors.New("PDF MediaBox unavailable")
		}
		ancestor := dict
		for depth := 0; ancestor != nil; depth++ {
			if depth > maxPDFPageTreeDepth {
				return nil, errors.New("PDF page inheritance exceeds limit")
			}
			for _, name := range []string{"Rotate", "UserUnit"} {
				if obj, ok := ancestor.Find(name); ok {
					v, err := ctx.DereferenceNumber(obj)
					if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || name == "UserUnit" && v != 1 || name == "Rotate" && (math.Mod(v, 90) != 0 || math.Abs(v) > 360000) {
						return nil, errors.New("unsupported PDF rotation or UserUnit")
					}
				}
			}
			parent, ok := ancestor.Find("Parent")
			if !ok {
				break
			}
			ancestor, err = ctx.DereferenceDict(parent)
			if err != nil {
				return nil, fmt.Errorf("reading PDF page ancestor: %w", err)
			}
		}
		crop := attrs.CropBox
		if crop == nil {
			crop = attrs.MediaBox
		}
		coordinates := func(r *types.Rectangle) [4]float64 { return [4]float64{r.LL.X, r.LL.Y, r.UR.X, r.UR.Y} }
		frames[page-1] = PDFPageGeometry{MediaBox: coordinates(attrs.MediaBox), CropBox: coordinates(crop), Rotation: ((attrs.Rotate % 360) + 360) % 360}
	}
	return frames, nil
}
