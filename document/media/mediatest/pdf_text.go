package mediatest

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"math/rand/v2"
	"strings"
)

// PDFTextPages writes one page per string, with a JPEG on every page.
// Empty strings produce image-only pages; whitespace is kept as native text.
func PDFTextPages(pages []string, indirectImageSubtype bool) []byte {
	return pdfTextPages(pages, indirectImageSubtype, false)
}

// PDFTextScanPages puts a distinct JPEG scan on each page, totaling about 31 MiB.
func PDFTextScanPages(pages []string) []byte {
	return pdfTextPages(pages, false, true)
}

func pdfTextPages(pages []string, indirectImageSubtype, scans bool) []byte {
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", "", "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>", ""}
	sharedImage := JPEG(8, 8, color.White)
	subtype := "/Image"
	if indirectImageSubtype {
		objects = append(objects, "/Image")
		subtype = "5 0 R"
	}
	objects[3] = fmt.Sprintf("<< /Type /XObject /Subtype %s /Width 8 /Height 8 /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>\nstream\n%s\nendstream", subtype, len(sharedImage), sharedImage)
	var kids []string
	random := rand.New(rand.NewPCG(71, 19)) //nolint:gosec // Synthetic scans must reproduce the same bytes.
	edge := max(8, int(math.Sqrt(float64(52_000_000)/float64(max(1, len(pages))))))
	escape := strings.NewReplacer(`\`, `\\`, "(", `\(`, ")", `\)`)
	for _, text := range pages {
		imageID := 4
		if scans {
			photo := image.NewRGBA(image.Rect(0, 0, edge, edge))
			for i := 0; i < len(photo.Pix); i += 4 {
				photo.Pix[i], photo.Pix[i+1], photo.Pix[i+2], photo.Pix[i+3] = byte(random.Uint32()&255), byte(random.Uint32()&255), byte(random.Uint32()&255), 255
			}
			var encoded bytes.Buffer
			if err := jpeg.Encode(&encoded, photo, &jpeg.Options{Quality: 75}); err != nil {
				panic(err)
			}
			imageID = len(objects) + 1
			objects = append(objects, fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>\nstream\n%s\nendstream", edge, edge, encoded.Len(), encoded.Bytes()))
		}
		pageID := len(objects) + 1
		kids = append(kids, fmt.Sprintf("%d 0 R", pageID))
		content := "q 8 0 0 8 72 650 cm /Photo Do Q"
		if text != "" {
			content += " BT /F1 12 Tf 72 720 Td (" + escape.Replace(text) + ") Tj ET"
		}
		objects = append(objects, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> /XObject << /Photo %d 0 R >> >> /Contents %d 0 R >>", imageID, pageID+1), fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content))
	}
	objects[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages))
	return PDFObjects("local PDF text", objects)
}
