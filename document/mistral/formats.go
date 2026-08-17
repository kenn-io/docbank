package mistral

import (
	"fmt"
	"slices"
)

const mediaTypePDF = "application/pdf"

// CandidateFormat describes one locally detectable document format. A
// candidate does not authorize an upload.
type CandidateFormat struct {
	ID        string `json:"id"`
	Family    string `json:"family"`
	MediaType string `json:"media_type"`
	UnitKind  string `json:"unit_kind"`
}

var candidateFormats = []CandidateFormat{
	{ID: "pdf", Family: "pdf", MediaType: mediaTypePDF, UnitKind: "page"},
	{ID: "docx", Family: "word", MediaType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", UnitKind: "page"},
	{ID: "doc", Family: "word", MediaType: "application/msword", UnitKind: "page"},
	{ID: "odt", Family: "word", MediaType: "application/vnd.oasis.opendocument.text", UnitKind: "page"},
	{ID: "rtf", Family: "word", MediaType: "application/rtf", UnitKind: "page"},
	{ID: "pptx", Family: "presentation", MediaType: "application/vnd.openxmlformats-officedocument.presentationml.presentation", UnitKind: "slide"},
	{ID: "ppt", Family: "presentation", MediaType: "application/vnd.ms-powerpoint", UnitKind: "slide"},
	{ID: "xlsx", Family: "spreadsheet", MediaType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", UnitKind: "sheet"},
	{ID: "xls", Family: "spreadsheet", MediaType: "application/vnd.ms-excel", UnitKind: "sheet"},
	{ID: "ods", Family: "spreadsheet", MediaType: "application/vnd.oasis.opendocument.spreadsheet", UnitKind: "sheet"},
	{ID: "numbers", Family: "spreadsheet", MediaType: "application/vnd.apple.numbers", UnitKind: "sheet"},
	{ID: "csv", Family: "spreadsheet", MediaType: "text/csv", UnitKind: "record"},
	{ID: "epub", Family: "ebook", MediaType: "application/epub+zip", UnitKind: "spine"},
	{ID: "txt", Family: "text", MediaType: "text/plain", UnitKind: "section"},
	{ID: "markdown", Family: "text", MediaType: "text/markdown", UnitKind: "section"},
	{ID: "rst", Family: "text", MediaType: "text/x-rst", UnitKind: "section"},
	{ID: "latex", Family: "text", MediaType: "application/x-tex", UnitKind: "section"},
	{ID: "json", Family: "structured", MediaType: "application/json", UnitKind: "record"},
	{ID: "jsonl", Family: "structured", MediaType: "application/x-ndjson", UnitKind: "record"},
	{ID: "xml", Family: "structured", MediaType: "application/xml", UnitKind: "record"},
	{ID: "yaml", Family: "structured", MediaType: "application/yaml", UnitKind: "record"},
	{ID: "go", Family: "source", MediaType: "text/x-go", UnitKind: "section"},
	{ID: "python", Family: "source", MediaType: "text/x-python", UnitKind: "section"},
	{ID: "javascript", Family: "source", MediaType: "text/javascript", UnitKind: "section"},
	{ID: "eml", Family: "mail", MediaType: "message/rfc822", UnitKind: "message"},
	{ID: "msg", Family: "mail", MediaType: "application/vnd.ms-outlook", UnitKind: "message"},
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

// ProbeFixtureSentinel returns the synthetic phrase required in one fixture.
func ProbeFixtureSentinel(formatID string) (string, error) {
	if _, ok := CandidateFormatByID(formatID); !ok {
		return "", fmt.Errorf("unknown Mistral probe format %q", formatID)
	}
	return "docbank probe " + formatID + " cedar 7319", nil
}
