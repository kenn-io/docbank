package production

import (
	"unicode"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/pdfproduction"
)

func alignNative(frames []document.PageFrameV1, inspected []pdfproduction.NativeTextPage, pages []redaction.Page, evidenceSHA, pdfSHA string) (redaction.TextMap, error) {
	if len(inspected) != len(frames) || len(pages) != len(frames) {
		return redaction.TextMap{}, mappingError("native page inspection is incomplete")
	}
	result := mapIdentity("aligned-text/v1", pdfSHA, evidenceSHA, "", pages)
	for index, observed := range inspected {
		if observed.Number != index+1 {
			return redaction.TextMap{}, mappingError("native page order differs")
		}
		start := int64(len(result.Text))
		for _, bounds := range observed.NonTextBounds {
			box, ok, err := pdfproduction.PhysicalBox(frames[index], bounds, pages[index]) //nolint:gosec // all slices have equal length above.
			if err != nil {
				return redaction.TextMap{}, err
			}
			if ok {
				result.Gaps = append(result.Gaps, redaction.Gap{Box: box, Anchor: start, Unordered: true})
			}
		}
		for _, glyph := range observed.Glyphs {
			box, ok, err := pdfproduction.PhysicalBox(frames[index], glyph.Bounds, pages[index]) //nolint:gosec // all three slices have equal length above
			if err != nil {
				return redaction.TextMap{}, err
			}
			if glyph.GapReason != "" {
				if ok {
					result.Gaps = append(result.Gaps, redaction.Gap{Box: box, Anchor: int64(len(result.Text))})
				}
				if glyph.Text != "" && allInvisibleWhitespace(glyph.Text) {
					result.Text += glyph.Text
				}
				continue
			}
			if glyph.Text == "" || !ok {
				continue
			}
			if allInvisibleWhitespace(glyph.Text) {
				result.Text += glyph.Text
				continue
			}
			span := redaction.Span{Start: int64(len(result.Text)), End: int64(len(result.Text) + len(glyph.Text))}
			result.Text += glyph.Text
			result.Atoms = append(result.Atoms, redaction.Atom{Span: span, Boxes: []redaction.Box{box}})
		}
		result.Pages[index].Span = redaction.Span{Start: start, End: int64(len(result.Text))}
	}
	if len(result.Atoms) == 0 && len(result.Gaps) == 0 {
		return redaction.TextMap{}, mappingError("PDF has no accepted visible native text")
	}
	return result, nil
}

func allInvisibleWhitespace(s string) bool {
	for _, r := range s {
		if !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}
