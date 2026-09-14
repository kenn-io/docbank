package document_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestSegmentEvidenceRequiresMatchingUnitOmissions(t *testing.T) {
	for _, test := range []struct {
		name   string
		kind   document.EvidenceLocatorKind
		origin document.EvidenceIndexOrigin
	}{
		{"page omission", document.EvidenceLocatorPage, document.EvidenceIndexOriginZero},
		{"one-based segment", document.EvidenceLocatorSegment, document.EvidenceIndexOriginOne},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := document.SourceEvidenceV1{
				ContractVersion: document.SourceEvidenceContractV1,
				Family:          "audio", Completeness: document.EvidencePartial, UnitKind: document.EvidenceUnitSegment,
				Units: []document.SourceEvidenceUnitV1{{
					Order: 0, Text: "synthetic cue",
					Locator: document.SourceEvidenceLocatorV1{
						Kind: document.EvidenceLocatorSegment, IndexOrigin: document.EvidenceIndexOriginZero,
						Start: 1000, End: 2000,
					},
				}},
				Omissions: []document.SourceEvidenceOmissionV1{{
					Kind: document.EvidenceOmissionUnit, Reason: "inaudible speech",
					Locator: &document.SourceEvidenceLocatorV1{
						Kind: document.EvidenceLocatorSegment, IndexOrigin: document.EvidenceIndexOriginZero,
						Start: 100, End: 500,
					},
				}},
			}
			policy, err := document.NewEvidencePolicy(10_000)
			require.NoError(t, err)
			normalized, err := document.NormalizeEvidenceV1(source, policy)
			require.NoError(t, err, "segment omissions need not cover every gap between cues")

			source.Omissions[0].Locator.Kind = test.kind
			source.Omissions[0].Locator.IndexOrigin = test.origin
			require.Error(t, document.ValidateSourceEvidenceV1(source))
			normalized.Checksum = ""
			normalized.Omissions[0].Locator.Kind = test.kind
			normalized.Omissions[0].Locator.IndexOrigin = test.origin
			_, _, err = document.MarshalNormalizedEvidenceV1(normalized)
			require.Error(t, err)
		})
	}
}

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
