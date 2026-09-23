package pdfproduction

import (
	"context"
	"encoding/json/v2"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"io"
	"reflect"
	"slices"
	"strings"
	"unicode"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
)

// WriteFresh accepts one canonical document authority before consuming any
// artifacts. Page callbacks cannot substitute runs, frames, masks or recipes.
func WriteFresh(ctx context.Context, out io.Writer, pages PageSequence, plan redaction.Resolved, recipe redaction.Recipe) error {
	if err := validateRecipe(recipe); err != nil {
		return err
	}
	if pages == nil {
		return errors.New("missing page sequence")
	}
	if err := preflightResolved(ctx, plan); err != nil {
		return err
	}
	if _, err := redaction.Text(plan); err != nil {
		return err
	}
	rb, err := canonical.Marshal(recipe)
	if err != nil {
		return err
	}
	if hashBytes(rb) != plan.RecipeSHA256 {
		return errors.New("resolved plan recipe digest mismatch")
	}
	// Freeze authority before calling any caller-owned page or PNG callback.
	encoded, sha, err := redaction.CanonicalResolved(plan)
	if err != nil {
		return err
	}
	var frozen redaction.Resolved
	if err := json.Unmarshal(encoded, &frozen); err != nil {
		return err
	}
	frozen.SHA256 = sha
	return writeFresh(ctx, out, &resolvedSequence{pages: pages, plan: frozen, recipe: recipe}, recipe)
}

func preflightResolved(ctx context.Context, p redaction.Resolved) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.PageCount < 1 || p.PageCount > maxPageCount || len(p.Runs) > 1_000_000 {
		return &redaction.Problem{Code: "render_limit"}
	}
	textBytes := make([]int, p.PageCount)
	boxCounts := make([]int, p.PageCount)
	maskCounts := make([]int, p.PageCount)
	for _, run := range p.Runs {
		if run.Page < 1 || run.Page > p.PageCount {
			return errors.New("invalid resolved run page")
		}
		i := run.Page - 1
		if len(run.Text) > maxTextBytes-textBytes[i] || len(run.Boxes) > 16384-boxCounts[i] {
			return &redaction.Problem{Code: "render_limit"}
		}
		textBytes[i] += len(run.Text)
		boxCounts[i] += len(run.Boxes)
	}
	for _, mask := range p.RedactBoxes {
		if mask.Page < 1 || mask.Page > p.PageCount {
			return errors.New("invalid resolved mask page")
		}
		maskCounts[mask.Page-1]++
	}
	for i, count := range maskCounts {
		if count > 16384 || int64(count)*int64(boxCounts[i]) > 8_000_000 {
			return &redaction.Problem{Code: "render_limit"}
		}
	}
	if _, exceeded, err := canonical.BoundedSize(p, 64<<20); err != nil {
		return err
	} else if exceeded {
		return &redaction.Problem{Code: "render_limit"}
	}
	return nil
}

// WriteRendition constructs bounded source renditions before a redaction plan
// exists. Its output is source evidence, never a sanitized production receipt.
func WriteRendition(ctx context.Context, out io.Writer, pages PageSequence, recipe redaction.Recipe) error {
	return writeFresh(ctx, out, pages, recipe)
}

type resolvedSequence struct {
	pages           PageSequence
	plan            redaction.Resolved
	recipe          redaction.Recipe
	page, run, mask int
}

