package csvpdf

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/go-pdf/fpdf"
	"go.kenn.io/docbank/document/ocr"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/sfnt"
)

const (
	columns      = 80
	linesPerPage = 48
)

type layoutLine struct {
	text         string
	record, cell int
}

func layout(ctx context.Context, records [][]string, limits Limits) ([]layoutLine, error) {
	font, err := sfnt.Parse(gomono.TTF)
	if err != nil {
		return nil, ocr.NewPreparationError("CSV PDF font could not be loaded", err)
	}
	var buffer sfnt.Buffer
	var lines []layoutLine
	appendLine := func(text string, record, cell int) error {
		if len(lines) == limits.MaxPages*linesPerPage {
			return ocr.NewPreparationError("CSV PDF exceeds page limit", nil)
		}
		lines = append(lines, layoutLine{text, record, cell})
		return nil
	}
	for recordIndex, record := range records {
		for cellIndex, cell := range record {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			for _, char := range cell {
				if char == '\n' {
					continue
				}
				allowedScript := unicode.Is(unicode.Latin, char) || unicode.Is(unicode.Greek, char) || unicode.Is(unicode.Cyrillic, char) || unicode.Is(unicode.Common, char)
				if char > 0xffff || unicode.IsControl(char) || unicode.Is(unicode.Cf, char) || unicode.IsMark(char) || !allowedScript {
					return nil, ocr.NewPreparationError("CSV cell contains unsupported text", nil)
				}
				glyph, err := font.GlyphIndex(&buffer, char)
				if err != nil || glyph == 0 {
					return nil, ocr.NewPreparationError("CSV cell contains a character absent from the embedded font", err)
				}
			}
			if err := appendLine(fmt.Sprintf("Record %d, cell %d", recordIndex+1, cellIndex+1), recordIndex+1, cellIndex+1); err != nil {
				return nil, err
			}
			for valueLine := range strings.SplitSeq(cell, "\n") {
				runes := []rune(valueLine)
				for len(runes) > columns {
					if err := appendLine(string(runes[:columns]), recordIndex+1, cellIndex+1); err != nil {
						return nil, err
					}
					runes = runes[columns:]
				}
				if err := appendLine(string(runes), recordIndex+1, cellIndex+1); err != nil {
					return nil, err
				}
			}
		}
	}
	return lines, nil
}

func render(ctx context.Context, records [][]string, limits Limits) ([]byte, []Span, int, error) {
	lines, err := layout(ctx, records, limits)
	if err != nil {
		return nil, nil, 0, err
	}
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetCatalogSort(true)
	pdf.SetCompression(true)
	pdf.SetCreationDate(time.Unix(0, 0).UTC())
	pdf.SetModificationDate(time.Unix(0, 0).UTC())
	pdf.SetAutoPageBreak(false, 0)
	pdf.AddUTF8FontFromBytes("GoMono", "", bytes.Clone(gomono.TTF))
	pdf.SetFont("GoMono", "", 10)
	var spans []Span
	pages := 0
	for index, line := range lines {
		if err := ctx.Err(); err != nil {
			return nil, nil, 0, err
		}
		row := index % linesPerPage
		if row == 0 {
			pdf.AddPage()
			pages++
			pdf.Text(20, 14, fmt.Sprintf("CSV conversion, generated page %d", pages))
		}
		pdf.Text(20, 24+float64(row)*5, line.text)
		span := Span{Page: pages, Record: line.record, Cell: line.cell}
		if len(spans) == 0 || spans[len(spans)-1] != span {
			spans = append(spans, span)
		}
	}
	output := boundedWriter{ctx: ctx, maximum: limits.MaxPDFBytes}
	if err := pdf.Output(&output); err != nil {
		if err := ctx.Err(); err != nil {
			return nil, nil, 0, err
		}
		if description, ok := errors.AsType[*ocr.PreparationError](err); ok {
			return nil, nil, 0, description
		}
		return nil, nil, 0, ocr.NewPreparationError("CSV PDF could not be written", err)
	}
	return output.Bytes(), spans, pages, nil
}

type boundedWriter struct {
	bytes.Buffer

	ctx     context.Context
	maximum int64
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > w.maximum-int64(w.Len()) {
		return 0, ocr.NewPreparationError("CSV PDF exceeds byte limit", nil)
	}
	n, err := w.Buffer.Write(p)
	if err != nil {
		return n, ocr.NewPreparationError("CSV PDF could not be buffered", err)
	}
	return n, nil
}
