// Package formatqualification owns the immutable fixture qualifications used
// by format coverage. It deliberately has no document, coverage, or processing
// dependency, so contract validation and live source composition can consult
// the same authority without an import cycle.
package formatqualification

import (
	"slices"
	"strings"
)

// Capability is the qualification manifest's dependency-neutral copy of the
// public capability vocabulary. Consumers must convert their typed capability
// at the package boundary.
type Capability string

const (
	CapabilityDetect     Capability = "detect"
	CapabilityRetain     Capability = "retain"
	CapabilityMetadata   Capability = "metadata"
	CapabilityExpand     Capability = "expand"
	CapabilityText       Capability = "text"
	CapabilityPages      Capability = "pages"
	CapabilityTranscript Capability = "transcript"
)

// InputKind identifies the bytes exercised by a qualification fixture.
type InputKind string

const InputOriginalFile InputKind = "original_file"

// Qualification binds one real test to one catalog capability and one pinned
// implementation or provider descriptor identity. Exactly matching this tuple
// is the only way runtime composition can consume a qualification.
type Qualification struct {
	CatalogID             string
	Capability            Capability
	Evidence              string
	ImplementationID      string
	DescriptorFingerprint string
	InputKind             InputKind
}

// Query is the exact typed lookup key for a qualification.
type Query Qualification

// EvidenceQuery is the narrower key available to the wire-contract validator,
// which cannot observe every local implementation identity. Runtime coverage
// composition must use Query and Lookup instead.
type EvidenceQuery struct {
	CatalogID  string
	Capability Capability
	Evidence   string
}

const (
	documentDetectionEvidence = "TestDetectFormatRecognizesBoundedDocumentFamilies"
	documentDetectionID       = "docbank-document-format-detection:pdf+ooxml+epub+compound+rtf+utf8-structured-text:v1"
	mediaDetectionID          = "docbank-media-detection:jpeg+png+webp+gif+iso-bmff:v1"
	originalRetentionID       = "docbank-original-retention:verified-blob+ingest-authority:v1"
	sourceMetadataExtractorID = "42b01ef9219b3b35dedf49ef98d311b21772da27631c3ca90597f28363de1ec5"
)

var baseQualifications = []Qualification{
	{CatalogID: "calendar", Capability: CapabilityMetadata, Evidence: "TestExtractSourceMetadataFromSyntheticFormats", ImplementationID: sourceMetadataExtractorID, InputKind: InputOriginalFile},
	{CatalogID: "csv", Capability: CapabilityDetect, Evidence: documentDetectionEvidence, ImplementationID: documentDetectionID, InputKind: InputOriginalFile},
	{CatalogID: "doc", Capability: CapabilityDetect, Evidence: documentDetectionEvidence, ImplementationID: documentDetectionID, InputKind: InputOriginalFile},
	{CatalogID: "docx", Capability: CapabilityDetect, Evidence: documentDetectionEvidence, ImplementationID: documentDetectionID, InputKind: InputOriginalFile},
	{CatalogID: "docx", Capability: CapabilityMetadata, Evidence: "TestExtractSourceMetadataFromSyntheticDOCX", ImplementationID: sourceMetadataExtractorID, InputKind: InputOriginalFile},
	{CatalogID: "eml", Capability: CapabilityDetect, Evidence: documentDetectionEvidence, ImplementationID: documentDetectionID, InputKind: InputOriginalFile},
	{CatalogID: "eml", Capability: CapabilityMetadata, Evidence: "TestExtractSourceMetadataFromSyntheticFormats", ImplementationID: sourceMetadataExtractorID, InputKind: InputOriginalFile},
	{CatalogID: "epub", Capability: CapabilityDetect, Evidence: documentDetectionEvidence, ImplementationID: documentDetectionID, InputKind: InputOriginalFile},
	{CatalogID: "gif", Capability: CapabilityDetect, Evidence: "TestDetectBytesRecognizesSupportedContainers", ImplementationID: mediaDetectionID, InputKind: InputOriginalFile},
	{CatalogID: "gif", Capability: CapabilityMetadata, Evidence: "TestExtractSourceMetadataReadsVisualContainerFacts", ImplementationID: sourceMetadataExtractorID, InputKind: InputOriginalFile},
	{CatalogID: "jpeg", Capability: CapabilityDetect, Evidence: "TestDetectBytesRecognizesSupportedContainers", ImplementationID: mediaDetectionID, InputKind: InputOriginalFile},
	{CatalogID: "jpeg", Capability: CapabilityMetadata, Evidence: "TestExtractSourceMetadataFromSyntheticFormats", ImplementationID: sourceMetadataExtractorID, InputKind: InputOriginalFile},
	{CatalogID: "json", Capability: CapabilityDetect, Evidence: documentDetectionEvidence, ImplementationID: documentDetectionID, InputKind: InputOriginalFile},
	{CatalogID: "jsonl", Capability: CapabilityDetect, Evidence: documentDetectionEvidence, ImplementationID: documentDetectionID, InputKind: InputOriginalFile},
	{CatalogID: "latex", Capability: CapabilityDetect, Evidence: documentDetectionEvidence, ImplementationID: documentDetectionID, InputKind: InputOriginalFile},
	{CatalogID: "mp3", Capability: CapabilityMetadata, Evidence: "TestExtractID3TextEncodingsAndFrameBoundary", ImplementationID: sourceMetadataExtractorID, InputKind: InputOriginalFile},
	{CatalogID: "mp4", Capability: CapabilityDetect, Evidence: "TestDetectBytesRecognizesSupportedContainers", ImplementationID: mediaDetectionID, InputKind: InputOriginalFile},
	{CatalogID: "mp4", Capability: CapabilityMetadata, Evidence: "TestExtractSourceMetadataReadsMP4CreationTime", ImplementationID: sourceMetadataExtractorID, InputKind: InputOriginalFile},
	{CatalogID: "pdf", Capability: CapabilityDetect, Evidence: documentDetectionEvidence, ImplementationID: documentDetectionID, InputKind: InputOriginalFile},
	{CatalogID: "pdf", Capability: CapabilityMetadata, Evidence: "TestExtractSourceMetadataUsesAuthoritativePDFInfo", ImplementationID: sourceMetadataExtractorID, InputKind: InputOriginalFile},
	{CatalogID: "pdf", Capability: CapabilityText, Evidence: "TestSyntheticMarkdownPDFOutput", DescriptorFingerprint: "129bea875ffc3ab1511ef447c82379298c1d358923bec6af91433786148cbc95", InputKind: InputOriginalFile},
	{CatalogID: "png", Capability: CapabilityDetect, Evidence: "TestDetectBytesRecognizesSupportedContainers", ImplementationID: mediaDetectionID, InputKind: InputOriginalFile},
	{CatalogID: "png", Capability: CapabilityMetadata, Evidence: "TestExtractSourceMetadataReadsVisualContainerFacts", ImplementationID: sourceMetadataExtractorID, InputKind: InputOriginalFile},
	{CatalogID: "pptx", Capability: CapabilityMetadata, Evidence: "TestExtractSourceMetadataFromSyntheticPPTX", ImplementationID: sourceMetadataExtractorID, InputKind: InputOriginalFile},
	{CatalogID: "tiff", Capability: CapabilityMetadata, Evidence: "TestExtractSourceMetadataReadsTIFFPhotoFacts", ImplementationID: sourceMetadataExtractorID, InputKind: InputOriginalFile},
	{CatalogID: "webp", Capability: CapabilityDetect, Evidence: "TestDetectBytesRecognizesSupportedContainers", ImplementationID: mediaDetectionID, InputKind: InputOriginalFile},
	{CatalogID: "webp", Capability: CapabilityMetadata, Evidence: "TestExtractSourceMetadataReadsVisualContainerFacts", ImplementationID: sourceMetadataExtractorID, InputKind: InputOriginalFile},
	{CatalogID: "xml", Capability: CapabilityDetect, Evidence: documentDetectionEvidence, ImplementationID: documentDetectionID, InputKind: InputOriginalFile},
	{CatalogID: "xlsx", Capability: CapabilityMetadata, Evidence: "TestExtractSourceMetadataFromSyntheticXLSX", ImplementationID: sourceMetadataExtractorID, InputKind: InputOriginalFile},
	{CatalogID: "yaml", Capability: CapabilityDetect, Evidence: documentDetectionEvidence, ImplementationID: documentDetectionID, InputKind: InputOriginalFile},
}

