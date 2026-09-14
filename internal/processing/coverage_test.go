package processing

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

var updateFormatCoverage = flag.Bool("update", false, "update the pinned format coverage fixture")

func TestFormatCoverageMatchesPinnedFixture(t *testing.T) {
	record, err := FormatCoverage(nil)
	require.NoError(t, err)
	encoded, _, err := document.MarshalFormatCoverageV1(record)
	require.NoError(t, err)
	fixture := filepath.Join("..", "..", "document", "testdata", "format_coverage.json")
	if *updateFormatCoverage {
		require.NoError(t, os.WriteFile(fixture, encoded, 0o644))
	}
	expected, err := os.ReadFile(fixture)
	require.NoError(t, err)
	assert.Equal(t, string(expected), string(encoded))
}

func TestFormatCoverageComposesQualifiedOwnersWithoutInflatingClaims(t *testing.T) {
	record, err := FormatCoverage(nil)
	require.NoError(t, err)
	assert.Len(t, record.Formats, 52)
	assert.Equal(t, SourceMetadataExtractorFingerprint, record.GeneratedBy.ExtractorFingerprint)
	assert.Empty(t, record.GeneratedBy.DecoderFormats)

	qualifiedDetect := map[string]bool{
		"csv": true, "doc": true, "docx": true, "eml": true, "epub": true,
		"gif": true, "jpeg": true, "json": true, "jsonl": true,
		"latex": true, "mp4": true, "pdf": true, "png": true, "webp": true,
		"xml": true, "yaml": true,
	}
	qualifiedMetadata := map[string]bool{
		"calendar": true, "docx": true, "eml": true, "gif": true, "jpeg": true,
		"mp3": true, "mp4": true, "pdf": true, "png": true, "pptx": true,
		"tiff": true, "webp": true, "xlsx": true,
	}
	for _, format := range record.Formats {
		require.Len(t, format.Capabilities, 7, format.ID)
		assert.Equal(t, document.CapabilityQualified, format.Capabilities[document.CapabilityRetain].State, format.ID)
		assert.Equal(t, qualifiedDetect[format.ID],
			format.Capabilities[document.CapabilityDetect].State == document.CapabilityQualified, format.ID)
		assert.Equal(t, qualifiedMetadata[format.ID],
			format.Capabilities[document.CapabilityMetadata].State == document.CapabilityQualified, format.ID)
		assert.NotEqual(t, document.CapabilityQualified, format.Capabilities[document.CapabilityPages].State, format.ID)
		assert.NotEqual(t, document.CapabilityQualified, format.Capabilities[document.CapabilityExpand].State, format.ID)
	}
	assert.Equal(t, document.CapabilityUnqualified,
		processingFormatByID(t, record, "txt").Capabilities[document.CapabilityDetect].State,
		"ambiguous plain text remains a classification hint")
	assert.Equal(t, document.CapabilityUnqualified,
		processingFormatByID(t, record, "go").Capabilities[document.CapabilityDetect].State,
		"MIME-selected source code remains unqualified")
}

func TestFormatCoverageUsesExecutedSyntheticProviderQualification(t *testing.T) {
	descriptor := syntheticMarkdownCoverageDescriptor(t)
	record, err := FormatCoverage([]document.RenditionDescriptor{descriptor})
	require.NoError(t, err)
	text := processingFormatByID(t, record, "pdf").Capabilities[document.CapabilityText]
	assert.Equal(t, document.CapabilityQualified, text.State)
	assert.Equal(t, descriptor.ID, text.Provider)
	assert.Equal(t, descriptor.Fingerprint, text.ProviderFingerprint)
	assert.Equal(t, "TestSyntheticMarkdownPDFOutput", text.Evidence)
}

func TestLookupFormatDistinguishesCatalogPendingAndUnknown(t *testing.T) {
	for _, testCase := range []struct {
		query string
		match document.FormatLookupMatch
	}{
		{query: ".PDF", match: document.FormatLookupFormat},
		{query: "wpd", match: document.FormatLookupPending},
		{query: "qqq", match: document.FormatLookupUnknown},
	} {
		lookup, err := LookupFormat(nil, testCase.query)
		require.NoError(t, err)
		assert.Equal(t, testCase.match, lookup.Match)
		assert.Equal(t, testCase.query, lookup.Query)
	}
}

func processingFormatByID(
	t *testing.T, record document.FormatCoverageV1, id string,
) document.FormatCapabilityV1 {
	t.Helper()
	for _, format := range record.Formats {
		if format.ID == id {
			return format
		}
	}
	require.FailNow(t, "format absent from coverage", id)
	return document.FormatCapabilityV1{}
}
