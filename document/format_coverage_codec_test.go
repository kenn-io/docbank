package document

import (
	"encoding/json/v2"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const qualifiedMetadataFingerprint = "42b01ef9219b3b35dedf49ef98d311b21772da27631c3ca90597f28363de1ec5"

func TestFormatCoverageCanonicalRoundTripAndRejections(t *testing.T) {
	record := validFormatCoverageRecord()
	encoded, fingerprint, err := MarshalFormatCoverageV1(record)
	require.NoError(t, err)
	assert.Len(t, fingerprint, 64)

	decoded, err := DecodeFormatCoverageV1(encoded)
	require.NoError(t, err)
	assert.Equal(t, record, decoded)

	decoded, err = DecodeFormatCoverageV1(append([]byte(" \n\t"), encoded...))
	require.NoError(t, err)
	assert.Equal(t, record, decoded)

	for _, testCase := range []struct {
		name string
		raw  []byte
		want string
	}{
		{name: "unknown member", raw: []byte(strings.Replace(string(encoded), `"contract_version":`, `"unknown":true,"contract_version":`, 1)), want: "unknown"},
		{name: "duplicate member", raw: []byte(strings.Replace(string(encoded), `"contract_version":`, `"contract_version":"format-coverage/v1","contract_version":`, 1)), want: "duplicate"},
		{name: "trailing value", raw: append(slices.Clone(encoded), []byte(` {}`)...), want: "after top-level value"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := DecodeFormatCoverageV1(testCase.raw)
			require.ErrorContains(t, err, testCase.want)
		})
	}
}

func TestFormatCoverageValidationRequiresCompleteRegisteredClaims(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*FormatCoverageV1)
		want   string
	}{
		{name: "missing capability", want: "capability", mutate: func(value *FormatCoverageV1) {
			delete(value.Formats[0].Capabilities, CapabilityDetect)
		}},
		{name: "unknown capability", want: "capability", mutate: func(value *FormatCoverageV1) {
			value.Formats[0].Capabilities["ocr"] = CapabilityStateV1{State: CapabilityUnsupported}
		}},
		{name: "unknown state", want: "state", mutate: func(value *FormatCoverageV1) {
			value.Formats[0].Capabilities[CapabilityDetect] = CapabilityStateV1{State: "maybe"}
		}},
		{name: "missing qualified evidence", want: "evidence", mutate: func(value *FormatCoverageV1) {
			value.Formats[0].Capabilities[CapabilityMetadata] = CapabilityStateV1{State: CapabilityQualified}
		}},
		{name: "arbitrary qualified evidence", want: "registered", mutate: func(value *FormatCoverageV1) {
			value.Formats[0].Capabilities[CapabilityMetadata] = CapabilityStateV1{State: CapabilityQualified, Evidence: "TestNameIsNotQualification"}
		}},
		{name: "mismatched implementation", want: "registered", mutate: func(value *FormatCoverageV1) {
			value.GeneratedBy.ExtractorID = strings.Repeat("b", 64)
		}},
		{name: "provider without fingerprint", want: "provider fingerprint", mutate: func(value *FormatCoverageV1) {
			state := value.Formats[0].Capabilities[CapabilityMetadata]
			state.Provider = "synthetic"
			value.Formats[0].Capabilities[CapabilityMetadata] = state
		}},
		{name: "oversized evidence", want: "evidence", mutate: func(value *FormatCoverageV1) {
			state := value.Formats[0].Capabilities[CapabilityMetadata]
			state.Evidence = strings.Repeat("a", MaxCoverageEvidenceBytes+1)
			value.Formats[0].Capabilities[CapabilityMetadata] = state
		}},
		{name: "oversized note", want: "note", mutate: func(value *FormatCoverageV1) {
			state := value.Formats[0].Capabilities[CapabilityDetect]
			state.Note = strings.Repeat("a", MaxCoverageNoteBytes+1)
			value.Formats[0].Capabilities[CapabilityDetect] = state
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			value := cloneCoverageForTest(validFormatCoverageRecord())
			testCase.mutate(&value)
			require.ErrorContains(t, ValidateFormatCoverageV1(value), testCase.want)
		})
	}
}

