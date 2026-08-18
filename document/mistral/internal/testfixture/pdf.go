// Package testfixture contains deterministic synthetic document bytes shared
// by Docbank's tests and its consumer test-support package.
package testfixture

import (
	"bytes"
	"encoding/hex"
	"fmt"
)

// MinimalPDF returns a deterministic one-page PDF containing a synthetic label
// in a comment. It is suitable for format and staging tests, not OCR probes.
func MinimalPDF(label string) []byte {
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>",
	}
	var output bytes.Buffer
	_, _ = fmt.Fprintf(&output, "%%PDF-1.4\n%%%s\n", hex.EncodeToString([]byte(label)))
	offsets := make([]int, len(objects))
	for index, object := range objects {
		offsets[index] = output.Len()
		_, _ = fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xref := output.Len()
	_, _ = fmt.Fprintf(&output, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		_, _ = fmt.Fprintf(&output, "%010d 00000 n \n", offset)
	}
	_, _ = fmt.Fprintf(&output, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return output.Bytes()
}
