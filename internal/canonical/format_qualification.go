package canonical

import (
	"slices"
	"strings"
)

// FormatCapability is the qualification manifest's dependency-neutral copy of the
// public capability vocabulary. Consumers must convert their typed capability
// at the package boundary.
type FormatCapability string

const (
	FormatCapabilityDetect     FormatCapability = "detect"
	FormatCapabilityRetain     FormatCapability = "retain"
	FormatCapabilityMetadata   FormatCapability = "metadata"
	FormatCapabilityExpand     FormatCapability = "expand"
	FormatCapabilityText       FormatCapability = "text"
	FormatCapabilityPages      FormatCapability = "pages"
	FormatCapabilityTranscript FormatCapability = "transcript"
)

// FormatInputKind identifies the bytes exercised by a qualification fixture.
type FormatInputKind string

const FormatInputOriginalFile FormatInputKind = "original_file"

// FormatQualification binds one real test to one catalog capability and one pinned
// implementation or provider descriptor identity. Exactly matching this tuple
// is the only way runtime composition can consume a qualification.
type FormatQualification struct {
	CatalogID             string
	Capability            FormatCapability
	Evidence              string
	ImplementationID      string
	DescriptorFingerprint string
	InputKind             FormatInputKind
}

// FormatQualificationQuery is the exact typed lookup key for a qualification.
type FormatQualificationQuery FormatQualification

// FormatQualificationEvidenceQuery is the narrower key available to the wire-contract validator,
// which cannot observe every local implementation identity. Runtime coverage
// composition must use FormatQualificationQuery and LookupFormatQualification instead.
type FormatQualificationEvidenceQuery struct {
	CatalogID  string
	Capability FormatCapability
	Evidence   string
}

const (
	documentDetectionEvidence = "TestDetectFormatRecognizesBoundedDocumentFamilies"
	documentDetectionID       = "docbank-document-format-detection:pdf+ooxml+epub+compound+rtf+utf8-structured-text:v1"
	mediaDetectionID          = "docbank-media-detection:jpeg+png+webp+gif+iso-bmff:v1"
	originalRetentionID       = "docbank-original-retention:verified-blob+ingest-authority:v1"
	// Independently pin the parser implementation, excluding its Go toolchain.
	sourceMetadataExtractorID = "764170f0fbd26cdfb6c2d6672a4561c82f44755115e8e0b25b4f5cfbe4de09ab"
)

