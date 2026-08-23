package document_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestNormalizedEvidenceV1CanonicalizesProviderEvidence(t *testing.T) {
	source := syntheticSourceEvidenceV1()
	policy, err := document.NewEvidencePolicy(100_000)
	require.NoError(t, err)

	first, err := document.NormalizeEvidenceV1(source, policy)
	require.NoError(t, err)

	shuffled := source
	shuffled.Artifacts = append([]document.SourceEvidenceArtifactV1(nil), source.Artifacts...)
	shuffled.Units = append([]document.SourceEvidenceUnitV1(nil), source.Units...)
	shuffled.Artifacts[0].ProviderID = "provider-artifact-renamed"
	shuffled.Units[0].ProviderID = "provider-page-renamed"
	shuffled.Units[0].Regions = append([]document.SourceEvidenceRegionV1(nil), source.Units[0].Regions...)
	shuffled.Units[0].Regions[0].ProviderID = "provider-parent-renamed"
	shuffled.Units[0].Regions[1].ProviderID = "provider-child-renamed"
	shuffled.Units[0].Regions[1].ParentProviderID = "provider-parent-renamed"
	shuffled.Units[0].Regions[1].ArtifactProviderID = "provider-artifact-renamed"
	shuffled.Units[0].Tables = append([]document.SourceEvidenceTableV1(nil), source.Units[0].Tables...)
	shuffled.Units[0].Tables[0].ProviderID = "provider-table-renamed"
	shuffled.Units[0].Tables[0].RegionProviderID = "provider-parent-renamed"
	shuffled.Units[0].Tables[0].Cells = append(
		[]document.SourceEvidenceTableCellV1(nil), source.Units[0].Tables[0].Cells...,
	)
	shuffled.Units[0].Tables[0].Cells[0].RegionProviderID = "provider-child-renamed"

	second, err := document.NormalizeEvidenceV1(shuffled, policy)
	require.NoError(t, err)
	assert.Equal(t, first, second, "provider-local identifiers must not enter durable identity")

	require.Len(t, first.Units, 2)
	assert.Equal(t, "Café\nline two", first.Units[0].Text)
	assert.Equal(t, []string{"Résumé"}, first.Units[0].HeadingPath)
	assert.Regexp(t, `^unit_[0-9a-f]{64}$`, first.Units[0].ID)
	require.Len(t, first.Units[0].Regions, 2)
	assert.Regexp(t, `^region_[0-9a-f]{64}$`, first.Units[0].Regions[0].ID)
	assert.Equal(t, first.Units[0].Regions[0].ID, first.Units[0].Regions[1].ParentID)
	assert.Equal(t, first.Artifacts[0].ID, first.Units[0].Regions[1].ArtifactID)
	require.NotNil(t, first.Units[0].Regions[0].Confidence)
	assert.Equal(t, int64(875_000), first.Units[0].Regions[0].Confidence.Value)
	assert.Equal(t, int64(1_000_000), first.Units[0].Regions[0].Confidence.Scale)
	require.Len(t, first.Units[0].Tables, 1)
	assert.Equal(t, first.Units[0].Regions[0].ID, first.Units[0].Tables[0].RegionID)
	assert.Equal(t, first.Units[0].Regions[1].ID, first.Units[0].Tables[0].Cells[0].RegionID)

	encoded, checksum, err := document.MarshalNormalizedEvidenceV1(first)
	require.NoError(t, err)
	wantBytes, err := os.ReadFile("testdata/normalized-evidence-v1.golden.json")
	require.NoError(t, err)
	wantBytes = bytes.TrimSuffix(wantBytes, []byte("\n"))
	assert.Equal(t, string(wantBytes), string(encoded))
	wantDigest := sha256.Sum256(encoded)
	assert.Equal(t, hex.EncodeToString(wantDigest[:]), checksum)
	assert.Equal(t, checksum, first.Checksum)

	reencoded, secondChecksum, err := document.MarshalNormalizedEvidenceV1(second)
	require.NoError(t, err)
	assert.Equal(t, encoded, reencoded)
	assert.Equal(t, checksum, secondChecksum)

	changed := source
	changed.Units = append([]document.SourceEvidenceUnitV1(nil), source.Units...)
	changed.Units[0].Text = "different evidence"
	changedNormalized, err := document.NormalizeEvidenceV1(changed, policy)
	require.NoError(t, err)
	assert.NotEqual(t, first.Units[0].ID, changedNormalized.Units[0].ID)
	assert.NotEqual(t, first.Checksum, changedNormalized.Checksum)

	tampered := first
	tampered.Checksum = ""
	tampered.Units = append([]document.NormalizedEvidenceUnitV1(nil), first.Units...)
	tampered.Units[0].Text = "CafX\nline two"
	_, _, err = document.MarshalNormalizedEvidenceV1(tampered)
	require.ErrorContains(t, err, "unit ID")

	tampered = first
	tampered.Checksum = ""
	tampered.Artifacts = append([]document.EvidenceArtifactV1(nil), first.Artifacts...)
	tampered.Artifacts[0].Pointer = "provider/other.json"
	_, _, err = document.MarshalNormalizedEvidenceV1(tampered)
	require.ErrorContains(t, err, "artifact ID")
}

