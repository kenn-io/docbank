package pdfproduction

import (
	"errors"
	"fmt"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/references"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/structs"
)

type fontScratchPage struct {
	instance pdfium.Pdfium
	document references.FPDF_DOCUMENT
	page     references.FPDF_PAGE
	index    int
}

func (p *nativeFontProof) scratchPage(width, height float64) (*fontScratchPage, error) {
	count, err := p.instance.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: p.document})
	if err != nil {
		return nil, fmt.Errorf("count source pages: %w", err)
	}
	page, err := p.instance.FPDFPage_New(&requests.FPDFPage_New{
		Document: p.document, PageIndex: count.PageCount, Width: width, Height: height,
	})
	if err != nil {
		return nil, fmt.Errorf("append font probe page: %w", err)
	}
	return &fontScratchPage{p.instance, p.document, page.Page, count.PageCount}, nil
}

func (s *fontScratchPage) ref() requests.Page { return requests.Page{ByReference: &s.page} }

func (s *fontScratchPage) close() error {
	_, closeErr := s.instance.FPDF_ClosePage(&requests.FPDF_ClosePage{Page: s.page})
	_, deleteErr := s.instance.FPDFPage_Delete(&requests.FPDFPage_Delete{Document: s.document,
		PageIndex: s.index})
	count, countErr := s.instance.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: s.document})
	if countErr == nil && count.PageCount != s.index {
		countErr = errors.New("font probe changed source page inventory")
	}
	return errors.Join(closeErr, deleteErr, countErr)
}

func (s *fontScratchPage) textObject(font references.FPDF_FONT, codes []uint32,
	matrix structs.FPDF_FS_MATRIX) (object references.FPDF_PAGEOBJECT, retErr error) {
	created, err := s.instance.FPDFPageObj_CreateTextObj(&requests.FPDFPageObj_CreateTextObj{
		Document: s.document, Font: font, FontSize: 72,
	})
	if err != nil {
		return "", fmt.Errorf("create font probe object: %w", err)
	}
	inserted := false
	defer func() {
		if !inserted {
			_, err := s.instance.FPDFPageObj_Destroy(&requests.FPDFPageObj_Destroy{
				PageObject: created.PageObject,
			})
			retErr = errors.Join(retErr, err)
		}
	}()
	_, err = s.instance.FPDFPageObj_Transform(&requests.FPDFPageObj_Transform{
		PageObject: created.PageObject, Transform: matrix,
	})
	if err != nil {
		return "", fmt.Errorf("position font probe: %w", err)
	}
	_, err = s.instance.FPDFText_SetCharcodes(&requests.FPDFText_SetCharcodes{
		PageObject: created.PageObject, CharCodes: codes,
	})
	if err != nil {
		return "", fmt.Errorf("set font probe codes: %w", err)
	}
	_, err = s.instance.FPDFPage_InsertObject(&requests.FPDFPage_InsertObject{Page: s.ref(),
		PageObject: created.PageObject})
	if err != nil {
		return "", fmt.Errorf("insert font probe object: %w", err)
	}
	inserted = true
	return created.PageObject, nil
}

// Preserve an incomplete-proof sentinel only when cleanup itself succeeded.
func finishFontScratch(result *error, cleanup error) {
	if cleanup != nil {
		*result = errors.Join(*result, fmt.Errorf("clean up font probe: %w", cleanup))
	}
}