func TestFormatCoverageValidationBindsProviderEvidenceToItsDescriptor(t *testing.T) {
	const descriptorFingerprint = "129bea875ffc3ab1511ef447c82379298c1d358923bec6af91433786148cbc95"
	providerless := cloneCoverageForTest(validFormatCoverageRecord())
	providerless.Formats[0].Capabilities[CapabilityText] = CapabilityStateV1{
		State: CapabilityQualified, Evidence: "TestSyntheticMarkdownPDFOutput",
	}
	require.ErrorContains(t, ValidateFormatCoverageV1(providerless), "registered")
	providerlessJSON, err := json.Marshal(providerless)
	require.NoError(t, err)
	_, err = DecodeFormatCoverageV1(providerlessJSON)
	require.ErrorContains(t, err, "registered")

	identified := cloneCoverageForTest(validFormatCoverageRecord())
	identified.GeneratedBy.BoundProviders = []string{descriptorFingerprint}
	identified.Formats[0].Capabilities[CapabilityText] = CapabilityStateV1{
		State: CapabilityQualified, Provider: "synthetic-markdown",
		ProviderFingerprint: descriptorFingerprint, Evidence: "TestSyntheticMarkdownPDFOutput",
	}
	require.NoError(t, ValidateFormatCoverageV1(identified))
	encoded, _, err := MarshalFormatCoverageV1(identified)
	require.NoError(t, err)
	decoded, err := DecodeFormatCoverageV1(encoded)
	require.NoError(t, err)
	assert.Equal(t, identified, decoded)

	identified.GeneratedBy.BoundProviders = nil
	require.ErrorContains(t, ValidateFormatCoverageV1(identified), "bound provider")
}

func TestFormatCoverageValidationKeepsOrderedProviderAlternatives(t *testing.T) {
	record := validFormatCoverageRecord()
	unqualified := completeCapabilityStates(CapabilityUnsupported)
	unqualified[CapabilityDetect] = CapabilityStateV1{State: CapabilityUnqualified}
	unsupported := completeCapabilityStates(CapabilityUnsupported)
	record.Formats[0].Variants = []FormatVariantCapabilityV1{
		{Capabilities: unqualified},
		{Capabilities: unsupported},
	}
	require.NoError(t, ValidateFormatCoverageV1(record))

	reversed := cloneCoverageForTest(record)
	slices.Reverse(reversed.Formats[0].Variants)
	require.ErrorContains(t, ValidateFormatCoverageV1(reversed), "sorted")

	duplicate := cloneCoverageForTest(record)
	duplicate.Formats[0].Variants[1] = duplicate.Formats[0].Variants[0]
	require.ErrorContains(t, ValidateFormatCoverageV1(duplicate), "duplicate")
}

func TestDecodeFormatCoverageReturnsDeepCanonicalCopy(t *testing.T) {
	record := validFormatCoverageRecord()
	encoded, _, err := MarshalFormatCoverageV1(record)
	require.NoError(t, err)
	decoded, err := DecodeFormatCoverageV1(encoded)
	require.NoError(t, err)

	decoded.Formats[0].Extensions[0] = "forged"
	decoded.Formats[0].Capabilities[CapabilityDetect] = CapabilityStateV1{State: CapabilityQualified}
	decoded.Pending = append(decoded.Pending, PendingFormatV1{Label: "FORGED"})
	assert.Equal(t, []string{"pdf"}, record.Formats[0].Extensions)
	assert.Equal(t, CapabilityUnsupported, record.Formats[0].Capabilities[CapabilityDetect].State)
	assert.Empty(t, record.Pending)
}

func validFormatCoverageRecord() FormatCoverageV1 {
	capabilities := completeCapabilityStates(CapabilityUnsupported)
	capabilities[CapabilityMetadata] = CapabilityStateV1{
		State: CapabilityQualified, Evidence: "TestExtractSourceMetadataUsesAuthoritativePDFInfo",
	}
	capabilities[CapabilityExpand] = CapabilityStateV1{State: CapabilityUnqualified}
	capabilities[CapabilityTranscript] = CapabilityStateV1{State: CapabilityNotApplicable}
	return FormatCoverageV1{
		ContractVersion: FormatCoverageContractV1,
		Formats: []FormatCapabilityV1{{
			ID: "pdf", QueryFamily: "document", MediaType: "application/pdf",
			Extensions: []string{"pdf"}, UnitKind: "page", Capabilities: capabilities,
			Variants: []FormatVariantCapabilityV1{},
		}},
		GeneratedBy: CoverageSourcesV1{
			BoundProviders: []string{}, CatalogRows: 1,
			ExtractorID: qualifiedMetadataFingerprint,
		},
		Pending: []PendingFormatV1{},
	}
}

func completeCapabilityStates(state CapabilityState) map[CapabilityKey]CapabilityStateV1 {
	result := make(map[CapabilityKey]CapabilityStateV1, len(AllCapabilityKeys()))
	for _, capability := range AllCapabilityKeys() {
		result[capability] = CapabilityStateV1{State: state}
	}
	return result
}

func cloneCoverageForTest(value FormatCoverageV1) FormatCoverageV1 {
	encoded, _, err := MarshalFormatCoverageV1(value)
	if err != nil {
		panic(err)
	}
	clone, err := DecodeFormatCoverageV1(encoded)
	if err != nil {
		panic(err)
	}
	return clone
}
