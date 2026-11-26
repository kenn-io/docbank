package canonical

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testExtractorFingerprint = "764170f0fbd26cdfb6c2d6672a4561c82f44755115e8e0b25b4f5cfbe4de09ab"

func TestLookupRequiresTheExactQualifiedTuple(t *testing.T) {
	query := FormatQualificationQuery{
		CatalogID:        "pdf",
		Capability:       FormatCapabilityMetadata,
		Evidence:         "TestExtractSourceMetadataUsesAuthoritativePDFInfo",
		ImplementationID: testExtractorFingerprint,
		InputKind:        FormatInputOriginalFile,
	}
	qualification, found := LookupFormatQualification(query)
	require.True(t, found)
	assert.Equal(t, FormatQualification(query), qualification)

	for _, mutate := range []func(*FormatQualificationQuery){
		func(value *FormatQualificationQuery) { value.CatalogID = "docx" },
		func(value *FormatQualificationQuery) { value.Capability = FormatCapabilityDetect },
		func(value *FormatQualificationQuery) { value.Evidence = "TestNameIsNotProof" },
		func(value *FormatQualificationQuery) { value.ImplementationID = "" },
		func(value *FormatQualificationQuery) { value.InputKind = "" },
	} {
		forged := query
		mutate(&forged)
		_, found := LookupFormatQualification(forged)
		assert.False(t, found, "%+v", forged)
	}
}

func TestQualificationsReturnsACopy(t *testing.T) {
	qualifications := AllFormatQualifications()
	require.NotEmpty(t, qualifications)
	qualifications[0].Evidence = "forged"
	assert.NotEqual(t, "forged", AllFormatQualifications()[0].Evidence)
}
