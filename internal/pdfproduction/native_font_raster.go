package pdfproduction

import (
	"context"
	"errors"
	"fmt"

	"github.com/klippa-app/go-pdfium/references"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/structs"
)

func (p *nativeFontProof) blankCodes(ctx context.Context, font references.FPDF_FONT,
	codes []uint32) (blank bool, retErr error) {
	page, err := p.scratchPage(288, 288)
	if err != nil {
		return false, err
	}
	defer func() { finishFontScratch(&retErr, page.close()) }()
	object, err := page.textObject(font, codes[:1], structs.FPDF_FS_MATRIX{A: 1, D: 1, E: 72, F: 144})
	if err != nil {
		return false, err
	}
	bitmap, err := p.instance.FPDFBitmap_Create(&requests.FPDFBitmap_Create{Width: 1200,
		Height: 1200, Alpha: 1})
	if err != nil {
		return false, fmt.Errorf("create font bitmap: %w", err)
	}
	defer func() {
		_, err := p.instance.FPDFBitmap_Destroy(&requests.FPDFBitmap_Destroy{Bitmap: bitmap.Bitmap})
		finishFontScratch(&retErr, err)
	}()
	for _, code := range codes {
		if err := p.available(ctx); err != nil {
			return false, err
		}
		if p.renders >= maxFontRenders {
			return false, errFontUnproven
		}
		p.renders++
		_, err = p.instance.FPDFText_SetCharcodes(&requests.FPDFText_SetCharcodes{PageObject: object,
			CharCodes: []uint32{code}})
		if err != nil {
			return false, fmt.Errorf("set font probe code: %w", err)
		}
		_, err = p.instance.FPDFBitmap_FillRect(&requests.FPDFBitmap_FillRect{Bitmap: bitmap.Bitmap,
			Width: 1200, Height: 1200, Color: 0})
		if err != nil {
			return false, fmt.Errorf("clear font bitmap: %w", err)
		}
		_, err = p.instance.FPDF_RenderPageBitmap(&requests.FPDF_RenderPageBitmap{Bitmap: bitmap.Bitmap,
			Page: page.ref(), SizeX: 1200, SizeY: 1200})
		if err != nil {
			return false, fmt.Errorf("render font probe: %w", err)
		}
		pixels, err := p.instance.FPDFBitmap_GetBuffer(&requests.FPDFBitmap_GetBuffer{Bitmap: bitmap.Bitmap})
		if err != nil {
			return false, fmt.Errorf("read font pixels: %w", err)
		}
		if len(pixels.Buffer) != 1200*1200*4 {
			return false, errors.New("font probe bitmap size mismatch")
		}
		for i := 3; i < len(pixels.Buffer); i += 4 {
			if pixels.Buffer[i] != 0 {
				return false, nil
			}
		}
	}
	return true, nil
}