func TestNormalizedEvidenceV1CanonicalizesUnorderedCollections(t *testing.T) {
	source := syntheticSourceEvidenceV1()
	source.Artifacts = append(source.Artifacts, document.SourceEvidenceArtifactV1{
		ProviderID: "provider-artifact-2",
		Pointer:    "provider/page.png",
		Role:       document.EvidenceArtifactImage,
		SHA256:     strings.Repeat("2", sha256.Size*2),
	})
	source.Completeness = document.EvidencePartial
	source.Omissions = []document.SourceEvidenceOmissionV1{
		{Kind: document.EvidenceOmissionRange, Range: &document.EvidenceTextRangeV1{Start: 7, End: 15}, Reason: "redacted"},
		{Kind: document.EvidenceOmissionRange, Range: &document.EvidenceTextRangeV1{Start: 0, End: 3}, Reason: "redacted"},
	}
	policy, err := document.NewEvidencePolicy(100_000)
	require.NoError(t, err)

	first, err := document.NormalizeEvidenceV1(source, policy)
	require.NoError(t, err)
	reordered := source
	reordered.Artifacts = slices.Clone(source.Artifacts)
	slices.Reverse(reordered.Artifacts)
	reordered.Omissions = slices.Clone(source.Omissions)
	slices.Reverse(reordered.Omissions)
	second, err := document.NormalizeEvidenceV1(reordered, policy)
	require.NoError(t, err)

	assert.Equal(t, first, second)
}

