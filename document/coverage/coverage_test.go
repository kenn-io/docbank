package coverage

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestComputeRecognizesMarkdownWithoutAnArtifactRole(t *testing.T) {
	descriptor := syntheticMarkdownDescriptor(t)
	descriptor.ArtifactRoles = nil
	descriptor.Fingerprint = ""
	descriptor, err := document.NewRenditionDescriptor(descriptor)
	require.NoError(t, err)
	sources := DefaultSources()
	sources.ExtractorID = "synthetic-extractor/v1"
	sources.BoundProviders = []document.RenditionDescriptor{descriptor}
	record, err := Compute(sources)
	require.NoError(t, err)
	state := formatByID(t, record, "pdf").Capabilities[document.CapabilityText]
	assert.Equal(t, document.CapabilityUnqualified, state.State)
	assert.Equal(t, descriptor.Fingerprint, state.ProviderFingerprint)
}

func TestComputeRequiresRuntimeIdentities(t *testing.T) {
	_, err := Compute(DefaultSources())
	require.ErrorContains(t, err, "extractor identity")
}

func TestDefaultSourcesEnumeratesOnlyFixtureQualifiedDetection(t *testing.T) {
	sources := DefaultSources()
	ids := make([]string, 0, len(sources.Detect))
	for _, capability := range sources.Detect {
		ids = append(ids, capability.CatalogID)
	}
	assert.ElementsMatch(t, []string{
		"csv", "doc", "docx", "eml", "epub", "gif", "jpeg", "json", "jsonl",
		"latex", "mp4", "pdf", "png", "webp", "xml", "yaml",
	}, ids)
	assert.NotContains(t, ids, "go", "MIME-selected source code has no independent byte recognition")
	assert.NotContains(t, ids, "txt", "ambiguous plain text has no independent byte signature")
}

func TestComputeJoinsOnlyQualifiedSameDescriptorRoles(t *testing.T) {
	provider := syntheticMarkdownDescriptor(t)
	sources := DefaultSources()
	sources.ExtractorID = "synthetic-extractor/v1"
	sources.BoundProviders = []document.RenditionDescriptor{provider}

	record, err := Compute(sources)
	require.NoError(t, err)
	pdf := formatByID(t, record, "pdf")
	assert.Equal(t, document.CapabilityQualified, pdf.Capabilities[document.CapabilityText].State)
	assert.Equal(t, provider.ID, pdf.Capabilities[document.CapabilityText].Provider)
	assert.NotEqual(t, document.CapabilityQualified, pdf.Capabilities[document.CapabilityPages].State,
		"text evidence does not qualify pages")
	assert.Equal(t, document.CapabilityUnsupported,
		formatByID(t, record, "dwg").Capabilities[document.CapabilityText].State)

	unbound := sources
	unbound.BoundProviders = nil
	withoutProviders, err := Compute(unbound)
	require.NoError(t, err)
	assert.Equal(t, document.CapabilityUnsupported,
		formatByID(t, withoutProviders, "pdf").Capabilities[document.CapabilityText].State)

	wrongRole := syntheticImageDescriptor(t)
	sources.BoundProviders = []document.RenditionDescriptor{wrongRole}
	roleMismatch, err := Compute(sources)
	require.NoError(t, err)
	assert.Equal(t, document.CapabilityUnsupported,
		formatByID(t, roleMismatch, "pdf").Capabilities[document.CapabilityText].State)
}

func syntheticImageDescriptor(t *testing.T) document.RenditionDescriptor {
	t.Helper()
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID: "synthetic-image", ContractVersion: document.RenditionProviderContractVersion,
		PolicyFingerprint: strings.Repeat("c", 64),
		TrustBoundary:     document.RenditionTrustOperatorNetwork,
		ReturnsStructured: true,
		SupportedFormats: []document.RenditionFormatCapability{{
			MediaFamily: "pdf", MediaType: "application/pdf",
			InputKind: document.RenditionInputOriginalFile,
		}},
		ArtifactRoles: []document.EvidenceArtifactRole{
			document.EvidenceArtifactImage, document.EvidenceArtifactStructured,
		},
	})
	require.NoError(t, err)
	return descriptor
}

