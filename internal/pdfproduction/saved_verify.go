package pdfproduction

import (
	"context"
	"errors"
	"fmt"
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
	plan redaction.Resolved, recipe redaction.Recipe,
) (result error) {
	if err := validateSavedPlan(ctx, plan, recipe); err != nil {
		return err
	}
	if reader == nil || size <= 0 || size > 512<<20 {
		return pdfIntegrity("saved PDF size exceeds bounds")
	}
	ctx, stop, err := watchRSS(ctx, recipe.QualifiedPeakRSSBytes, readProcessRSS)
	if err != nil {
		return err
	}
	defer stop()
	defer func() {
		if cause := context.Cause(ctx); cause != nil {
			result = errors.Join(result, cause)
		}
	}()
	setup, cancel := context.WithTimeout(ctx, time.Duration(recipe.PageTimeoutSeconds)*time.Second)
	index, err := parseFreshIndex(setup, reader, size, plan.PageCount)
	cancel()
	if err != nil {
		return err
	}
	engine, err := NewPDFium(recipe)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, engine.Close()) }()
	e, ok := engine.(*pdfiumEngine)
	if !ok {
		return errors.New("unexpected PDFium engine implementation")
	}
	for i := range plan.Pages {
		if err := verifySavedPage(ctx, e, index, i, plan); err != nil {
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

func savedPageDescriptor(plan redaction.Resolved, i int) (PageArtifact, error) {
	page := plan.Pages[i]
	layout := redaction.PageLayout{Source: page, Output: page}
	data, err := canonical.Marshal(layout)
	if err != nil {
		return PageArtifact{}, err
	}
	a := PageArtifact{Page: page, Layout: layout, LayoutSHA256: hashBytes(data), ResolvedSHA256: plan.SHA256,
		Endorsements: []redaction.Endorsement{}, EndorsementsSHA256: hashBytes([]byte("[]"))}
	for _, run := range plan.Runs {
		if run.Page == page.Number {
			a.Runs = append(a.Runs, run)
		}
	}
	return a, nil
}

func verifySavedPage(ctx context.Context, e *pdfiumEngine, index *freshIndex, i int,
	plan redaction.Resolved,
) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(e.recipe.PageTimeoutSeconds)*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	a, err := savedPageDescriptor(plan, i)
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
	masks, err := PlanPixels(a.Page, boxes, e.recipe)
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
		pixels := verificationPixels{width: w, height: h, next: rows.Next}
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
