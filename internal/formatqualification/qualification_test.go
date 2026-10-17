package formatqualification

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testExtractorFingerprint = "964ac280176d2884d54452ad11eb3601cd7f4c3ffb010afe40f8fc0ad6d53683"

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
