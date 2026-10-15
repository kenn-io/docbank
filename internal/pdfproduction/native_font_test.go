package pdfproduction

import (
	"context"
	"strings"
	"testing"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/references"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/redactiontest"
)

func TestNativeFontProofActualEncoding(t *testing.T) {
	for _, tc := range []struct {
		name, font, cmap string
		want             whitespaceProof
	}{
		{"benign Differences", "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding << " +
			"/BaseEncoding /WinAnsiEncoding /Differences [65 /space] >> >>", "", whitespaceBlank},
		{"unsupported Symbol", "<< /Type /Font /Subtype /Type1 /BaseFont /Symbol >>", "",
			whitespaceUnsupported},
		{"widths and descriptor", "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding " +
			"/WinAnsiEncoding /FirstChar 0 /LastChar 255 /Widths [" + strings.Repeat("600 ",
			256) + "] /FontDescriptor << /Type /FontDescriptor /FontName /Helvetica /Flags 32 " +
			"/FontBBox [-166 -225 1000 931] /ItalicAngle 0 /Ascent 718 /Descent -207 " +
			"/CapHeight 718 /StemV 88 >> >>", "", whitespaceBlank},
		{"ordinary", "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>", "", whitespaceBlank},
		{"Differences paints A", "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding << " +
			"/BaseEncoding /WinAnsiEncoding /Differences [32 /A] >> /ToUnicode 6 0 R >>",
			"1 begincodespacerange <00> <FF> endcodespacerange 1 beginbfchar <20> <0020> endbfchar",
			whitespaceInked},
		{"unused inked alias", "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding " +
			"/WinAnsiEncoding /ToUnicode 6 0 R >>",
			"1 begincodespacerange <00> <FF> endcodespacerange 1 beginbfchar <41> <0020> endbfchar",
			whitespaceInked},
		{"changed anchor", "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding " +
			"/WinAnsiEncoding /ToUnicode 6 0 R >>",
			"1 begincodespacerange <00> <FF> endcodespacerange 1 beginbfchar <4C> <0041> endbfchar",
			whitespaceUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := redactiontest.FontPDF("BT /F1 12 Tf 72 720 Td (L R) Tj ET", tc.font, tc.cmap)
			withNativeFont(t, data, func(ctx context.Context, proof *nativeFontProof, page requests.Page,
				object references.FPDF_PAGEOBJECT) {
				result, err := proof.check(ctx, object)
				require.NoError(t, err)
				require.Equal(t, tc.want, result)
			})
		})
	}
}

func TestNativeFontProofLimitsAndRestoration(t *testing.T) {
	data := redactiontest.FontPDF("BT /F1 12 Tf 72 720 Td (L R) Tj ET",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>", "")
	withNativeFont(t, data, func(ctx context.Context, proof *nativeFontProof, page requests.Page,
		object references.FPDF_PAGEOBJECT) {
		source := document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001",
			SHA256: hashBytes(data), Size: int64(len(data))}
		frame, err := document.NewPDFPageFrame(source, 1,
			[4]float64{0, 0, 612, 792}, [4]float64{0, 0, 612, 792}, 0)
		require.NoError(t, err)
		before, cleanup, err := renderNativePage(proof.instance, proof.document, page, frame)
		require.NoError(t, err)
		defer cleanup()
		for _, tc := range []struct {
			name  string
			limit func(*nativeFontProof)
			want  whitespaceProof
		}{
			{"complete", func(p *nativeFontProof) {}, whitespaceBlank},
			{"batches", func(p *nativeFontProof) { p.batches = maxFontBatches }, whitespaceUnsupported},
			{"renders", func(p *nativeFontProof) { p.renders = maxFontRenders - 1 }, whitespaceUnsupported},
		} {
			t.Run(tc.name, func(t *testing.T) {
				p := newNativeFontProof(proof.instance, proof.document)
				tc.limit(p)
				result, err := p.check(ctx, object)
				require.NoError(t, err)
				require.Equal(t, tc.want, result)
				count, err := p.instance.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: p.document})
				require.NoError(t, err)
				require.Equal(t, 1, count.PageCount)
				after, cleanup, err := renderNativePage(p.instance, p.document, page, frame)
				require.NoError(t, err)
				defer cleanup()
				equal, err := nativeImagesEqual(ctx, before, after)
				require.NoError(t, err)
				require.True(t, equal, "source pixels changed during font proof")
			})
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		_, err = proof.check(canceled, object)
		require.ErrorIs(t, err, context.Canceled)
	})
}

func withNativeFont(t *testing.T, data []byte, check func(context.Context, *nativeFontProof,
	requests.Page, references.FPDF_PAGEOBJECT)) {
	t.Helper()
	engine, err := NewPDFium(QualifiedRecipe())
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()
	native, ok := engine.(*pdfiumEngine)
	require.True(t, ok)
	err = native.withInstanceContext(t.Context(), func(ctx context.Context,
		p pdfium.Pdfium) error {
		doc, err := p.OpenDocument(&requests.OpenDocument{File: &data})
		require.NoError(t, err)
		defer func() {
			_, err := p.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc.Document})
			require.NoError(t, err)
		}()
		page, err := p.FPDF_LoadPage(&requests.FPDF_LoadPage{Document: doc.Document, Index: 0})
		require.NoError(t, err)
		defer func() {
			_, err := p.FPDF_ClosePage(&requests.FPDF_ClosePage{Page: page.Page})
			require.NoError(t, err)
		}()
		ref := requests.Page{ByReference: &page.Page}
		object, err := p.FPDFPage_GetObject(&requests.FPDFPage_GetObject{Page: ref, Index: 0})
		require.NoError(t, err)
		check(ctx, newNativeFontProof(p, doc.Document), ref, object.PageObject)
		return nil
	})
	require.NoError(t, err)
}
