package csvpdf

import (
	"bytes"
	"errors"
	"io"
	"slices"

	"go.kenn.io/docbank/document/ocr"
)

// Span associates a generated PDF page with a one-based CSV record and cell.
type Span struct {
	Page   int `json:"page"`
	Record int `json:"record"`
	Cell   int `json:"cell"`
}

// Receipt records conversion provenance, not permission to upload either file.
type Receipt struct {
	SourceSHA256      string `json:"source_sha256"`
	SourceBytes       int64  `json:"source_bytes"`
	PDFSHA256         string `json:"pdf_sha256"`
	PDFBytes          int64  `json:"pdf_bytes"`
	Pages             int    `json:"pages"`
	PolicyFingerprint string `json:"policy_fingerprint"`
	ConverterVersion  string `json:"converter_version"`
	Spans             []Span `json:"spans"`
}

// Result owns verified PDF bytes and their conversion receipt.
type Result struct {
	pdf     []byte
	receipt Receipt
}

// PDF returns a copy of the generated PDF, or nil for a nil or zero result.
func (r *Result) PDF() []byte {
	if r == nil {
		return nil
	}
	return bytes.Clone(r.pdf)
}

// Receipt returns a deep copy of the provenance, or zero for a nil result.
func (r *Result) Receipt() Receipt {
	if r == nil {
		return Receipt{}
	}
	receipt := r.receipt
	receipt.Spans = slices.Clone(receipt.Spans)
	return receipt
}

// Source returns a fresh stream over the generated PDF for the existing OCR API.
func (r *Result) Source() (ocr.Source, error) {
	if r == nil || len(r.pdf) == 0 {
		return ocr.Source{}, errors.New("CSV PDF result is invalid")
	}
	return ocr.NewSource(io.NopCloser(bytes.NewReader(bytes.Clone(r.pdf))), "application/pdf", r.receipt.PDFBytes, r.receipt.PDFSHA256)
}
