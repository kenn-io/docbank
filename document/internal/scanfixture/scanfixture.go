// Package scanfixture provides deterministic synthetic inputs for measuring
// Docbank's existing PDF evidence APIs. It deliberately makes no scan-routing
// decisions.
package scanfixture

import (
	"bytes"
	_ "embed"
	"fmt"
	"image/color"
	"strconv"
	"strings"

	"go.kenn.io/docbank/document/media/mediatest"
)

const (
	oversizedBytes = 134217729
	mediaTypePDF   = "application/pdf"
)

var (
	// These parser-valid seeds are frozen because PDF encryption uses random
	// salts and mediatest.WebP is intentionally only a header fixture.
	//go:embed testdata/encrypted.pdf
	encryptedPDF []byte

	//go:embed testdata/single-image.webp
	singleImageWebP []byte
)

// Fixture is one synthetic input. Generated inputs are built separately so
// ordinary corpus measurements do not allocate the oversized document.
type Fixture struct {
	Name      string
	MediaType string
	Bytes     []byte
	Generated bool
}

// Corpus returns fixtures in stable name order with fresh bytes on each call.
func Corpus() []Fixture {
	white := color.White
	lowContrast := color.RGBA{R: 126, G: 126, B: 126, A: 255}
	return []Fixture{
		{Name: "blank", MediaType: mediaTypePDF, Bytes: blankPDF()},
		{
			Name: "born-digital", MediaType: mediaTypePDF,
			Bytes: textPDF(3, "Born digital synthetic text fills this page with readable content."),
		},
		{Name: "encrypted", MediaType: mediaTypePDF, Bytes: bytes.Clone(encryptedPDF)},
		{
			Name: "image-only", MediaType: mediaTypePDF,
			Bytes: imagePDF(3, mediatest.JPEG(8, 8, white)),
		},
		{
			Name: "low-quality", MediaType: mediaTypePDF,
			Bytes: imagePDF(1, mediatest.JPEG(8, 8, lowContrast)),
		},
		{Name: "malformed", MediaType: mediaTypePDF, Bytes: malformedPDF()},
		{Name: "mixed-above-ratio", MediaType: mediaTypePDF, Bytes: mixedPDF(8, 2)},
		{Name: "mixed-below-ratio", MediaType: mediaTypePDF, Bytes: mixedPDF(1, 1)},
		{Name: "oversized", MediaType: mediaTypePDF, Generated: true},
		{
			Name: "rotated", MediaType: mediaTypePDF,
			Bytes: rotatedPDF(mediatest.JPEG(8, 8, white)),
		},
		{Name: "single-image-jpeg", MediaType: "image/jpeg", Bytes: mediatest.JPEG(8, 8, white)},
		{Name: "single-image-png", MediaType: "image/png", Bytes: mediatest.PNG(8, 8, white)},
		{Name: "single-image-webp", MediaType: "image/webp", Bytes: bytes.Clone(singleImageWebP)},
		{Name: "undecodable-font", MediaType: mediaTypePDF, Bytes: undecodableFontPDF()},
		{Name: "unsupported-filter", MediaType: mediaTypePDF, Bytes: unsupportedFilterPDF()},
		{Name: "unsupported-format", MediaType: "audio/wav", Bytes: mediatest.WAV()},
	}
}

// OversizedBytes builds a two-page PDF one byte over 128 MiB. Padding lives in
// the header comment so readers find startxref and EOF near the end of the file.
func OversizedBytes() []byte {
	objects := pageObjects(make([]pageKind, 2), "Oversized synthetic document text.", nil)
	base := mediatest.PDFObjects("oversized", objects)
	xref := bytes.Index(base, []byte("\nxref\n")) + 1
	// Object offsets have fixed-width entries; only startxref grows in digits.
	xrefGrowth := len(strconv.Itoa(oversizedBytes)) - len(strconv.Itoa(xref))
	padding := oversizedBytes - len(base) - xrefGrowth
	return mediatest.PDFObjects("oversized"+strings.Repeat("x", padding), objects)
}

type pageKind int

const (
	pageText pageKind = iota
	pageImage
)

func textPDF(pages int, phrase string) []byte {
	kinds := make([]pageKind, pages)
	return mediatest.PDFObjects("text", pageObjects(kinds, phrase, nil))
}

