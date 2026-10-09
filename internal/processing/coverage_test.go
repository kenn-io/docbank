package processing

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/coverage"
	"go.kenn.io/docbank/internal/emailmime"
	internalformatcoverage "go.kenn.io/docbank/internal/formatcoverage"
)

var updateFormatCoverage = flag.Bool("update", false, "update the pinned format coverage fixture")

func TestMetadataQualificationSurvivesToolchainChanges(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		version     string
		fingerprint string
	}{
		{"go1.27.0", "2a9cc064fd6fa4ced7e4b643eb88de7568c1e2143bc972159d822058d6942e88"},
		{"go1.27.1", "ec4e29e8e006c593bc872056e209c2b90c5b15d8ffa7a5ec7e3a289fe766f349"},
	} {
		t.Run(test.version, func(t *testing.T) {
			t.Parallel()
			recipe := emailmime.Recipe()
			recipe.GoVersion = test.version
			assert.Equal(t, test.fingerprint,
				fingerprintSourceMetadataExtractor(sourceMetadataExtractorDescriptor, recipe),
				"stored metadata generations must retain their existing identity")
			id := sourceMetadataImplementationID(sourceMetadataExtractorDescriptor, recipe)
			record, err := internalformatcoverage.Compute(nil, id)
			require.NoError(t, err)
			assert.Equal(t, SourceMetadataImplementationID, record.GeneratedBy.ExtractorID)
			assert.Equal(t, document.CapabilityQualified,
				processingFormatByID(t, record, "xlsx").Capabilities[document.CapabilityMetadata].State)
		})
	}
}

func TestMetadataQualificationRejectsParserChanges(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"local parsers", "email decoder"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			descriptor, recipe := sourceMetadataExtractorDescriptor, emailmime.Recipe()
			if change == "local parsers" {
				descriptor += "-changed"
			} else {
				recipe.ImplementationRevision++
			}
			id := sourceMetadataImplementationID(descriptor, recipe)
			record, err := internalformatcoverage.Compute(nil, id)
			require.NoError(t, err)
			assert.Equal(t, document.CapabilityUnqualified,
				processingFormatByID(t, record, "xlsx").Capabilities[document.CapabilityMetadata].State)
		})
	}
}

func TestFormatCoverageMatchesPinnedFixture(t *testing.T) {
	t.Parallel()
	record, err := internalformatcoverage.Compute(nil, SourceMetadataImplementationID)
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
	t.Parallel()
	record, err := internalformatcoverage.Compute(nil, SourceMetadataImplementationID)
	require.NoError(t, err)
	assert.Len(t, record.Formats, 52)
	assert.Equal(t, SourceMetadataImplementationID, record.GeneratedBy.ExtractorID)

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
	t.Parallel()
	descriptor := syntheticMarkdownCoverageDescriptor(t)
	record, err := internalformatcoverage.Compute([]document.RenditionDescriptor{descriptor}, SourceMetadataImplementationID)
	require.NoError(t, err)
	text := processingFormatByID(t, record, "pdf").Capabilities[document.CapabilityText]
	assert.Equal(t, document.CapabilityQualified, text.State)
	assert.Equal(t, descriptor.ID, text.Provider)
	assert.Equal(t, descriptor.Fingerprint, text.ProviderFingerprint)
	assert.Equal(t, "TestSyntheticMarkdownPDFOutput", text.Evidence)
}

func TestFormatCoverageRequiresTheActiveMetadataExtractorQualification(t *testing.T) {
	t.Parallel()
	record, err := internalformatcoverage.Compute(nil, "synthetic-extractor/v2")
	require.NoError(t, err)
	assert.Equal(t, "synthetic-extractor/v2", record.GeneratedBy.ExtractorID)
	pdf := processingFormatByID(t, record, "pdf")
	assert.Equal(t, document.CapabilityUnqualified, pdf.Capabilities[document.CapabilityMetadata].State)
	assert.Equal(t, document.CapabilityQualified, pdf.Capabilities[document.CapabilityDetect].State)
}

func TestLookupFormatDistinguishesCatalogPendingAndUnknown(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		query string
		match document.FormatLookupMatch
	}{
		{query: ".PDF", match: document.FormatLookupFormat},
		{query: "wpd", match: document.FormatLookupPending},
		{query: "qqq", match: document.FormatLookupUnknown},
	} {
		record, err := internalformatcoverage.Compute(nil, SourceMetadataImplementationID)
		require.NoError(t, err)
		lookup := coverage.Lookup(record, testCase.query)
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