var baseQualifications = []FormatQualification{
	{CatalogID: "calendar", Capability: FormatCapabilityMetadata, Evidence: "TestExtractSourceMetadataFromSyntheticFormats", ImplementationID: sourceMetadataExtractorID, InputKind: FormatInputOriginalFile},
	{CatalogID: "csv", Capability: FormatCapabilityDetect, Evidence: documentDetectionEvidence, ImplementationID: documentDetectionID, InputKind: FormatInputOriginalFile},
	{CatalogID: "doc", Capability: FormatCapabilityDetect, Evidence: documentDetectionEvidence, ImplementationID: documentDetectionID, InputKind: FormatInputOriginalFile},
	{CatalogID: "docx", Capability: FormatCapabilityDetect, Evidence: documentDetectionEvidence, ImplementationID: documentDetectionID, InputKind: FormatInputOriginalFile},
	{CatalogID: "docx", Capability: FormatCapabilityMetadata, Evidence: "TestExtractSourceMetadataFromSyntheticDOCX", ImplementationID: sourceMetadataExtractorID, InputKind: FormatInputOriginalFile},
	{CatalogID: "eml", Capability: FormatCapabilityDetect, Evidence: documentDetectionEvidence, ImplementationID: documentDetectionID, InputKind: FormatInputOriginalFile},
	{CatalogID: "eml", Capability: FormatCapabilityMetadata, Evidence: "TestExtractSourceMetadataFromSyntheticFormats", ImplementationID: sourceMetadataExtractorID, InputKind: FormatInputOriginalFile},
	{CatalogID: "epub", Capability: FormatCapabilityDetect, Evidence: documentDetectionEvidence, ImplementationID: documentDetectionID, InputKind: FormatInputOriginalFile},
	{CatalogID: "gif", Capability: FormatCapabilityDetect, Evidence: "TestDetectBytesRecognizesSupportedContainers", ImplementationID: mediaDetectionID, InputKind: FormatInputOriginalFile},
	{CatalogID: "gif", Capability: FormatCapabilityMetadata, Evidence: "TestExtractSourceMetadataReadsVisualContainerFacts", ImplementationID: sourceMetadataExtractorID, InputKind: FormatInputOriginalFile},
	{CatalogID: "jpeg", Capability: FormatCapabilityDetect, Evidence: "TestDetectBytesRecognizesSupportedContainers", ImplementationID: mediaDetectionID, InputKind: FormatInputOriginalFile},
	{CatalogID: "jpeg", Capability: FormatCapabilityMetadata, Evidence: "TestExtractSourceMetadataFromSyntheticFormats", ImplementationID: sourceMetadataExtractorID, InputKind: FormatInputOriginalFile},
	{CatalogID: "json", Capability: FormatCapabilityDetect, Evidence: documentDetectionEvidence, ImplementationID: documentDetectionID, InputKind: FormatInputOriginalFile},
	{CatalogID: "jsonl", Capability: FormatCapabilityDetect, Evidence: documentDetectionEvidence, ImplementationID: documentDetectionID, InputKind: FormatInputOriginalFile},
	{CatalogID: "latex", Capability: FormatCapabilityDetect, Evidence: documentDetectionEvidence, ImplementationID: documentDetectionID, InputKind: FormatInputOriginalFile},
	{CatalogID: "mp3", Capability: FormatCapabilityMetadata, Evidence: "TestExtractID3TextEncodingsAndFrameBoundary", ImplementationID: sourceMetadataExtractorID, InputKind: FormatInputOriginalFile},
	{CatalogID: "mp4", Capability: FormatCapabilityDetect, Evidence: "TestDetectBytesRecognizesSupportedContainers", ImplementationID: mediaDetectionID, InputKind: FormatInputOriginalFile},
	{CatalogID: "mp4", Capability: FormatCapabilityMetadata, Evidence: "TestExtractSourceMetadataReadsMP4CreationTime", ImplementationID: sourceMetadataExtractorID, InputKind: FormatInputOriginalFile},
	{CatalogID: "pdf", Capability: FormatCapabilityDetect, Evidence: documentDetectionEvidence, ImplementationID: documentDetectionID, InputKind: FormatInputOriginalFile},
	{CatalogID: "pdf", Capability: FormatCapabilityMetadata, Evidence: "TestExtractSourceMetadataUsesAuthoritativePDFInfo", ImplementationID: sourceMetadataExtractorID, InputKind: FormatInputOriginalFile},
	{CatalogID: "pdf", Capability: FormatCapabilityText, Evidence: "TestPDFTextCorpus", DescriptorFingerprint: "915bf5d1a85f1c2ae395f23827312fbcd54c533fbe09d95cfad58e2f456697c3", InputKind: FormatInputOriginalFile},
	{CatalogID: "pdf", Capability: FormatCapabilityText, Evidence: "TestSyntheticMarkdownPDFOutput", DescriptorFingerprint: "129bea875ffc3ab1511ef447c82379298c1d358923bec6af91433786148cbc95", InputKind: FormatInputOriginalFile},
	{CatalogID: "png", Capability: FormatCapabilityDetect, Evidence: "TestDetectBytesRecognizesSupportedContainers", ImplementationID: mediaDetectionID, InputKind: FormatInputOriginalFile},
	{CatalogID: "png", Capability: FormatCapabilityMetadata, Evidence: "TestExtractSourceMetadataReadsVisualContainerFacts", ImplementationID: sourceMetadataExtractorID, InputKind: FormatInputOriginalFile},
	{CatalogID: "pptx", Capability: FormatCapabilityMetadata, Evidence: "TestExtractSourceMetadataFromSyntheticPPTX", ImplementationID: sourceMetadataExtractorID, InputKind: FormatInputOriginalFile},
	{CatalogID: "tiff", Capability: FormatCapabilityMetadata, Evidence: "TestExtractSourceMetadataReadsTIFFPhotoFacts", ImplementationID: sourceMetadataExtractorID, InputKind: FormatInputOriginalFile},
	{CatalogID: "webp", Capability: FormatCapabilityDetect, Evidence: "TestDetectBytesRecognizesSupportedContainers", ImplementationID: mediaDetectionID, InputKind: FormatInputOriginalFile},
	{CatalogID: "webp", Capability: FormatCapabilityMetadata, Evidence: "TestExtractSourceMetadataReadsVisualContainerFacts", ImplementationID: sourceMetadataExtractorID, InputKind: FormatInputOriginalFile},
	{CatalogID: "xml", Capability: FormatCapabilityDetect, Evidence: documentDetectionEvidence, ImplementationID: documentDetectionID, InputKind: FormatInputOriginalFile},
	{CatalogID: "xlsx", Capability: FormatCapabilityMetadata, Evidence: "TestExtractSourceMetadataFromSyntheticXLSX", ImplementationID: sourceMetadataExtractorID, InputKind: FormatInputOriginalFile},
	{CatalogID: "yaml", Capability: FormatCapabilityDetect, Evidence: documentDetectionEvidence, ImplementationID: documentDetectionID, InputKind: FormatInputOriginalFile},
}