var retainedCatalogIDs = []string{
	"7z", "calendar", "csv", "doc", "docx", "dwf", "dwg", "dxf", "eml", "epub", "flac", "gif", "go",
	"gzip", "html", "javascript", "jpeg", "json", "jsonl", "jsonl-alias", "latex", "m4a", "markdown", "mov",
	"mp3", "mp4", "msg", "numbers", "ods", "odt", "ogg", "pdf", "png", "ppt", "pptx", "python", "rar",
	"rst", "rtf", "svg", "tar", "tiff", "txt", "wav", "webm", "webp", "xhtml", "xls", "xlsx", "xml",
	"yaml", "zip",
}

var qualifications = makeQualifications()

func makeQualifications() []Qualification {
	result := slices.Clone(baseQualifications)
	for _, catalogID := range retainedCatalogIDs {
		result = append(result, Qualification{
			CatalogID: catalogID, Capability: CapabilityRetain,
			Evidence:         "TestPrepareUploadRetainsOriginalBytesForEveryCatalogFormat",
			ImplementationID: originalRetentionID, InputKind: InputOriginalFile,
		})
	}
	slices.SortFunc(result, compareQualifications)
	return result
}

func compareQualifications(left, right Qualification) int {
	for _, pair := range [][2]string{
		{left.CatalogID, right.CatalogID}, {string(left.Capability), string(right.Capability)},
		{left.Evidence, right.Evidence}, {left.ImplementationID, right.ImplementationID},
		{left.DescriptorFingerprint, right.DescriptorFingerprint}, {string(left.InputKind), string(right.InputKind)},
	} {
		if order := strings.Compare(pair[0], pair[1]); order != 0 {
			return order
		}
	}
	return 0
}

// All returns a defensive copy of every qualification in stable key order.
func All() []Qualification { return slices.Clone(qualifications) }

// Lookup returns the qualification only when every typed identity field
// matches an immutable manifest entry.
func Lookup(query Query) (Qualification, bool) {
	want := Qualification(query)
	for _, qualification := range qualifications {
		if qualification == want {
			return qualification, true
		}
	}
	return Qualification{}, false
}

// LookupEvidence returns all immutable qualifications behind one exact wire
// evidence claim. Runtime composition must still prove one full Query match.
func LookupEvidence(query EvidenceQuery) []Qualification {
	var matches []Qualification
	for _, qualification := range qualifications {
		if qualification.CatalogID == query.CatalogID &&
			qualification.Capability == query.Capability &&
			qualification.Evidence == query.Evidence {
			matches = append(matches, qualification)
		}
	}
	return matches
}
