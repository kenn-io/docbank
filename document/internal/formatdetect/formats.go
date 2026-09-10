package formatdetect

import (
	"fmt"
	"slices"

	"go.kenn.io/docbank/document"
)

const (
	mediaTypeJSON = "application/json"
	mediaTypePDF  = "application/pdf"
	formatIDPDF   = "pdf"
)

// CandidateFormat describes one locally detectable document format. A
// candidate does not authorize an upload.
type CandidateFormat struct {
	ID        string `json:"id"`
	Family    string `json:"family"`
	MediaType string `json:"media_type"`
	UnitKind  string `json:"unit_kind"`
}

var candidateFormats = providerCandidateFormats()

func providerCandidateFormats() []CandidateFormat {
	var formats []CandidateFormat
	for _, metadata := range document.FormatMetadataCatalog() {
		if metadata.Provider {
			formats = append(formats, CandidateFormat{ID: metadata.ID, Family: metadata.Family,
				MediaType: metadata.MediaType, UnitKind: metadata.UnitKind})
		}
	}
	return formats
}

// CandidateFormats returns a defensive copy in stable probe order.
func CandidateFormats() []CandidateFormat {
	return slices.Clone(candidateFormats)
}

// CandidateFormatByID returns the candidate with the given stable identifier.
func CandidateFormatByID(id string) (CandidateFormat, bool) {
	for _, candidate := range candidateFormats {
		if candidate.ID == id {
			return candidate, true
		}
	}
	return CandidateFormat{}, false
}

// CandidateFormatByMediaType returns the one exact canonical declared type.
func CandidateFormatByMediaType(mediaType string) (CandidateFormat, bool) {
	return candidateByMediaType(mediaType)
}

// ProbeFixtureSentinel returns the synthetic phrase required in one fixture.
func ProbeFixtureSentinel(formatID string) (string, error) {
	if _, ok := CandidateFormatByID(formatID); !ok {
		return "", fmt.Errorf("unknown Mistral probe format %q", formatID)
	}
	return "docbank probe " + formatID + " cedar 7319", nil
}