func TestSourceEvidenceV1RejectsInvalidAuthority(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*document.SourceEvidenceV1)
		want   string
	}{
		{
			name: "unknown completeness",
			mutate: func(source *document.SourceEvidenceV1) {
				source.Completeness = document.EvidenceCompleteness("unknown")
			},
			want: "completeness",
		},
		{
			name: "noncontiguous units",
			mutate: func(source *document.SourceEvidenceV1) {
				source.Units[1].Order = 3
			},
			want: "noncontiguous unit order",
		},
		{
			name: "missing page locator",
			mutate: func(source *document.SourceEvidenceV1) {
				source.Units[0].Locator = document.SourceEvidenceLocatorV1{}
			},
			want: "page locator",
		},
		{
			name: "nonfinite confidence",
			mutate: func(source *document.SourceEvidenceV1) {
				source.Units[0].Regions[0].Confidence.Value = math.NaN()
			},
			want: "confidence",
		},
		{
			name: "invalid geometry",
			mutate: func(source *document.SourceEvidenceV1) {
				source.Units[0].Regions[0].Geometry.Boxes[0].Right = -1
			},
			want: "geometry box",
		},
		{
			name: "range splits normalization sequence",
			mutate: func(source *document.SourceEvidenceV1) {
				source.Units[0].Regions[0].TextRange = document.EvidenceTextRangeV1{Start: 0, End: 4}
			},
			want: "normalization boundary",
		},
		{
			name: "mutable artifact URL",
			mutate: func(source *document.SourceEvidenceV1) {
				source.Artifacts[0].Pointer = "https://provider.test/result/1"
			},
			want: "artifact pointer",
		},
		{
			name: "unknown parent",
			mutate: func(source *document.SourceEvidenceV1) {
				source.Units[0].Regions[1].ParentProviderID = "missing"
			},
			want: "unknown parent",
		},
		{
			name: "table points at non-table region",
			mutate: func(source *document.SourceEvidenceV1) {
				source.Units[0].Regions[0].Kind = document.EvidenceRegionParagraph
			},
			want: "table region",
		},
		{
			name: "cell points at non-cell region",
			mutate: func(source *document.SourceEvidenceV1) {
				source.Units[0].Regions[1].Kind = document.EvidenceRegionParagraph
			},
			want: "cell region",
		},
		{
			name: "complete manifest has unit omission",
			mutate: func(source *document.SourceEvidenceV1) {
				source.Units[0].Omissions = []document.SourceEvidenceOmissionV1{{
					Kind: document.EvidenceOmissionField, Field: "geometry", Reason: "provider omitted geometry",
				}}
			},
			want: "complete source evidence",
		},
		{
			name: "invalid artifact digest",
			mutate: func(source *document.SourceEvidenceV1) {
				source.Artifacts[0].SHA256 = "ABC"
			},
			want: "SHA-256",
		},
		{
			name: "duplicate canonical artifact",
			mutate: func(source *document.SourceEvidenceV1) {
				duplicate := source.Artifacts[0]
				duplicate.ProviderID = "provider-artifact-duplicate"
				source.Artifacts = append(source.Artifacts, duplicate)
			},
			want: "canonical identity",
		},
		{
			name: "confidence bounds collapse after fixed-point conversion",
			mutate: func(source *document.SourceEvidenceV1) {
				source.Units[0].Confidence = &document.SourceEvidenceConfidenceV1{
					Interpretation: document.EvidenceConfidenceHigherIsBetter,
					Minimum:        0,
					Maximum:        0.0000001,
					Value:          0.00000005,
				}
			},
			want: "fixed-point confidence",
		},
		{
			name: "unknown document family",
			mutate: func(source *document.SourceEvidenceV1) {
				source.Family = "future-family"
			},
			want: "unknown document family",
		},
		{
			name: "overflowing table row",
			mutate: func(source *document.SourceEvidenceV1) {
				source.Units[0].Tables[0].Cells[0].Row = math.MaxInt
			},
			want: "coordinates",
		},
		{
			name: "artifact pointer query",
			mutate: func(source *document.SourceEvidenceV1) {
				source.Artifacts[0].Pointer = "provider/evidence.json?mutable=1"
			},
			want: "artifact pointer",
		},
		{
			name: "artifact pointer fragment",
			mutate: func(source *document.SourceEvidenceV1) {
				source.Artifacts[0].Pointer = "provider/evidence.json#mutable"
			},
			want: "artifact pointer",
		},
		{
			name: "artifact pointer escaped traversal",
			mutate: func(source *document.SourceEvidenceV1) {
				source.Artifacts[0].Pointer = "provider/%2e%2e/evidence.json"
			},
			want: "artifact pointer",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := syntheticSourceEvidenceV1()
			test.mutate(&source)
			require.ErrorContains(t, document.ValidateSourceEvidenceV1(source), test.want)
		})
	}
}