func TestComputePreservesProviderAlternativesDeterministically(t *testing.T) {
	first := syntheticMarkdownDescriptor(t)
	second := syntheticImageDescriptor(t)
	sources := DefaultSources()
	sources.ExtractorID = "synthetic-extractor/v1"
	sources.BoundProviders = []document.RenditionDescriptor{second, first}

	record, err := Compute(sources)
	require.NoError(t, err)
	pdf := formatByID(t, record, "pdf")
	require.Len(t, pdf.Variants, 2)
	markdownVariant := variantByProvider(t, pdf, document.CapabilityText, first.Fingerprint)
	imageVariant := variantByProvider(t, pdf, document.CapabilityPages, second.Fingerprint)
	assert.Equal(t, document.CapabilityQualified,
		markdownVariant.Capabilities[document.CapabilityText].State)
	assert.Equal(t, document.CapabilityUnsupported,
		markdownVariant.Capabilities[document.CapabilityPages].State)
	assert.Equal(t, document.CapabilityUnsupported,
		imageVariant.Capabilities[document.CapabilityText].State)
	assert.Equal(t, document.CapabilityUnqualified,
		imageVariant.Capabilities[document.CapabilityPages].State)
	assert.Equal(t, document.CapabilityQualified, pdf.Capabilities[document.CapabilityText].State)
	assert.Equal(t, first.Fingerprint, pdf.Capabilities[document.CapabilityText].ProviderFingerprint)

	sources.BoundProviders = []document.RenditionDescriptor{first, second}
	reordered, err := Compute(sources)
	require.NoError(t, err)
	assert.Equal(t, record, reordered)
}

func TestComputePreservesTranscriptFamilyApplicability(t *testing.T) {
	provider := syntheticTranscriptDescriptor(t)
	sources := DefaultSources()
	sources.ExtractorID = "synthetic-extractor/v1"
	sources.BoundProviders = []document.RenditionDescriptor{provider}

	record, err := Compute(sources)
	require.NoError(t, err)
	assert.Equal(t, document.CapabilityNotApplicable,
		formatByID(t, record, "pdf").Capabilities[document.CapabilityTranscript].State)
	mp4 := formatByID(t, record, "mp4").Capabilities[document.CapabilityTranscript]
	assert.Equal(t, document.CapabilityUnqualified, mp4.State)
	assert.Equal(t, provider.Fingerprint, mp4.ProviderFingerprint)
}

func TestComputeRejectsConflictingMetadataDeclarationsInAnyOrder(t *testing.T) {
	qualified := MetadataCapability{
		CatalogID: "pdf", Evidence: "fixture", ImplementationID: strings.Repeat("e", 64),
	}
	notApplicable := MetadataCapability{CatalogID: "pdf", NotApplicable: true}
	for _, metadata := range [][]MetadataCapability{
		{qualified, notApplicable},
		{notApplicable, qualified},
	} {
		sources := DefaultSources()
		sources.ExtractorID = "synthetic-extractor/v1"
		sources.Metadata = metadata
		_, err := Compute(sources)
		require.ErrorContains(t, err, "conflicting metadata declarations for pdf")
	}
}

func TestComputeRejectsMutatedProviderIdentity(t *testing.T) {
	provider := syntheticMarkdownDescriptor(t)
	provider.ID = "forged-provider-id"
	sources := DefaultSources()
	sources.ExtractorID = "synthetic-extractor/v1"
	sources.BoundProviders = []document.RenditionDescriptor{provider}

	_, err := Compute(sources)
	require.ErrorContains(t, err, "descriptor")
}

