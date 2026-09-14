package scanfixture

import (
	"errors"

	"go.kenn.io/docbank/document/internal/formatdetect"
)

// Measurement records only what Docbank's existing PDF evidence APIs can
// establish. It intentionally carries no scan classification or thresholds.
type Measurement struct {
	Name          string
	Skipped       string `json:",omitzero"`
	Pages         int64
	PageError     string
	Inspect       formatdetect.PDFMeasurements
	InspectError  string
	MetadataError string
	Unbounded     bool
}

// Measure records existing page, stream, and metadata API results for a small
// committed PDF fixture using the processing service's limits formula for
// the given source-size bound. Other media and generated inputs are skipped.
func Measure(f Fixture, maxSourceBytes int64) Measurement {
	measurement := Measurement{Name: f.Name}
	if f.MediaType != mediaTypePDF {
		measurement.Skipped = "not_pdf"
		return measurement
	}
	if f.Generated {
		measurement.Skipped = "generated_input"
		return measurement
	}
	source := f.Bytes
	var err error
	measurement.Pages, err = formatdetect.CountPDFPages(source)
	if err != nil {
		measurement.PageError = err.Error()
	}
	measurement.Inspect, err = formatdetect.InspectPDF(source, formatdetect.PDFLimits{
		MaxExpandedBytes: maxSourceBytes,
		MaxEntryBytes:    maxSourceBytes,
		MaxEntries:       100_000,
	})
	measurement.Unbounded = errors.Is(err, formatdetect.ErrPDFUnbounded)
	if err != nil {
		measurement.InspectError = err.Error()
	}
	_, err = formatdetect.ReadPDFMetadata(source)
	if err != nil {
		measurement.MetadataError = err.Error()
	}
	return measurement
}