func TestSourceEvidenceV1AllowsExplicitDegradedGenericUnit(t *testing.T) {
	source := document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1,
		Completeness:    document.EvidenceDegradedProvenance,
		Family:          "pdf",
		UnitKind:        document.EvidenceUnitGeneric,
		Omissions: []document.SourceEvidenceOmissionV1{{
			Kind: document.EvidenceOmissionField, Field: "natural_provenance",
			Reason: "converter returned Markdown without page structure",
		}},
		Units: []document.SourceEvidenceUnitV1{{
			Order: 0, Text: "readable evidence",
			Locator: document.SourceEvidenceLocatorV1{
				Kind: document.EvidenceLocatorGeneric, IndexOrigin: document.EvidenceIndexOriginNone,
			},
		}},
	}

	require.NoError(t, document.ValidateSourceEvidenceV1(source))
	policy, err := document.NewEvidencePolicy(1_000)
	require.NoError(t, err)
	normalized, err := document.NormalizeEvidenceV1(source, policy)
	require.NoError(t, err)
	assert.Equal(t, document.EvidenceDegradedProvenance, normalized.Completeness)
	assert.Equal(t, document.EvidenceLocatorGeneric, normalized.Units[0].Locator.Kind)
}

func TestMarshalNormalizedEvidenceV1RejectsExtremeConfidenceBounds(t *testing.T) {
	policy, err := document.NewEvidencePolicy(1_000)
	require.NoError(t, err)
	normalized, err := document.NormalizeEvidenceV1(syntheticSourceEvidenceV1(), policy)
	require.NoError(t, err)

	normalized.Checksum = ""
	normalized.Units[0].Confidence = &document.EvidenceConfidenceV1{
		Interpretation: document.EvidenceConfidenceHigherIsBetter,
		Minimum:        math.MinInt64,
		Maximum:        1,
		Scale:          document.EvidenceFixedScale,
		Value:          0,
	}

	_, _, err = document.MarshalNormalizedEvidenceV1(normalized)
	require.ErrorContains(t, err, "confidence")
}

func TestMarshalNormalizedEvidenceV1RejectsInvalidOmissions(t *testing.T) {
	policy, err := document.NewEvidencePolicy(1_000)
	require.NoError(t, err)
	normalized, err := document.NormalizeEvidenceV1(syntheticSourceEvidenceV1(), policy)
	require.NoError(t, err)

	normalized.Checksum = ""
	normalized.Omissions = []document.EvidenceOmissionV1{{
		Kind: document.EvidenceOmissionKind("unknown"), Reason: "provider omitted evidence",
	}}

	_, _, err = document.MarshalNormalizedEvidenceV1(normalized)
	require.ErrorContains(t, err, "omission")
}

func TestNormalizeEvidenceV1MapsManyRanges(t *testing.T) {
	const regionCount = 5_000
	regions := make([]document.SourceEvidenceRegionV1, regionCount)
	for index := range regions {
		regions[index] = document.SourceEvidenceRegionV1{
			ProviderID: fmt.Sprintf("region-%d", index),
			Kind:       document.EvidenceRegionParagraph,
			Order:      index,
			TextRange:  document.EvidenceTextRangeV1{Start: index * 10, End: index*10 + 1},
		}
	}
	source := document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1,
		Completeness:    document.EvidenceComplete,
		Family:          "text",
		UnitKind:        document.EvidenceUnitSection,
		Units: []document.SourceEvidenceUnitV1{{
			Order: 0, Text: strings.Repeat("x", regionCount*10), Regions: regions,
			Locator: document.SourceEvidenceLocatorV1{
				Kind: document.EvidenceLocatorSection, IndexOrigin: document.EvidenceIndexOriginNone,
			},
		}},
	}
	policy, err := document.NewEvidencePolicy(regionCount * 10)
	require.NoError(t, err)

	normalized, err := document.NormalizeEvidenceV1(source, policy)
	require.NoError(t, err)
	require.Len(t, normalized.Units[0].Regions, regionCount)
}

