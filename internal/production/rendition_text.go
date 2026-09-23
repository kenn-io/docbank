package production

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfproduction"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

const (
	textDPI            = 300
	letterWidthPixels  = 2550
	letterHeightPixels = 3300
	marginPixels       = 225
	fontSizePoints     = 11
	lineHeightPixels   = 58 // 14pt at 300 DPI, rounded deterministically.
)

type textRenditionLimits struct {
	MaxInputBytes  int
	MaxPages       int
	MaxAtoms       int
	MaxMapBytes    int64
	MaxOutputBytes int64
}

var qualifiedTextRenditionLimits = textRenditionLimits{
	MaxInputBytes:  16 << 20,
	MaxPages:       1_000,
	MaxAtoms:       2_000_000,
	MaxMapBytes:    128 << 20,
	MaxOutputBytes: 256 << 20,
}

type textGlyph struct {
	span                 redaction.Span
	page, x0, y0, x1, y1 int
}
type textLine struct {
	span                 redaction.Span
	page, x0, y0, x1, y1 int
	text                 string
}
type renderedTextPage struct {
	lines                []textLine
	span                 redaction.Span
	glyphStart, glyphEnd int
}

func RenderTextRendition(ctx context.Context, text string, units []redaction.Unit) ([]byte, redaction.TextMap, error) {
	return renderTextRendition(ctx, text, units, qualifiedTextRenditionLimits)
}

func renderTextRendition(ctx context.Context, text string, units []redaction.Unit, limits textRenditionLimits) ([]byte, redaction.TextMap, error) {
	if err := ctx.Err(); err != nil {
		return nil, redaction.TextMap{}, err
	}
	if limits.MaxInputBytes < 1 || limits.MaxPages < 1 || limits.MaxAtoms < 1 || limits.MaxMapBytes < 1 || limits.MaxOutputBytes < 1 || len(text) > limits.MaxInputBytes {
		return nil, redaction.TextMap{}, &Problem{Code: "rendition_limit_exceeded"}
	}
	if text == "" || !utf8.ValidString(text) {
		return nil, redaction.TextMap{}, errUnsupportedRendition
	}
	parsedFont, err := opentype.Parse(pdfproduction.BundledUnicodeFont())
	if err != nil {
		return nil, redaction.TextMap{}, fmt.Errorf("parse bundled rendition font: %w", err)
	}
	face, err := opentype.NewFace(parsedFont, &opentype.FaceOptions{Size: fontSizePoints, DPI: textDPI, Hinting: font.HintingNone})
	if err != nil {
		return nil, redaction.TextMap{}, fmt.Errorf("open bundled rendition font: %w", err)
	}
	defer face.Close() //nolint:errcheck // No resources beyond the in-memory font.
	pages, glyphs, err := layoutText(ctx, text, face, limits)
	if err != nil {
		return nil, redaction.TextMap{}, err
	}
	evidenceSHA, err := canonicalDigest(struct {
		Contract, Text string
		Units          []redaction.Unit
	}{"text-rendition/v1", text, units})
	if err != nil {
		return nil, redaction.TextMap{}, err
	}
	m := mapIdentity("aligned-text/v1", strings.Repeat("0", 64), evidenceSHA, text, make([]redaction.Page, len(pages)))
	for i := range pages {
		frameSHA, err := canonicalDigest(struct {
			Contract            string
			Page, Width, Height int
		}{"text-rendition-frame/v1", i + 1, 85000, 110000})
		if err != nil {
			return nil, redaction.TextMap{}, err
		}
		m.Pages[i] = redaction.Page{Number: i + 1, FrameSHA256: frameSHA, Width: 85000, Height: 110000, Span: pages[i].span}
	}
	for _, glyph := range glyphs {
		if allInvisibleWhitespace(text[glyph.span.Start:glyph.span.End]) {
			continue
		}
		m.Atoms = append(m.Atoms, redaction.Atom{Span: glyph.span, Boxes: []redaction.Box{pixelBox(m.Pages[glyph.page], glyph.x0, glyph.y0, glyph.x1, glyph.y1)}})
	}
	if len(units) == 0 {
		m.Units = paragraphUnits(m, evidenceSHA)
	} else {
		m.Units, err = bindProvidedUnits(m, units)
		if err != nil {
			return nil, redaction.TextMap{}, err
		}
	}
	if observed, exceeded, err := canonical.BoundedSize(m, limits.MaxMapBytes); err != nil {
		return nil, redaction.TextMap{}, err
	} else if exceeded || observed > limits.MaxMapBytes {
		return nil, redaction.TextMap{}, &Problem{Code: "rendition_limit_exceeded"}
	}
	var output bytes.Buffer
	sequence := &textPageSequence{text: text, face: face, pages: pages, glyphs: glyphs, mapPages: m.Pages}
	recipe := pdfproduction.QualifiedRecipe()
	recipe.MaxStagingBytes = min(recipe.MaxStagingBytes, limits.MaxOutputBytes)
	if err := pdfproduction.WriteRendition(ctx, &output, sequence, recipe); err != nil {
		if errors.Is(err, pdfproduction.ErrOutputLimit) {
			return nil, redaction.TextMap{}, &Problem{Code: "rendition_limit_exceeded"}
		}
		return nil, redaction.TextMap{}, err
	}
	data := output.Bytes()
	m.PDFSHA256 = hashData(data)
	canonicalizeMap(&m)
	canonicalMap, mapSHA256, err := redaction.CanonicalTextMap(m)
	if err != nil {
		return nil, redaction.TextMap{}, err
	}
	if int64(len(canonicalMap)) > limits.MaxMapBytes {
		return nil, redaction.TextMap{}, &Problem{Code: "rendition_limit_exceeded"}
	}
	m.SHA256 = mapSHA256
	if err := redaction.ValidateMap(m); err != nil {
		return nil, redaction.TextMap{}, err
	}
	return data, m, nil
}

