package mistral

import (
	"fmt"

	"go.kenn.io/docbank/document/internal/formatdetect"
)

const (
	mediaTypeJSON = "application/json"
	mediaTypePDF  = "application/pdf"
	formatIDPDF   = "pdf"
)

// CandidateFormat describes one locally detectable document format. A
// candidate does not authorize an upload.
type CandidateFormat = formatdetect.CandidateFormat

var candidateFormats = formatdetect.CandidateFormats()

// CandidateFormats returns a defensive copy in stable probe order.
func CandidateFormats() []CandidateFormat {
	return formatdetect.CandidateFormats()
}

// CandidateFormatByID returns the candidate with the given stable identifier.
func CandidateFormatByID(id string) (CandidateFormat, bool) {
	return formatdetect.CandidateFormatByID(id)
}

// ProbeFixtureSentinel returns the synthetic phrase required in one fixture.
func ProbeFixtureSentinel(formatID string) (string, error) {
	if _, ok := CandidateFormatByID(formatID); !ok {
		return "", fmt.Errorf("unknown Mistral probe format %q", formatID)
	}
	return "docbank probe " + formatID + " cedar 7319", nil
}