func TestNormalizeEvidenceV1PartitionsDocumentRangesByUnit(t *testing.T) {
	const unitCount = 2_000
	units := make([]document.SourceEvidenceUnitV1, unitCount)
	omissions := make([]document.SourceEvidenceOmissionV1, unitCount)
	for index := range unitCount {
		units[index] = document.SourceEvidenceUnitV1{
			Order: index, Text: "x",
			Locator: document.SourceEvidenceLocatorV1{
				Kind: document.EvidenceLocatorSection, IndexOrigin: document.EvidenceIndexOriginNone,
			},
		}
		omissions[index] = document.SourceEvidenceOmissionV1{
			Kind: document.EvidenceOmissionRange, Range: &document.EvidenceTextRangeV1{Start: 0, End: 1},
			Reason: "redacted", UnitOrder: index,
		}
	}
	source := document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1,
		Completeness:    document.EvidencePartial,
		Family:          "text",
		Omissions:       omissions,
		UnitKind:        document.EvidenceUnitSection,
		Units:           units,
	}
	policy, err := document.NewEvidencePolicy(unitCount)
	require.NoError(t, err)

	normalized, err := document.NormalizeEvidenceV1(source, policy)
	require.NoError(t, err)
	require.Len(t, normalized.Omissions, unitCount)
}

func syntheticSourceEvidenceV1() document.SourceEvidenceV1 {
	return document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1,
		Completeness:    document.EvidenceComplete,
		Family:          "pdf",
		UnitKind:        document.EvidenceUnitPage,
		Artifacts: []document.SourceEvidenceArtifactV1{{
			ProviderID: "provider-artifact-7",
			Pointer:    "provider/structured-evidence.json",
			Role:       document.EvidenceArtifactStructured,
			SHA256:     "1111111111111111111111111111111111111111111111111111111111111111",
		}},
		Units: []document.SourceEvidenceUnitV1{
			{
				ProviderID:  "provider-page-3",
				Order:       0,
				Text:        "Cafe\u0301\r\nline two",
				HeadingPath: []string{"Re\u0301sume\u0301"},
				Locator: document.SourceEvidenceLocatorV1{
					Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne,
					Start: 1, End: 1,
				},
				Regions: []document.SourceEvidenceRegionV1{
					{
						ProviderID: "provider-region-9", Order: 0, Kind: document.EvidenceRegionTable,
						TextRange: document.EvidenceTextRangeV1{Start: 0, End: 5},
						Confidence: &document.SourceEvidenceConfidenceV1{
							Interpretation: document.EvidenceConfidenceProbability,
							Minimum:        0, Maximum: 1, Value: 0.875,
						},
						Geometry: &document.SourceEvidenceGeometryV1{
							CoordinateOrigin: document.EvidenceCoordinateTopLeft,
							CoordinateSpace:  document.EvidenceCoordinatePage,
							Unit:             document.EvidenceGeometryPixel,
							Scale:            1_000,
							Width:            800_000,
							Height:           1_200_000,
							Orientation:      0,
							Boxes: []document.EvidenceBoxV1{{
								Left: 10_000, Top: 20_000, Right: 300_000, Bottom: 80_000,
							}},
						},
					},
					{
						ProviderID: "provider-region-2", ParentProviderID: "provider-region-9",
						ArtifactProviderID: "provider-artifact-7", Order: 1,
						Kind:      document.EvidenceRegionTableCell,
						TextRange: document.EvidenceTextRangeV1{Start: 7, End: 15},
					},
				},
				Tables: []document.SourceEvidenceTableV1{{
					ProviderID: "provider-table-4", Order: 0, RegionProviderID: "provider-region-9",
					Rows: 1, Columns: 1,
					Cells: []document.SourceEvidenceTableCellV1{{
						Order: 0, Row: 0, Column: 0, RowSpan: 1, ColumnSpan: 1, Header: true,
						RegionProviderID: "provider-region-2",
						TextRange:        document.EvidenceTextRangeV1{Start: 7, End: 15},
					}},
				}},
			},
			{
				ProviderID: "provider-page-8", Order: 1, Text: "second page",
				Locator: document.SourceEvidenceLocatorV1{
					Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne,
					Start: 2, End: 2,
				},
			},
		},
	}
}