func layoutText(ctx context.Context, text string, face font.Face, limits textRenditionLimits) ([]renderedTextPage, []textGlyph, error) {
	newPage := func(start int64, glyphStart int) renderedTextPage {
		return renderedTextPage{span: redaction.Span{Start: start, End: start}, glyphStart: glyphStart, glyphEnd: glyphStart}
	}
	pages := []renderedTextPage{newPage(0, 0)}
	var glyphs []textGlyph
	page, x, baseline := 0, marginPixels, marginPixels+46
	lineStart, lineX0 := 0, marginPixels
	lineGlyphStart := 0
	finishLine := func(end int) {
		if end > lineStart && len(glyphs) > lineGlyphStart {
			last := glyphs[len(glyphs)-1]
			pages[page].lines = append(pages[page].lines, textLine{span: redaction.Span{Start: int64(lineStart), End: int64(end)}, page: page, x0: lineX0, y0: baseline - 46, x1: last.x1, y1: baseline + 12, text: text[lineStart:end]})
		}
	}
	breakLine := func(lineEnd, nextStart int) bool {
		finishLine(lineEnd)
		pages[page].span.End = int64(nextStart)
		x, baseline, lineStart, lineGlyphStart = marginPixels, baseline+lineHeightPixels, nextStart, len(glyphs)
		if baseline+12 > letterHeightPixels-marginPixels {
			if len(pages) == limits.MaxPages {
				return false
			}
			pages = append(pages, newPage(int64(nextStart), len(glyphs)))
			page++
			baseline = marginPixels + 46
		}
		return true
	}
	for offset, r := range text {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		next := offset + utf8.RuneLen(r)
		if r == '\n' && offset > 0 && text[offset-1] == '\r' {
			continue // CRLF was consumed as one line break below.
		}
		if r == '\n' || r == '\r' {
			if r == '\r' && next < len(text) && text[next] == '\n' {
				next++
			}
			if !breakLine(offset, next) {
				return nil, nil, &Problem{Code: "rendition_limit_exceeded"}
			}
			continue
		}
		if unicode.IsSpace(r) {
			r = ' '
		}
		advance, ok := face.GlyphAdvance(r)
		if !ok {
			return nil, nil, fmt.Errorf("bundled font has no glyph for U+%04X: %w", r, errUnsupportedRendition)
		}
		pixels := max(1, advance.Ceil())
		if x+pixels > letterWidthPixels-marginPixels && x > marginPixels {
			if !breakLine(offset, offset) {
				return nil, nil, &Problem{Code: "rendition_limit_exceeded"}
			}
		}
		if len(glyphs) == limits.MaxAtoms {
			return nil, nil, &Problem{Code: "rendition_limit_exceeded"}
		}
		glyphs = append(glyphs, textGlyph{span: redaction.Span{Start: int64(offset), End: int64(next)}, page: page, x0: x, y0: baseline - 46, x1: x + pixels, y1: baseline + 12})
		pages[page].glyphEnd = len(glyphs)
		x += pixels
		pages[page].span.End = int64(next)
	}
	finishLine(len(text))
	pages[page].span.End = int64(len(text))
	return pages, glyphs, nil
}

func pixelBox(page redaction.Page, x0, y0, x1, y1 int) redaction.Box {
	return redaction.Box{Page: page.Number, FrameSHA256: page.FrameSHA256, X0: int64(x0) * 10000 / textDPI, Y0: int64(y0) * 10000 / textDPI, X1: (int64(x1)*10000 + textDPI - 1) / textDPI, Y1: (int64(y1)*10000 + textDPI - 1) / textDPI}
}

