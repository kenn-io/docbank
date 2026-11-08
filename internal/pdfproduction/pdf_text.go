package pdfproduction

import (
	"context"
	"errors"
	"fmt"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/requests"
)

// PDFTextPolicy binds extraction behavior to the pinned engine and resource policy.
const PDFTextPolicy = "pdftext/v1:count-before-get;native-zero-omissions;literal-markdown;per-page-lease;memory=512MiB;execution=60s;rss=1GiB;text-only-admission;" + wasmSHA256

// PDFTextPage contains the native character count before any text normalization.
type PDFTextPage struct {
	Pages      int
	Characters int
	Text       string
}

// ReadPDFTextPage opens verified bytes in a fresh bounded runtime. Waiting for
// another job's lease uses the caller's context; execution gets the page deadline.
func ReadPDFTextPage(ctx context.Context, data []byte, index int, maxPages int64, maxBytes int) (result PDFTextPage, resultErr error) {
	engine, err := NewPDFium(QualifiedRecipe())
	if err != nil {
		return result, err
	}
	e, ok := engine.(*pdfiumEngine)
	if !ok {
		return result, errors.New("PDF text requires the PDFium engine")
	}
	defer func() { resultErr = errors.Join(resultErr, e.Close()) }()
	e.session, err = wasmBacking.acquire(ctx, uint64(e.recipe.WASMMemoryBytes)) //nolint:gosec // Qualified recipe has a positive, bounded memory limit.
	if err != nil {
		return result, err
	}
	err = e.withInstanceContext(ctx, func(_ context.Context, instance pdfium.Pdfium) error {
		doc, err := instance.OpenDocument(&requests.OpenDocument{File: &data})
		if err != nil {
			return fmt.Errorf("open PDF text: %w", err)
		}
		count, err := instance.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: doc.Document})
		if err != nil {
			return fmt.Errorf("count PDF pages: %w", err)
		}
		if count.PageCount < 1 || int64(count.PageCount) > maxPages || index < 0 || index >= count.PageCount {
			return errors.New("PDF text page limit exceeded")
		}
		result.Pages = count.PageCount
		loaded, err := instance.FPDFText_LoadPage(&requests.FPDFText_LoadPage{Page: requests.Page{ByIndex: &requests.PageByIndex{Document: doc.Document, Index: index}}})
		if err != nil {
			return fmt.Errorf("load PDF text page: %w", err)
		}
		chars, err := instance.FPDFText_CountChars(&requests.FPDFText_CountChars{TextPage: loaded.TextPage})
		if err != nil {
			return fmt.Errorf("count PDF text characters: %w", err)
		}
		if chars.Count < 0 || chars.Count > maxBytes {
			return errors.New("PDF text character limit exceeded")
		}
		result.Characters = chars.Count
		if chars.Count == 0 {
			return nil
		}
		text, err := instance.FPDFText_GetText(&requests.FPDFText_GetText{TextPage: loaded.TextPage, Count: chars.Count})
		if err != nil {
			return fmt.Errorf("read PDF text: %w", err)
		}
		if len(text.Text) > maxBytes {
			return errors.New("PDF text byte limit exceeded")
		}
		result.Text = text.Text
		return nil
	})
	return result, err
}
