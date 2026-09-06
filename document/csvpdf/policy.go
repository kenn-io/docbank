// Package csvpdf converts byte-verified CSV sources to bounded PDFs locally.
package csvpdf

import (
	"crypto/sha256"
	"errors"
	"fmt"

	"go.kenn.io/docbank/internal/canonical"
	"golang.org/x/image/font/gofont/gomono"
)

// ConverterVersion identifies the parser, layout, font and PDF writer contract.
const ConverterVersion = "csvpdf-v1-fpdf-0.9.0"

// Limits bounds one conversion. NewPolicy permits tightening the defaults only.
type Limits struct {
	MaxSourceBytes int64 `json:"max_source_bytes"`
	MaxPDFBytes    int64 `json:"max_pdf_bytes"`
	MaxRecords     int   `json:"max_records"`
	MaxCells       int   `json:"max_cells"`
	MaxCellBytes   int   `json:"max_cell_bytes"`
	MaxPages       int   `json:"max_pages"`
}

// DefaultLimits returns the hard ceilings for a conversion.
func DefaultLimits() Limits {
	return Limits{MaxSourceBytes: 1 << 20, MaxPDFBytes: 10 << 20, MaxRecords: 10_000, MaxCells: 50_000, MaxCellBytes: 64 << 10, MaxPages: 100}
}

// Policy captures immutable conversion limits. Its zero value is invalid.
type Policy struct {
	limits      Limits
	fingerprint string
}

// NewPolicy validates limits and fingerprints the complete conversion contract.
func NewPolicy(limits Limits) (Policy, error) {
	ceiling := DefaultLimits()
	if limits.MaxSourceBytes <= 0 || limits.MaxSourceBytes > ceiling.MaxSourceBytes || limits.MaxPDFBytes <= 0 || limits.MaxPDFBytes > ceiling.MaxPDFBytes || limits.MaxRecords <= 0 || limits.MaxRecords > ceiling.MaxRecords || limits.MaxCells <= 0 || limits.MaxCells > ceiling.MaxCells || limits.MaxCellBytes <= 0 || limits.MaxCellBytes > ceiling.MaxCellBytes || limits.MaxPages <= 0 || limits.MaxPages > ceiling.MaxPages {
		return Policy{}, errors.New("CSV PDF limits must be positive and within DefaultLimits")
	}
	encoded, err := canonical.Marshal(struct {
		Version    string `json:"version"`
		Layout     string `json:"layout"`
		FontSHA256 string `json:"font_sha256"`
		Limits     Limits `json:"limits"`
	}{ConverterVersion, "a4-10pt-mono-80-columns-48-lines-v1", digest(gomono.TTF), limits})
	if err != nil {
		return Policy{}, err
	}
	return Policy{limits: limits, fingerprint: digest(encoded)}, nil
}

// Fingerprint returns the conversion identity; an invalid policy returns empty.
func (p Policy) Fingerprint() string { return p.fingerprint }

func digest(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }
