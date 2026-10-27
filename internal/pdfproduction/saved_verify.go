package pdfproduction

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"io"
	"time"

	"github.com/klippa-app/go-pdfium"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
)

// VerifySaved checks native-redaction PDF masks, text and structure against a
// resolved review. It cannot compare unmasked pixels with the unavailable source.
// The caller owns a stable reader for the entire call; no PDF copy is created.
func VerifySaved(ctx context.Context, reader io.ReaderAt, size int64,
	plan redaction.Resolved, recipe redaction.Recipe, presentation []PagePresentation,
) (result error) {
	if err := validateSavedPlan(ctx, plan, recipe); err != nil {
		return err
	}
	if presentation != nil && len(presentation) != plan.PageCount {
		return pdfIntegrity("presentation page count differs")
	}
	if reader == nil || size <= 0 || size > 512<<20 {
		return pdfIntegrity("saved PDF size exceeds bounds")
	}
	ctx, stop, err := watchRSS(ctx, recipe.QualifiedPeakRSSBytes, readProcessRSS)
	if err != nil {
		return err
	}
	defer stop()
	var engine Engine
	defer func() {
		// Capture verification's cause before teardown can trigger another refusal.
		if cause := context.Cause(ctx); cause != nil {
			result = errors.Join(result, cause)
		}
		if engine != nil {
			result = errors.Join(result, engine.Close())
		}
	}()
	setup, cancel := context.WithTimeout(ctx, time.Duration(recipe.PageTimeoutSeconds)*time.Second)
	index, err := parseFreshIndex(setup, reader, size, plan.PageCount)
	cancel()
	if err != nil {
		return err
	}
	engine, err = NewPDFium(recipe)
	if err != nil {
		return err
	}
	e, ok := engine.(*pdfiumEngine)
	if !ok {
		return errors.New("unexpected PDFium engine implementation")
	}
	for i := range plan.Pages {
		if err := verifySavedPage(ctx, e, index, i, plan, presentation); err != nil {
			return fmt.Errorf("saved PDF page %d: %w", i+1, err)
		}
	}
	return ctx.Err()
}

func validateSavedPlan(ctx context.Context, plan redaction.Resolved, recipe redaction.Recipe) error {
	if err := validateRecipe(recipe); err != nil {
		return err
	}
	if recipe.DPI != 300 || plan.PageCount < 1 || plan.PageCount > 16 {
		return errors.New("unsupported saved redaction recipe or page count")
	}
	if err := preflightResolved(ctx, plan); err != nil {
		return err
	}
	if _, err := redaction.Text(plan); err != nil {
		return err
	}
	data, err := canonical.Marshal(recipe)
	if err != nil {
		return err
	}
	if hashBytes(data) != plan.RecipeSHA256 {
		return errors.New("saved review recipe digest mismatch")
	}
	return nil
}

func savedPageDescriptor(plan redaction.Resolved, i int, presentation []PagePresentation) (PageArtifact, error) {
	page := plan.Pages[i]
	p, err := presentationAt(page, i, presentation)
	if err != nil {
		return PageArtifact{}, err
	}
	layout := p.Layout
	data, err := canonical.Marshal(layout)
	if err != nil {
		return PageArtifact{}, err
	}
	endorsements, err := canonical.Marshal(p.Endorsements)
	if err != nil {
		return PageArtifact{}, err
	}
	a := PageArtifact{Page: layout.Output, Layout: layout, LayoutSHA256: hashBytes(data), ResolvedSHA256: plan.SHA256,
		Endorsements: p.Endorsements, EndorsementsSHA256: hashBytes(endorsements)}
	for _, run := range plan.Runs {
		if run.Page == page.Number {
			a.Runs = append(a.Runs, run)
		}
	}
	return a, nil
}

func verifySavedPage(ctx context.Context, e *pdfiumEngine, index *freshIndex, i int,
	plan redaction.Resolved, presentation []PagePresentation,
) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(e.recipe.PageTimeoutSeconds)*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	a, err := savedPageDescriptor(plan, i, presentation)
	if err != nil {
		return err
	}
	if err := validatePageDescriptor(a, e.recipe); err != nil {
		return err
	}
	var boxes []redaction.Box
	for _, box := range plan.RedactBoxes {
		if box.Page == a.Page.Number {
			boxes = append(boxes, box)
		}
	}
	masks, err := PlanPixels(a.Layout.Source, boxes, e.recipe)
	if err != nil {
		return err
	}
	parsed, err := index.page(ctx, i, a, e.recipe)
	if err != nil {
		return err
	}
	w, h, err := dimensions(a.Page, e.recipe)
	if err != nil {
		return err
	}
	// The index describes the complete caller-owned snapshot, not a mutable path.
	return e.withInstanceContext(ctx, func(ctx context.Context, instance pdfium.Pdfium) (pageErr error) {
		size := index.ends[len(index.ends)-1]
		ref, err := openVerificationPage(instance, index.reader, size, verificationPage{i, plan.PageCount, a, parsed.advances})
		if err != nil {
			return err
		}
		rows, err := openSavedRGBRows(ctx, parsed.image, w, h, masks)
		if err != nil {
			return err
		}
		defer func() { pageErr = errors.Join(pageErr, rows.Close()) }()
		next, err := savedFooterRows(rows, a, e.recipe, w, h)
		if err != nil {
			return err
		}
		pixels := verificationPixels{width: w, height: h, next: next}
		for top := 0; top < h; top += 256 {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := compareTile(instance, ref, pixels, top, min(256, h-top)); err != nil {
				return err
			}
		}
		return rows.Finish()
	})
}

// Compare visible glyphs with independently painted labels as well as checking
// the PDF text program. Each grayscale sample may differ by at most 16/255
// inside a number box; there is no positional tolerance or averaging over pixels.
// The background outside the boxes remains exactly white.
func savedFooterRows(rows *savedRGBRows, a PageArtifact, recipe redaction.Recipe, w, h int) (func() ([]byte, error), error) {
	if a.Layout.StripHeight == 0 {
		return rows.Next, nil
	}
	_, sourceHeight, err := dimensions(a.Layout.Source, recipe)
	if err != nil {
		return nil, err
	}
	footer := image.NewNRGBA(image.Rect(0, sourceHeight, w, h))
	draw.Draw(footer, footer.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	if err := paintEndorsements(footer, a.Layout, a.Endorsements, recipe); err != nil {
		return nil, err
	}
	rects := make([]image.Rectangle, len(a.Endorsements))
	for i, e := range a.Endorsements {
		rects[i] = endorsementRectangle(e.Box, recipe.DPI)
	}
	return func() ([]byte, error) {
		y := rows.row
		row, err := rows.Next()
		if err != nil {
			return nil, err
		}
		if y >= sourceHeight {
			expected := footer.Pix[footer.PixOffset(0, y):]
			for x := range w {
				r, g, b := row[x*4], row[x*4+1], row[x*4+2]
				owner := -1
				for i, rect := range rects {
					if image.Pt(x, y).In(rect) {
						owner = i
						break
					}
				}
				if owner < 0 {
					if r != 255 || g != 255 || b != 255 {
						return nil, pdfIntegrity("number footer background differs")
					}
				} else {
					if r != g || g != b {
						return nil, pdfIntegrity("number footer ink is not neutral")
					}
					difference := int(r) - int(expected[x*4])
					if difference < -16 || difference > 16 {
						return nil, pdfIntegrity("number footer glyphs or placement differ")
					}
				}
			}
		}
		return row, nil
	}, nil
}
