package renderpdf

import (
	"bytes"
	"errors"
	"io"

	"go.kenn.io/docbank/document/ocr"
)

// Receipt records source, normalized document, and PDF provenance.
type Receipt struct {
	SourceFormat      string `json:"source_format"`
	OriginalFormat    string `json:"original_format"`
	SourceExtension   string `json:"source_extension"`
	SourceSHA256      string `json:"source_sha256"`
	SourceBytes       int64  `json:"source_bytes"`
	NormalizedSHA256  string `json:"normalized_sha256"`
	NormalizedBytes   int64  `json:"normalized_bytes"`
	PDFSHA256         string `json:"pdf_sha256"`
	PDFBytes          int64  `json:"pdf_bytes"`
	Pages             int    `json:"pages"`
	PolicyFingerprint string `json:"policy_fingerprint"`
	ConverterVersion  string `json:"converter_version"`
	RuntimeIdentity   string `json:"runtime_identity"`
	RunnerIdentity    string `json:"runner_identity"`
}

// Result owns verified PDF bytes and their conversion receipt.
type Result struct {
	pdf     []byte
	receipt Receipt
}

// PDF returns a copy of the generated PDF, or nil for a nil result.
func (result *Result) PDF() []byte {
	if result == nil {
		return nil
	}
	return bytes.Clone(result.pdf)
}

// Receipt returns a copy of the conversion provenance, or zero for a nil result.
func (result *Result) Receipt() Receipt {
	if result == nil {
		return Receipt{}
	}
	return result.receipt
}

// Source returns a fresh OCR source over the exact generated PDF.
func (result *Result) Source() (ocr.Source, error) {
	if result == nil || len(result.pdf) == 0 {
		return ocr.Source{}, errors.New("render PDF result is invalid")
	}
	return ocr.NewSource(io.NopCloser(bytes.NewReader(bytes.Clone(result.pdf))),
		"application/pdf", result.receipt.PDFBytes, result.receipt.PDFSHA256)
}