var retainedCatalogIDs = []string{
	"7z", "calendar", "csv", "doc", "docx", "dwf", "dwg", "dxf", "eml", "epub", "flac", "gif", "go",
	"gzip", "html", "javascript", "jpeg", "json", "jsonl", "jsonl-alias", "latex", "m4a", "markdown", "mov",
	"mp3", "mp4", "msg", "numbers", "ods", "odt", "ogg", "pdf", "png", "ppt", "pptx", "python", "rar",
	"rst", "rtf", "svg", "tar", "tiff", "txt", "wav", "webm", "webp", "xhtml", "xls", "xlsx", "xml",
	"yaml", "zip",
}

var qualifications = makeQualifications()

func makeQualifications() []FormatQualification {
	result := slices.Clone(baseQualifications)
	for _, catalogID := range retainedCatalogIDs {
		result = append(result, FormatQualification{
			CatalogID: catalogID, Capability: FormatCapabilityRetain,
			Evidence:         "TestPrepareUploadRetainsOriginalBytesForEveryCatalogFormat",
			ImplementationID: originalRetentionID, InputKind: FormatInputOriginalFile,
		})
	}
	slices.SortFunc(result, compareQualifications)
	return result
}

func compareQualifications(left, right FormatQualification) int {
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

// AllFormatQualifications returns a defensive copy of every qualification in stable key order.
func AllFormatQualifications() []FormatQualification { return slices.Clone(qualifications) }

// LookupFormatQualification returns the qualification only when every typed identity field
// matches an immutable manifest entry.
func LookupFormatQualification(query FormatQualificationQuery) (FormatQualification, bool) {
	want := FormatQualification(query)
	for _, qualification := range qualifications {
		if qualification == want {
			return qualification, true
		}
	}
	return FormatQualification{}, false
}

// LookupFormatQualificationEvidence returns all immutable qualifications behind one exact wire
// evidence claim. Runtime composition must still prove one full FormatQualificationQuery match.
func LookupFormatQualificationEvidence(query FormatQualificationEvidenceQuery) []FormatQualification {
	var matches []FormatQualification
	for _, qualification := range qualifications {
		if qualification.CatalogID == query.CatalogID &&
			qualification.Capability == query.Capability &&
			qualification.Evidence == query.Evidence {
			matches = append(matches, qualification)
		}
	}
	return matches
}