func TestLookupPrefersCatalogAndPreservesTheOriginalQuery(t *testing.T) {
	sources := DefaultSources()
	sources.ExtractorID = "synthetic-extractor/v1"
	record, err := Compute(sources)
	require.NoError(t, err)

	for _, testCase := range []struct {
		query string
		match document.FormatLookupMatch
		id    string
	}{
		{query: ".JpG", match: document.FormatLookupFormat, id: "jpeg"},
		{query: "ZIP", match: document.FormatLookupFormat, id: "zip"},
		{query: "WPD", match: document.FormatLookupPending, id: "WPD"},
		{query: ".emlxpart", match: document.FormatLookupPending, id: "EMLXPART"},
		{query: " qqq ", match: document.FormatLookupUnknown},
	} {
		lookup := Lookup(record, testCase.query)
		assert.Equal(t, testCase.query, lookup.Query)
		assert.Equal(t, testCase.match, lookup.Match)
		switch testCase.match {
		case document.FormatLookupFormat:
			require.NotNil(t, lookup.Format)
			assert.Equal(t, testCase.id, lookup.Format.ID)
		case document.FormatLookupPending:
			require.NotNil(t, lookup.Pending)
			assert.Equal(t, testCase.id, lookup.Pending.Label)
		case document.FormatLookupUnknown:
			assert.Nil(t, lookup.Format)
			assert.Nil(t, lookup.Pending)
		}
	}
}

func syntheticMarkdownDescriptor(t *testing.T) document.RenditionDescriptor {
	t.Helper()
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID: "synthetic-markdown", ContractVersion: document.RenditionProviderContractVersion,
		PolicyFingerprint: strings.Repeat("b", 64),
		TrustBoundary:     document.RenditionTrustOperatorNetwork,
		ReturnsMarkdown:   true,
		ReturnsStructured: true,
		SupportedFormats: []document.RenditionFormatCapability{{
			MediaFamily: "pdf", MediaType: "application/pdf",
			InputKind: document.RenditionInputOriginalFile,
		}},
		ArtifactRoles: []document.EvidenceArtifactRole{
			document.EvidenceArtifactMarkdown, document.EvidenceArtifactStructured,
		},
	})
	require.NoError(t, err)
	return descriptor
}

func syntheticTranscriptDescriptor(t *testing.T) document.RenditionDescriptor {
	t.Helper()
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID: "synthetic-transcript", ContractVersion: document.RenditionProviderContractVersion,
		PolicyFingerprint: strings.Repeat("d", 64),
		TrustBoundary:     document.RenditionTrustOperatorNetwork,
		ReturnsStructured: true,
		SupportedFormats: []document.RenditionFormatCapability{
			{MediaFamily: "pdf", MediaType: "application/pdf", InputKind: document.RenditionInputOriginalFile},
			{MediaFamily: "audio_video", MediaType: "video/mp4", InputKind: document.RenditionInputOriginalFile},
		},
		ArtifactRoles: []document.EvidenceArtifactRole{
			document.EvidenceArtifactStructured, document.EvidenceArtifactTranscript,
		},
	})
	require.NoError(t, err)
	return descriptor
}

func variantByProvider(
	t *testing.T, format document.FormatCapabilityV1, capability document.CapabilityKey, fingerprint string,
) document.FormatVariantCapabilityV1 {
	t.Helper()
	for _, variant := range format.Variants {
		if variant.Capabilities[capability].ProviderFingerprint == fingerprint {
			return variant
		}
	}
	require.FailNow(t, "provider variant absent from coverage", fingerprint)
	return document.FormatVariantCapabilityV1{}
}

func formatByID(t *testing.T, record document.FormatCoverageV1, id string) document.FormatCapabilityV1 {
	t.Helper()
	for _, format := range record.Formats {
		if format.ID == id {
			return format
		}
	}
	require.FailNow(t, "format absent from coverage", id)
	return document.FormatCapabilityV1{}
}
