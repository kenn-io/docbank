package document_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestSegmentEvidenceAllowsAdjacentAndOverlappingCues(t *testing.T) {
	source := document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1,
		Family:          "audio",
		Completeness:    document.EvidencePartial,
		UnitKind:        document.EvidenceUnitSegment,
		Omissions: []document.SourceEvidenceOmissionV1{{
			Kind:   document.EvidenceOmissionField,
			Field:  "non_speech_content",
			Reason: "speech transcript does not describe non-speech content",
		}},
	}
	for i, span := range [][2]int64{{0, 1000}, {1000, 2000}, {1500, 2500}, {5000, 6000}} {
		source.Units = append(source.Units, document.SourceEvidenceUnitV1{
			Order: i,
			Text:  "synthetic cue",
			Locator: document.SourceEvidenceLocatorV1{
				Kind:        document.EvidenceLocatorSegment,
				IndexOrigin: document.EvidenceIndexOriginZero,
				Start:       span[0],
				End:         span[1],
			},
		})
	}
	policy, err := document.NewEvidencePolicy(10_000)
	require.NoError(t, err)
	normalized, err := document.NormalizeEvidenceV1(source, policy)
	require.NoError(t, err)
	raw, sum, err := document.MarshalNormalizedEvidenceV1(normalized)
	require.NoError(t, err)
	require.NotEmpty(t, raw)
	require.Len(t, sum, 64)

	source.Units[0].Locator.End = 0
	require.Error(t, document.ValidateSourceEvidenceV1(source))
}