func (s *resolvedSequence) Next(ctx context.Context) (PageArtifact, error) {
	a, err := s.pages.Next(ctx)
	if errors.Is(err, io.EOF) {
		if s.page != s.plan.PageCount {
			return PageArtifact{}, errors.New("resolved page inventory truncated")
		}
		return PageArtifact{}, io.EOF
	}
	if err != nil {
		return PageArtifact{}, err
	}
	if s.page >= s.plan.PageCount || a.Layout.Source != s.plan.Pages[s.page] || a.ResolvedSHA256 != s.plan.SHA256 {
		return PageArtifact{}, errors.New("artifact differs from resolved page authority")
	}
	s.page++
	firstRun := s.run
	for s.run < len(s.plan.Runs) && s.plan.Runs[s.run].Page == s.page {
		s.run++
	}
	runs := s.plan.Runs[firstRun:s.run]
	if a.Runs != nil && !reflect.DeepEqual(a.Runs, runs) {
		return PageArtifact{}, errors.New("artifact runs differ from resolved plan")
	}
	a.Runs = runs
	firstMask := s.mask
	for s.mask < len(s.plan.RedactBoxes) && s.plan.RedactBoxes[s.mask].Page == s.page {
		s.mask++
	}
	a.masks = s.plan.RedactBoxes[firstMask:s.mask]
	a.bound = true
	rectangles, err := PlanPixels(a.Layout.Source, a.masks, s.recipe)
	if err != nil {
		return PageArtifact{}, err
	}
	for _, run := range runs {
		for _, box := range run.Boxes {
			if !validBox(box, a.Layout.Source) {
				return PageArtifact{}, errors.New("invalid resolved run box")
			}
			if run.Kind == "text" {
				for _, mask := range rectangles {
					if pixelRectangle(box, s.recipe.DPI).Overlaps(mask) {
						return PageArtifact{}, errors.New("retained run intersects resolved mask")
					}
				}
			}
		}
	}
	a.Endorsements = slices.Clone(a.Endorsements)
	if err := paintEndorsements(nil, a.Layout, a.Endorsements, s.recipe); err != nil {
		return PageArtifact{}, err
	}
	for _, e := range a.Endorsements {
		if e.Kind == "label" {
			contained := false
			for _, mask := range a.masks {
				if e.Box.X0 >= mask.X0 && e.Box.Y0 >= mask.Y0 && e.Box.X1 <= mask.X1 && e.Box.Y1 <= mask.Y1 {
					contained = true
					break
				}
			}
			if !contained {
				return PageArtifact{}, endorsementConflict()
			}
		}
	}
	return a, nil
}

// Receipt hashes are private authority. Final pixels must actually implement
// its masks and public endorsements even if a caller mutates a burned raster.
func validateBoundPixels(img image.Image, a PageArtifact, r redaction.Recipe) error {
	if !a.bound {
		return nil
	}
	w, h, err := dimensions(a.Page, r)
	if err != nil {
		return err
	}
	expected := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(expected, expected.Bounds(), image.NewUniform(color.Black), image.Point{}, draw.Src)
	_, sourceH, err := dimensions(a.Layout.Source, r)
	if err != nil {
		return err
	}
	strip := image.Rect(0, sourceH, w, h)
	draw.Draw(expected, strip, image.NewUniform(color.White), image.Point{}, draw.Src)
	if err := paintEndorsements(expected, a.Layout, a.Endorsements, r); err != nil {
		return err
	}
	masks, err := PlanPixels(a.Layout.Source, a.masks, r)
	if err != nil {
		return err
	}
	masks = append(masks, strip)
	for _, rect := range masks {
		for y := rect.Min.Y; y < rect.Max.Y; y++ {
			for x := rect.Min.X; x < rect.Max.X; x++ {
				if color.NRGBAModel.Convert(img.At(x, y)) != expected.NRGBAAt(x, y) {
					return errors.New("final page pixels differ from resolved masks or endorsements")
				}
			}
		}
	}
	return nil
}

// A whitespace-only run has no source ink. Give it deterministic invisible
// placement without changing its canonical boxes, Unicode, or source anchor.
func runPlacement(run redaction.Run, page redaction.Page) redaction.Box {
	if len(run.Boxes) > 0 {
		return run.Boxes[0]
	}
	return redaction.Box{Page: page.Number, FrameSHA256: page.FrameSHA256, X1: min(page.Width, 1000), Y1: min(page.Height, 1000)}
}
func whitespaceOnly(text string) bool {
	return text != "" && strings.IndexFunc(text, func(r rune) bool { return !unicode.IsSpace(r) }) < 0
}
func glyphRune(r rune) rune {
	if unicode.IsSpace(r) {
		return ' '
	}
	return r
}
