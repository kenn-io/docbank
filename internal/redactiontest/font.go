package redactiontest

import (
	"fmt"
	"strings"
)

// FontPDF places direct text under F1 with an optional ToUnicode stream at object 6.
func FontPDF(content, font, cmap string, extra ...string) []byte {
	stream := func(s string) string {
		return fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream",
			len(s), s)
	}
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << " +
			"/F1 4 0 R >> >> /Contents 5 0 R >>",
		font, stream(content), stream(cmap),
	}
	objects = append(objects, extra...)
	var pdf strings.Builder
	pdf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for i, obj := range objects {
		offsets[i] = pdf.Len()
		fmt.Fprintf(&pdf, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := pdf.Len()
	fmt.Fprintf(&pdf, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&pdf, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&pdf, "trailer << /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1,
		xref)
	return []byte(pdf.String())
}
