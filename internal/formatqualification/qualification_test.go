package formatqualification

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testExtractorFingerprint = "2810f932642b0a1ec3e4df0fbd6287a223b3e2c8add35c6e95b40281f7fff786"

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

func TestQualificationsReturnsACopy(t *testing.T) {
	qualifications := All()
	require.NotEmpty(t, qualifications)
	qualifications[0].Evidence = "forged"
	assert.NotEqual(t, "forged", All()[0].Evidence)
}
