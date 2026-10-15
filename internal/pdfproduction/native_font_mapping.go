package pdfproduction

import (
	"context"
	"fmt"
	"slices"
	"unicode"
	"unicode/utf8"

	"github.com/klippa-app/go-pdfium/references"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/structs"
)

func (p *nativeFontProof) whitespaceCodes(ctx context.Context,
	font references.FPDF_FONT) (codes []uint32, retErr error) {
	page, err := p.scratchPage(6144, 2400)
	if err != nil {
		return nil, err
	}
	defer func() { finishFontScratch(&retErr, page.close()) }()
	owners := make(map[structs.FPDF_FS_MATRIX]int, 256)
	for code := range 256 {
		if err := p.available(ctx); err != nil {
			return nil, err
		}
		matrix := structs.FPDF_FS_MATRIX{A: 1, D: 1, E: float32(32 + (code%16)*384),
			F: float32(64 + (code/16)*144)}
		_, err = page.textObject(font, []uint32{'L', uint32(code), 'R'}, matrix)
		if err != nil {
			return nil, err
		}
		owners[matrix] = code
	}
	text, err := p.instance.FPDFText_LoadPage(&requests.FPDFText_LoadPage{Page: page.ref()})
	if err != nil {
		return nil, fmt.Errorf("load font mapping page: %w", err)
	}
	defer func() {
		_, err := p.instance.FPDFText_ClosePage(&requests.FPDFText_ClosePage{TextPage: text.TextPage})
		finishFontScratch(&retErr, err)
	}()
	readings, err := p.readFontMappings(ctx, text.TextPage, owners)
	if err != nil {
		return nil, err
	}
	if !slices.Equal(readings['L'], []rune("LLR")) || !slices.Equal(readings['R'], []rune("LRR")) {
		return nil, errFontUnproven
	}
	for code, r := range &readings {
		if len(r) < 3 || len(r) > 18 || r[0] != 'L' || r[len(r)-1] != 'R' {
			return nil, errFontUnproven
		}
		if slices.ContainsFunc(r[1:len(r)-1], unicode.IsSpace) {
			codes = append(codes, uint32(code))
		}
	}
	return codes, nil
}

func (p *nativeFontProof) readFontMappings(ctx context.Context, text references.FPDF_TEXTPAGE,
	owners map[structs.FPDF_FS_MATRIX]int) ([256][]rune, error) {
	var readings [256][]rune
	count, err := p.instance.FPDFText_CountChars(&requests.FPDFText_CountChars{TextPage: text})
	if err != nil {
		return readings, fmt.Errorf("count font observations: %w", err)
	}
	if count.Count < 1 || count.Count > maxFontObservations {
		return readings, errFontUnproven
	}
	for i := range count.Count {
		if err := p.available(ctx); err != nil {
			return readings, err
		}
		generated, err := p.instance.FPDFText_IsGenerated(&requests.FPDFText_IsGenerated{TextPage: text,
			Index: i})
		if err != nil {
			return readings, fmt.Errorf("read generated flag: %w", err)
		}
		if generated.IsGenerated {
			continue
		}
		object, err := p.instance.FPDFText_GetTextObject(&requests.FPDFText_GetTextObject{
			TextPage: text, Index: i,
		})
		if err != nil {
			if err.Error() == "could not get object" {
				return readings, errFontUnproven
			}
			return readings, fmt.Errorf("read font observation owner: %w", err)
		}
		matrix, err := p.instance.FPDFPageObj_GetMatrix(&requests.FPDFPageObj_GetMatrix{
			PageObject: object.TextObject,
		})
		if err != nil {
			return readings, fmt.Errorf("read font observation matrix: %w", err)
		}
		code, known := owners[matrix.Matrix]
		if !known {
			return readings, errFontUnproven
		}
		value, err := p.instance.FPDFText_GetUnicode(&requests.FPDFText_GetUnicode{TextPage: text,
			Index: i})
		if err != nil {
			return readings, fmt.Errorf("read font mapping: %w", err)
		}
		if value.Unicode > utf8.MaxRune || value.Unicode >= 0xd800 && value.Unicode <= 0xdfff {
			return readings, errFontUnproven
		}
		readings[code] = append(readings[code], rune(value.Unicode))
		if len(readings[code]) > 18 {
			return readings, errFontUnproven
		}
	}
	return readings, nil
}
