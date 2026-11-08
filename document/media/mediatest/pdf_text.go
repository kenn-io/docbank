package mediatest

import (
	"fmt"
	"image/color"
	"strings"
)

// PDFTextPages writes one page per string, with a JPEG on every page.
// Empty strings produce image-only pages; whitespace is kept as native text.
func PDFTextPages(pages []string, indirectImageSubtype bool) []byte {
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", "", "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>", ""}
	image := JPEG(8, 8, color.White)
	subtype := "/Image"
	if indirectImageSubtype {
		objects = append(objects, "/Image")
		subtype = "5 0 R"
	}
	objects[3] = fmt.Sprintf("<< /Type /XObject /Subtype %s /Width 8 /Height 8 /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>\nstream\n%s\nendstream", subtype, len(image), image)
	var kids []string
	escape := strings.NewReplacer(`\`, `\\`, "(", `\(`, ")", `\)`)
	for _, text := range pages {
		pageID := len(objects) + 1
		kids = append(kids, fmt.Sprintf("%d 0 R", pageID))
		content := "q 8 0 0 8 72 650 cm /Photo Do Q"
		if text != "" {
			content += " BT /F1 12 Tf 72 720 Td (" + escape.Replace(text) + ") Tj ET"
		}
		objects = append(objects, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> /XObject << /Photo 4 0 R >> >> /Contents %d 0 R >>", pageID+1), fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content))
	}
	objects[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages))
	return PDFObjects("local PDF text", objects)
}