func imagePDF(pages int, payload []byte) []byte {
	kinds := make([]pageKind, pages)
	for index := range kinds {
		kinds[index] = pageImage
	}
	return mediatest.PDFObjects("image", pageObjects(kinds, "", payload))
}

func mixedPDF(textPages, imagePages int) []byte {
	kinds := make([]pageKind, 0, textPages+imagePages)
	for range textPages {
		kinds = append(kinds, pageText)
	}
	for range imagePages {
		kinds = append(kinds, pageImage)
	}
	return mediatest.PDFObjects("mixed", pageObjects(kinds,
		"Mixed synthetic pages contain readable text.", mediatest.JPEG(8, 8, color.White)))
}

func pageObjects(kinds []pageKind, phrase string, imagePayload []byte) []string {
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", ""}
	pageNumbers := make([]int, 0, len(kinds))
	for _, kind := range kinds {
		pageNumber := len(objects) + 1
		contents := pageNumber + 1
		pageNumbers = append(pageNumbers, pageNumber)
		switch kind {
		case pageText:
			page, content := textPage(2, contents, phrase)
			objects = append(objects, page, content)
		case pageImage:
			xobject := contents + 1
			page, content, image := imagePage(2, contents, xobject, "DCTDecode", string(imagePayload))
			objects = append(objects, page, content, image)
		}
	}
	objects[1] = pagesObject(pageNumbers)
	return objects
}

func pagesObject(pageNumbers []int) string {
	var kids strings.Builder
	for _, number := range pageNumbers {
		_, _ = fmt.Fprintf(&kids, "%d 0 R ", number)
	}
	return fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids.String(), len(pageNumbers))
}

func blankPDF() []byte {
	return mediatest.PDFObjects("blank", []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>",
	})
}

func malformedPDF() []byte {
	return mediatest.PDFObjects("malformed", []string{"<< /Type /Catalog /Pages 99 0 R >>"})
}

func rotatedPDF(payload []byte) []byte {
	page, content, image := imagePage(2, 4, 5, "DCTDecode", string(payload))
	page = strings.TrimSuffix(page, " >>") + " /Rotate 90 >>"
	return mediatest.PDFObjects("rotated", []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		page, content, image,
	})
}

func undecodableFontPDF() []byte {
	stream := "BT /F1 12 Tf 72 720 Td (" + strings.Repeat("\\001", 32) + ") Tj ET"
	return mediatest.PDFObjects("undecodable-font", []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792]" +
			" /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica" +
			" /Encoding << /Type /Encoding /Differences [1 /synthetic_missing] >> >>",
	})
}

func unsupportedFilterPDF() []byte {
	stream := "synthetic opaque page content"
	return mediatest.PDFObjects("unsupported-filter", []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R >>",
		fmt.Sprintf("<< /Length %d /Filter /UnknownDecode >>\nstream\n%s\nendstream",
			len(stream), stream),
	})
}

// textPage returns the page and content-stream objects for one born-digital
// page. parent is the /Pages object number and contents is the object number
// this page's content stream will occupy.
func textPage(parent, contents int, phrase string) (page, content string) {
	escaped := strings.NewReplacer("\\", "\\\\", "(", "\\(", ")", "\\)").Replace(phrase)
	stream := "BT /F1 12 Tf 72 720 Td (" + escaped + ") Tj ET"
	return fmt.Sprintf("<< /Type /Page /Parent %d 0 R /MediaBox [0 0 612 792]"+
			" /Resources << /Font << /F1 << /Type /Font /Subtype /Type1"+
			" /BaseFont /Helvetica >> >> >> /Contents %d 0 R >>", parent, contents),
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream)
}

// imagePage returns the page, content-stream and image XObject objects for one
// scanned page. Every numeric argument is an object number; filter is the
// XObject filter name without its leading slash; payload is the raw stream.
func imagePage(
	parent, contents, xobject int, filter, payload string,
) (page, content, image string) {
	stream := "q 612 0 0 792 0 0 cm /Im0 Do Q"
	return fmt.Sprintf("<< /Type /Page /Parent %d 0 R /MediaBox [0 0 612 792]"+
			" /Resources << /XObject << /Im0 %d 0 R >> >> /Contents %d 0 R >>",
			parent, xobject, contents),
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width 8 /Height 8"+
			" /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /%s /Length %d >>"+
			"\nstream\n%s\nendstream", filter, len(payload), payload)
}
