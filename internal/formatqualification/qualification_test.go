package formatqualification

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testExtractorFingerprint = "42b01ef9219b3b35dedf49ef98d311b21772da27631c3ca90597f28363de1ec5"

func TestLookupRequiresTheExactQualifiedTuple(t *testing.T) {
	query := Query{
		CatalogID:        "pdf",
		Capability:       CapabilityMetadata,
		Evidence:         "TestExtractSourceMetadataUsesAuthoritativePDFInfo",
		ImplementationID: testExtractorFingerprint,
		InputKind:        InputOriginalFile,
	}
	qualification, found := Lookup(query)
	require.True(t, found)
	assert.Equal(t, Qualification(query), qualification)

	for _, mutate := range []func(*Query){
		func(value *Query) { value.CatalogID = "docx" },
		func(value *Query) { value.Capability = CapabilityDetect },
		func(value *Query) { value.Evidence = "TestNameIsNotProof" },
		func(value *Query) { value.ImplementationID = "" },
		func(value *Query) { value.InputKind = "" },
	} {
		forged := query
		mutate(&forged)
		_, found := Lookup(forged)
		assert.False(t, found, "%+v", forged)
	}
}

func TestQualificationsAreImmutableAndFixtureBacked(t *testing.T) {
	qualifications := All()
	require.NotEmpty(t, qualifications)
	expected := []Qualification{
		{CatalogID: "calendar", Capability: CapabilityMetadata, Evidence: "TestExtractSourceMetadataFromSyntheticFormats", ImplementationID: testExtractorFingerprint, InputKind: InputOriginalFile},
		{CatalogID: "csv", Capability: CapabilityDetect, Evidence: "TestDetectFormatRecognizesBoundedDocumentFamilies", ImplementationID: "docbank-document-format-detection:pdf+ooxml+epub+compound+rtf+utf8-structured-text:v1", InputKind: InputOriginalFile},
		{CatalogID: "doc", Capability: CapabilityDetect, Evidence: "TestDetectFormatRecognizesBoundedDocumentFamilies", ImplementationID: "docbank-document-format-detection:pdf+ooxml+epub+compound+rtf+utf8-structured-text:v1", InputKind: InputOriginalFile},
		{CatalogID: "docx", Capability: CapabilityDetect, Evidence: "TestDetectFormatRecognizesBoundedDocumentFamilies", ImplementationID: "docbank-document-format-detection:pdf+ooxml+epub+compound+rtf+utf8-structured-text:v1", InputKind: InputOriginalFile},
		{CatalogID: "docx", Capability: CapabilityMetadata, Evidence: "TestExtractSourceMetadataFromSyntheticDOCX", ImplementationID: testExtractorFingerprint, InputKind: InputOriginalFile},
		{CatalogID: "eml", Capability: CapabilityDetect, Evidence: "TestDetectFormatRecognizesBoundedDocumentFamilies", ImplementationID: "docbank-document-format-detection:pdf+ooxml+epub+compound+rtf+utf8-structured-text:v1", InputKind: InputOriginalFile},
		{CatalogID: "eml", Capability: CapabilityMetadata, Evidence: "TestExtractSourceMetadataFromSyntheticFormats", ImplementationID: testExtractorFingerprint, InputKind: InputOriginalFile},
		{CatalogID: "epub", Capability: CapabilityDetect, Evidence: "TestDetectFormatRecognizesBoundedDocumentFamilies", ImplementationID: "docbank-document-format-detection:pdf+ooxml+epub+compound+rtf+utf8-structured-text:v1", InputKind: InputOriginalFile},
		{CatalogID: "gif", Capability: CapabilityDetect, Evidence: "TestDetectBytesRecognizesSupportedContainers", ImplementationID: "docbank-media-detection:jpeg+png+webp+gif+iso-bmff:v1", InputKind: InputOriginalFile},
		{CatalogID: "gif", Capability: CapabilityMetadata, Evidence: "TestExtractSourceMetadataReadsVisualContainerFacts", ImplementationID: testExtractorFingerprint, InputKind: InputOriginalFile},
		{CatalogID: "jpeg", Capability: CapabilityDetect, Evidence: "TestDetectBytesRecognizesSupportedContainers", ImplementationID: "docbank-media-detection:jpeg+png+webp+gif+iso-bmff:v1", InputKind: InputOriginalFile},
		{CatalogID: "jpeg", Capability: CapabilityMetadata, Evidence: "TestExtractSourceMetadataFromSyntheticFormats", ImplementationID: testExtractorFingerprint, InputKind: InputOriginalFile},
		{CatalogID: "json", Capability: CapabilityDetect, Evidence: "TestDetectFormatRecognizesBoundedDocumentFamilies", ImplementationID: "docbank-document-format-detection:pdf+ooxml+epub+compound+rtf+utf8-structured-text:v1", InputKind: InputOriginalFile},
		{CatalogID: "jsonl", Capability: CapabilityDetect, Evidence: "TestDetectFormatRecognizesBoundedDocumentFamilies", ImplementationID: "docbank-document-format-detection:pdf+ooxml+epub+compound+rtf+utf8-structured-text:v1", InputKind: InputOriginalFile},
		{CatalogID: "latex", Capability: CapabilityDetect, Evidence: "TestDetectFormatRecognizesBoundedDocumentFamilies", ImplementationID: "docbank-document-format-detection:pdf+ooxml+epub+compound+rtf+utf8-structured-text:v1", InputKind: InputOriginalFile},
		{CatalogID: "mp3", Capability: CapabilityMetadata, Evidence: "TestExtractID3TextEncodingsAndFrameBoundary", ImplementationID: testExtractorFingerprint, InputKind: InputOriginalFile},
		{CatalogID: "mp4", Capability: CapabilityDetect, Evidence: "TestDetectBytesRecognizesSupportedContainers", ImplementationID: "docbank-media-detection:jpeg+png+webp+gif+iso-bmff:v1", InputKind: InputOriginalFile},
		{CatalogID: "mp4", Capability: CapabilityMetadata, Evidence: "TestExtractSourceMetadataReadsMP4CreationTime", ImplementationID: testExtractorFingerprint, InputKind: InputOriginalFile},
		{CatalogID: "pdf", Capability: CapabilityDetect, Evidence: "TestDetectFormatRecognizesBoundedDocumentFamilies", ImplementationID: "docbank-document-format-detection:pdf+ooxml+epub+compound+rtf+utf8-structured-text:v1", InputKind: InputOriginalFile},
		{CatalogID: "pdf", Capability: CapabilityMetadata, Evidence: "TestExtractSourceMetadataUsesAuthoritativePDFInfo", ImplementationID: testExtractorFingerprint, InputKind: InputOriginalFile},
		{CatalogID: "pdf", Capability: CapabilityText, Evidence: "TestSyntheticMarkdownPDFOutput", DescriptorFingerprint: "129bea875ffc3ab1511ef447c82379298c1d358923bec6af91433786148cbc95", InputKind: InputOriginalFile},
		{CatalogID: "png", Capability: CapabilityDetect, Evidence: "TestDetectBytesRecognizesSupportedContainers", ImplementationID: "docbank-media-detection:jpeg+png+webp+gif+iso-bmff:v1", InputKind: InputOriginalFile},
		{CatalogID: "png", Capability: CapabilityMetadata, Evidence: "TestExtractSourceMetadataReadsVisualContainerFacts", ImplementationID: testExtractorFingerprint, InputKind: InputOriginalFile},
		{CatalogID: "pptx", Capability: CapabilityMetadata, Evidence: "TestExtractSourceMetadataFromSyntheticPPTX", ImplementationID: testExtractorFingerprint, InputKind: InputOriginalFile},
		{CatalogID: "tiff", Capability: CapabilityMetadata, Evidence: "TestExtractSourceMetadataReadsTIFFPhotoFacts", ImplementationID: testExtractorFingerprint, InputKind: InputOriginalFile},
		{CatalogID: "webp", Capability: CapabilityDetect, Evidence: "TestDetectBytesRecognizesSupportedContainers", ImplementationID: "docbank-media-detection:jpeg+png+webp+gif+iso-bmff:v1", InputKind: InputOriginalFile},
		{CatalogID: "webp", Capability: CapabilityMetadata, Evidence: "TestExtractSourceMetadataReadsVisualContainerFacts", ImplementationID: testExtractorFingerprint, InputKind: InputOriginalFile},
		{CatalogID: "xml", Capability: CapabilityDetect, Evidence: "TestDetectFormatRecognizesBoundedDocumentFamilies", ImplementationID: "docbank-document-format-detection:pdf+ooxml+epub+compound+rtf+utf8-structured-text:v1", InputKind: InputOriginalFile},
		{CatalogID: "xlsx", Capability: CapabilityMetadata, Evidence: "TestExtractSourceMetadataFromSyntheticXLSX", ImplementationID: testExtractorFingerprint, InputKind: InputOriginalFile},
		{CatalogID: "yaml", Capability: CapabilityDetect, Evidence: "TestDetectFormatRecognizesBoundedDocumentFamilies", ImplementationID: "docbank-document-format-detection:pdf+ooxml+epub+compound+rtf+utf8-structured-text:v1", InputKind: InputOriginalFile},
	}
	for _, catalogID := range []string{
		"7z", "calendar", "csv", "doc", "docx", "dwf", "dwg", "dxf", "eml", "epub", "flac", "gif", "go",
		"gzip", "html", "javascript", "jpeg", "json", "jsonl", "jsonl-alias", "latex", "m4a", "markdown", "mov",
		"mp3", "mp4", "msg", "numbers", "ods", "odt", "ogg", "pdf", "png", "ppt", "pptx", "python", "rar",
		"rst", "rtf", "svg", "tar", "tiff", "txt", "wav", "webm", "webp", "xhtml", "xls", "xlsx", "xml",
		"yaml", "zip",
	} {
		expected = append(expected, Qualification{
			CatalogID: catalogID, Capability: CapabilityRetain,
			Evidence:         "TestPrepareUploadRetainsOriginalBytesForEveryCatalogFormat",
			ImplementationID: "docbank-original-retention:verified-blob+ingest-authority:v1",
			InputKind:        InputOriginalFile,
		})
	}
	slices.SortFunc(expected, func(left, right Qualification) int {
		if order := strings.Compare(left.CatalogID, right.CatalogID); order != 0 {
			return order
		}
		return strings.Compare(string(left.Capability), string(right.Capability))
	})
	assert.Equal(t, expected, qualifications)

	qualifications[0].Evidence = "forged"
	assert.NotEqual(t, "forged", All()[0].Evidence)
}