func textPageArtifact(ctx context.Context, page redaction.Page, rendered renderedTextPage, glyphs []textGlyph, text string, face font.Face) (pdfproduction.PageArtifact, error) {
	img := image.NewNRGBA(image.Rect(0, 0, letterWidthPixels, letterHeightPixels))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	for _, glyph := range glyphs {
		if err := ctx.Err(); err != nil {
			return pdfproduction.PageArtifact{}, err
		}
		drawer := font.Drawer{Dst: img, Src: image.NewUniform(color.Black), Face: face, Dot: fixed.P(glyph.x0, glyph.y1-12)}
		value := text[glyph.span.Start:glyph.span.End]
		if allInvisibleWhitespace(value) {
			value = " "
		}
		drawer.DrawString(value)
	}
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, img); err != nil {
		return pdfproduction.PageArtifact{}, fmt.Errorf("encode text rendition page: %w", err)
	}
	data := bytes.Clone(pngBytes.Bytes())
	runs := make([]redaction.Run, 0, len(rendered.lines))
	for _, line := range rendered.lines {
		if line.text == "" {
			continue
		}
		runs = append(runs, redaction.Run{Kind: "text", Text: line.text, Page: page.Number, SourceSpan: &redaction.Span{Start: line.span.Start, End: line.span.End}, Boxes: []redaction.Box{pixelBox(page, line.x0, line.y0, line.x1, line.y1)}, FontSizeMilliPoints: 11000})
	}
	layout := redaction.PageLayout{Source: page, Output: page}
	layoutBytes, err := canonical.Marshal(layout)
	if err != nil {
		return pdfproduction.PageArtifact{}, err
	}
	endorsements := []redaction.Endorsement{}
	endorsementBytes, err := canonical.Marshal(endorsements)
	if err != nil {
		return pdfproduction.PageArtifact{}, err
	}
	resolvedSHA, err := canonicalDigest(struct {
		Page int
		Span redaction.Span
		Runs []redaction.Run
	}{page.Number, page.Span, runs})
	if err != nil {
		return pdfproduction.PageArtifact{}, err
	}
	return pdfproduction.PageArtifact{Page: page, PNGSHA256: hashData(data), PNGSize: int64(len(data)), ResolvedSHA256: resolvedSHA, Layout: layout, LayoutSHA256: hashData(layoutBytes), Endorsements: endorsements, EndorsementsSHA256: hashData(endorsementBytes), Runs: runs, OpenPNG: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }}, nil
}

func paragraphUnits(m redaction.TextMap, evidenceSHA string) []redaction.Unit {
	var result []redaction.Unit
	start, ordinal := 0, 0
	for start < len(m.Text) {
		end := len(m.Text)
		if i := strings.Index(m.Text[start:], "\n\n"); i >= 0 {
			end = start + i
		}
		if end > start && strings.TrimSpace(m.Text[start:end]) != "" {
			id, _ := canonicalDigest(struct {
				EvidenceSHA256, Kind string
				Order                int
			}{evidenceSHA, "paragraph", ordinal})
			spans := pageBoundedSpans(m.Pages, []redaction.Span{{Start: int64(start), End: int64(end)}})
			result = append(result, redaction.Unit{ID: "paragraph:" + id[:24], Kind: "paragraph", Spans: spans, Boxes: boxesForSpans(m, spans)})
			ordinal++
		}
		start = end + 2
	}
	return result
}

type pageSequence struct {
	pages []pdfproduction.PageArtifact
	index int
}

// textPageSequence materializes and encodes exactly one page per Next call.
// WriteFresh consumes its OpenPNG callback before requesting the next page, so
// no earlier 33 MiB page raster or encoded PNG remains owned by the sequence.
type textPageSequence struct {
	text     string
	face     font.Face
	pages    []renderedTextPage
	glyphs   []textGlyph
	mapPages []redaction.Page
	index    int
}

func (s *textPageSequence) Next(ctx context.Context) (pdfproduction.PageArtifact, error) {
	if err := ctx.Err(); err != nil {
		return pdfproduction.PageArtifact{}, err
	}
	if s.index == len(s.pages) {
		return pdfproduction.PageArtifact{}, io.EOF
	}
	rendered := s.pages[s.index]
	artifact, err := textPageArtifact(ctx, s.mapPages[s.index], rendered, s.glyphs[rendered.glyphStart:rendered.glyphEnd], s.text, s.face)
	if err != nil {
		return pdfproduction.PageArtifact{}, err
	}
	s.index++
	return artifact, nil
}

func (s *pageSequence) Next(context.Context) (pdfproduction.PageArtifact, error) {
	if s.index == len(s.pages) {
		return pdfproduction.PageArtifact{}, io.EOF
	}
	p := s.pages[s.index]
	s.index++
	return p, nil
}

func hashData(value []byte) string { h := sha256.Sum256(value); return hex.EncodeToString(h[:]) }
