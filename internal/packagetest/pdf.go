// Package packagetest contains independent readers shared by package tests.
package packagetest

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// PDFText extracts visible PDF text with Poppler. Product code does not depend
// on Poppler; this reader keeps stamp verification independent from pdfcpu.
func PDFText(ctx context.Context, pdf []byte) (string, error) {
	command := exec.CommandContext(ctx, "pdftotext", "-enc", "UTF-8", "-", "-")
	command.Stdin = bytes.NewReader(pdf)
	var output bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &output
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("independent PDF reader: %w: %s", err, stderr.String())
	}
	return output.String(), nil
}
