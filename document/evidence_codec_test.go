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
	"unicode/utf8"

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
	wantBytes = bytes.TrimSuffix(wantBytes, []byte("\r\n"))
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
			name: "duplicate page locator",
			mutate: func(source *document.SourceEvidenceV1) {
				source.Units[1].Locator.Start = 1
				source.Units[1].Locator.End = 1
			},
			want: "locator sequence overlaps",
		},
		{
			name: "mixed locator origins",
			mutate: func(source *document.SourceEvidenceV1) {
				source.Units[1].Locator.IndexOrigin = document.EvidenceIndexOriginZero
			},
			want: "locator sequence changes index origin",
		},
		{
			name: "complete locator gap",
			mutate: func(source *document.SourceEvidenceV1) {
				source.Units[1].Locator.Start = 3
				source.Units[1].Locator.End = 3
			},
			want: "complete locator sequence has a gap",
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
		{
			name: "artifact pointer parent directory",
			mutate: func(source *document.SourceEvidenceV1) {
				source.Artifacts[0].Pointer = ".."
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

func TestEvidenceV1LocatorSequenceRequiresGapOmission(t *testing.T) {
	source := syntheticSourceEvidenceV1()
	source.Completeness = document.EvidencePartial
	source.Omissions = []document.SourceEvidenceOmissionV1{{
		Kind: document.EvidenceOmissionField, Field: "source_unit", Reason: "provider omitted page 2",
	}}
	source.Units[1].Locator.Start = 3
	source.Units[1].Locator.End = 3

	require.ErrorContains(t, document.ValidateSourceEvidenceV1(source), "locator gap")
}

func TestEvidenceV1LocatorSequenceAllowsDeclaredGap(t *testing.T) {
	source := syntheticSourceEvidenceV1()
	source.Completeness = document.EvidencePartial
	source.Omissions = []document.SourceEvidenceOmissionV1{{
		Kind: document.EvidenceOmissionUnit,
		Locator: &document.SourceEvidenceLocatorV1{
			Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne,
			Start: 2, End: 99,
		},
		Reason: "provider omitted pages 2 through 99",
	}}
	source.Units[1].Locator.Start = 100
	source.Units[1].Locator.End = 100

	require.NoError(t, document.ValidateSourceEvidenceV1(source))
	policy, err := document.NewEvidencePolicy(100_000)
	require.NoError(t, err)
	normalized, err := document.NormalizeEvidenceV1(source, policy)
	require.NoError(t, err)
	require.Len(t, normalized.Omissions, 1)
	require.NotNil(t, normalized.Omissions[0].Locator)
	assert.Equal(t, int64(2), normalized.Omissions[0].Locator.Start)
	assert.Equal(t, int64(99), normalized.Omissions[0].Locator.End)
}

func TestEvidenceV1LocatorSequenceAllowsCoveredAndTrailingOmissions(t *testing.T) {
	t.Run("split interior gap", func(t *testing.T) {
		source := syntheticSourceEvidenceV1()
		source.Completeness = document.EvidencePartial
		source.Units[1].Locator.Start = 5
		source.Units[1].Locator.End = 5
		source.Omissions = []document.SourceEvidenceOmissionV1{
			{
				Kind: document.EvidenceOmissionUnit,
				Locator: &document.SourceEvidenceLocatorV1{
					Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne,
					Start: 2, End: 3,
				},
				Reason: "provider omitted pages 2 and 3",
			},
			{
				Kind: document.EvidenceOmissionUnit,
				Locator: &document.SourceEvidenceLocatorV1{
					Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne,
					Start: 4, End: 4,
				},
				Reason: "provider omitted page 4",
			},
		}

		policy, err := document.NewEvidencePolicy(100_000)
		require.NoError(t, err)
		_, err = document.NormalizeEvidenceV1(source, policy)
		require.NoError(t, err)
	})

	t.Run("trailing range", func(t *testing.T) {
		source := syntheticSourceEvidenceV1()
		source.Completeness = document.EvidencePartial
		source.Omissions = []document.SourceEvidenceOmissionV1{{
			Kind: document.EvidenceOmissionUnit,
			Locator: &document.SourceEvidenceLocatorV1{
				Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne,
				Start: 3, End: 10,
			},
			Reason: "provider omitted trailing pages",
		}}

		policy, err := document.NewEvidencePolicy(100_000)
		require.NoError(t, err)
		_, err = document.NormalizeEvidenceV1(source, policy)
		require.NoError(t, err)
	})
}

func TestEvidenceV1LocatorSequenceAllowsNamedUnitOmissions(t *testing.T) {
	for _, unitKind := range []document.EvidenceUnitKind{
		document.EvidenceUnitMessage,
		document.EvidenceUnitSection,
	} {
		t.Run(string(unitKind), func(t *testing.T) {
			family := "mail"
			if unitKind == document.EvidenceUnitSection {
				family = "text"
			}
			locatorKind := document.EvidenceLocatorKind(unitKind)
			source := document.SourceEvidenceV1{
				ContractVersion: document.SourceEvidenceContractV1,
				Completeness:    document.EvidencePartial,
				Family:          family,
				UnitKind:        unitKind,
				Units: []document.SourceEvidenceUnitV1{{
					Order: 0, Text: "present unit",
					Locator: document.SourceEvidenceLocatorV1{
						Kind: locatorKind, IndexOrigin: document.EvidenceIndexOriginNone, Name: "present",
					},
				}},
				Omissions: []document.SourceEvidenceOmissionV1{{
					Kind: document.EvidenceOmissionUnit,
					Locator: &document.SourceEvidenceLocatorV1{
						Kind: locatorKind, IndexOrigin: document.EvidenceIndexOriginNone, Name: "missing",
					},
					Reason: "provider omitted a named unit",
				}},
			}

			policy, err := document.NewEvidencePolicy(100_000)
			require.NoError(t, err)
			_, err = document.NormalizeEvidenceV1(source, policy)
			require.NoError(t, err)
		})
	}
}

func TestEvidenceV1RejectsUnitOmissionsOutsideLocatorGaps(t *testing.T) {
	tests := []struct {
		name    string
		locator document.EvidenceLocatorV1
	}{
		{
			name: "wrong kind",
			locator: document.EvidenceLocatorV1{
				Kind: document.EvidenceLocatorSlide, IndexOrigin: document.EvidenceIndexOriginOne,
				Start: 2, End: 2,
			},
		},
		{
			name: "wrong origin",
			locator: document.EvidenceLocatorV1{
				Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginZero,
				Start: 2, End: 2,
			},
		},
		{
			name: "overlaps present unit",
			locator: document.EvidenceLocatorV1{
				Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne,
				Start: 1, End: 1,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := syntheticSourceEvidenceV1()
			source.Completeness = document.EvidencePartial
			sourceLocator := document.SourceEvidenceLocatorV1(test.locator)
			source.Omissions = []document.SourceEvidenceOmissionV1{{
				Kind: document.EvidenceOmissionUnit, Locator: &sourceLocator, Reason: "provider omitted a unit",
			}}

			require.ErrorContains(t, document.ValidateSourceEvidenceV1(source), "does not match a locator gap")
			source.Units[1].Locator.Start = 3
			source.Units[1].Locator.End = 3
			source.Omissions = []document.SourceEvidenceOmissionV1{{
				Kind: document.EvidenceOmissionUnit,
				Locator: &document.SourceEvidenceLocatorV1{
					Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne,
					Start: 2, End: 2,
				},
				Reason: "provider omitted page 2",
			}}
			policy, err := document.NewEvidencePolicy(100_000)
			require.NoError(t, err)
			normalized, err := document.NormalizeEvidenceV1(source, policy)
			require.NoError(t, err)
			normalized.Checksum = ""
			normalized.Omissions[0].Locator = &test.locator

			_, _, err = document.MarshalNormalizedEvidenceV1(normalized)
			require.ErrorContains(t, err, "does not match a locator gap")
		})
	}
}

func TestEvidenceV1RejectsDocumentFieldOmissionUnitOrder(t *testing.T) {
	for _, unitOrder := range []int{-1, 99} {
		t.Run(fmt.Sprintf("unit order %d", unitOrder), func(t *testing.T) {
			source := syntheticSourceEvidenceV1()
			source.Completeness = document.EvidencePartial
			source.Omissions = []document.SourceEvidenceOmissionV1{{
				Kind: document.EvidenceOmissionField, Field: "natural_provenance",
				Reason: "provider omitted provenance", UnitOrder: unitOrder,
			}}

			require.ErrorContains(t, document.ValidateSourceEvidenceV1(source), "global unit order")

			source.Omissions[0].UnitOrder = 0
			policy, err := document.NewEvidencePolicy(100_000)
			require.NoError(t, err)
			normalized, err := document.NormalizeEvidenceV1(source, policy)
			require.NoError(t, err)
			normalized.Checksum = ""
			normalized.Omissions[0].UnitOrder = unitOrder

			_, _, err = document.MarshalNormalizedEvidenceV1(normalized)
			require.ErrorContains(t, err, "global unit order")
		})
	}
}

func TestNormalizedEvidenceV1RejectsInvalidLocatorSequence(t *testing.T) {
	policy, err := document.NewEvidencePolicy(100_000)
	require.NoError(t, err)
	normalized, err := document.NormalizeEvidenceV1(syntheticSourceEvidenceV1(), policy)
	require.NoError(t, err)
	normalized.Checksum = ""
	normalized.Units[1].Locator.Start = 1
	normalized.Units[1].Locator.End = 1

	_, _, err = document.MarshalNormalizedEvidenceV1(normalized)
	require.ErrorContains(t, err, "locator sequence overlaps")
}

func TestNormalizedEvidenceV1NamespacesStructureByUnit(t *testing.T) {
	source := syntheticSourceEvidenceV1()
	source.Units[1].Text = source.Units[0].Text
	source.Units[1].Regions = append([]document.SourceEvidenceRegionV1(nil), source.Units[0].Regions...)
	source.Units[1].Tables = append([]document.SourceEvidenceTableV1(nil), source.Units[0].Tables...)
	policy, err := document.NewEvidencePolicy(100_000)
	require.NoError(t, err)

	normalized, err := document.NormalizeEvidenceV1(source, policy)
	require.NoError(t, err)

	assert.NotEqual(t, normalized.Units[0].Regions[0].ID, normalized.Units[1].Regions[0].ID)
	assert.NotEqual(t, normalized.Units[0].Tables[0].ID, normalized.Units[1].Tables[0].ID)
}

func TestNormalizedEvidenceV1EnforcesSourceBounds(t *testing.T) {
	policy, err := document.NewEvidencePolicy(100_000)
	require.NoError(t, err)

	t.Run("artifacts", func(t *testing.T) {
		normalized, err := document.NormalizeEvidenceV1(syntheticSourceEvidenceV1(), policy)
		require.NoError(t, err)
		normalized.Checksum = ""
		normalized.Artifacts = make([]document.EvidenceArtifactV1, 10_001)

		_, _, err = document.MarshalNormalizedEvidenceV1(normalized)
		require.ErrorContains(t, err, "too many artifacts")
	})

	t.Run("table dimensions", func(t *testing.T) {
		normalized, err := document.NormalizeEvidenceV1(syntheticSourceEvidenceV1(), policy)
		require.NoError(t, err)
		normalized.Checksum = ""
		normalized.Units[0].Tables[0].Rows = 1_000_001

		_, _, err = document.MarshalNormalizedEvidenceV1(normalized)
		require.ErrorContains(t, err, "table dimensions")
	})

	t.Run("NUL text", func(t *testing.T) {
		normalized, err := document.NormalizeEvidenceV1(syntheticSourceEvidenceV1(), policy)
		require.NoError(t, err)
		normalized.Checksum = ""
		normalized.Units[0].Text = "\x00"

		_, _, err = document.MarshalNormalizedEvidenceV1(normalized)
		require.ErrorContains(t, err, "NUL")
	})
}

func TestEvidenceV1RejectsOverlappingTableCells(t *testing.T) {
	source := syntheticSourceEvidenceV1()
	source.Units[0].Tables[0].Rows = 3
	source.Units[0].Tables[0].Columns = 3
	source.Units[0].Tables[0].Cells[0].RowSpan = 2
	source.Units[0].Tables[0].Cells[0].ColumnSpan = 2
	overlap := source.Units[0].Tables[0].Cells[0]
	overlap.Order = 1
	overlap.Row = 1
	overlap.Column = 1
	source.Units[0].Tables[0].Cells = append(source.Units[0].Tables[0].Cells, overlap)

	require.ErrorContains(t, document.ValidateSourceEvidenceV1(source), "overlapping cells")

	source.Units[0].Tables[0].Cells = source.Units[0].Tables[0].Cells[:1]
	policy, err := document.NewEvidencePolicy(100_000)
	require.NoError(t, err)
	normalized, err := document.NormalizeEvidenceV1(source, policy)
	require.NoError(t, err)
	normalized.Checksum = ""
	normalizedOverlap := normalized.Units[0].Tables[0].Cells[0]
	normalizedOverlap.Order = 1
	normalizedOverlap.Row = 1
	normalizedOverlap.Column = 1
	normalized.Units[0].Tables[0].Cells = append(
		normalized.Units[0].Tables[0].Cells, normalizedOverlap,
	)

	_, _, err = document.MarshalNormalizedEvidenceV1(normalized)
	require.ErrorContains(t, err, "overlapping cells")
}

func TestNormalizedEvidenceV1RejectsNoncanonicalLocatorName(t *testing.T) {
	policy, err := document.NewEvidencePolicy(100_000)
	require.NoError(t, err)
	normalized, err := document.NormalizeEvidenceV1(syntheticSourceEvidenceV1(), policy)
	require.NoError(t, err)
	normalized.Checksum = ""
	normalized.Units[0].Locator.Name = "Cafe\u0301"

	_, _, err = document.MarshalNormalizedEvidenceV1(normalized)
	require.ErrorContains(t, err, "locator name is not canonical")
}

func TestEvidenceV1RejectsExcessHeadings(t *testing.T) {
	t.Run("depth", func(t *testing.T) {
		source := syntheticSourceEvidenceV1()
		source.Units[0].HeadingPath = make([]string, 65)
		for index := range source.Units[0].HeadingPath {
			source.Units[0].HeadingPath[index] = "heading"
		}

		require.ErrorContains(t, document.ValidateSourceEvidenceV1(source), "heading depth")
	})

	t.Run("aggregate bytes", func(t *testing.T) {
		source := syntheticSourceEvidenceV1()
		source.Units[0].HeadingPath = []string{strings.Repeat("h", (1<<20)+1)}

		require.ErrorContains(t, document.ValidateSourceEvidenceV1(source), "heading bytes")
	})

	t.Run("normalized", func(t *testing.T) {
		policy, err := document.NewEvidencePolicy(100_000)
		require.NoError(t, err)
		normalized, err := document.NormalizeEvidenceV1(syntheticSourceEvidenceV1(), policy)
		require.NoError(t, err)
		normalized.Checksum = ""
		normalized.Units[0].HeadingPath = make([]string, 65)
		for index := range normalized.Units[0].HeadingPath {
			normalized.Units[0].HeadingPath[index] = "heading"
		}

		_, _, err = document.MarshalNormalizedEvidenceV1(normalized)
		require.ErrorContains(t, err, "heading depth")
	})
}

func TestSourceEvidenceV1AllowsBlankUnits(t *testing.T) {
	source := document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1,
		Completeness:    document.EvidenceComplete,
		Family:          "pdf",
		UnitKind:        document.EvidenceUnitPage,
		Units: []document.SourceEvidenceUnitV1{{
			Order: 0,
			Locator: document.SourceEvidenceLocatorV1{
				Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne,
				Start: 1, End: 1,
			},
		}},
	}

	require.NoError(t, document.ValidateSourceEvidenceV1(source))
	policy, err := document.NewEvidencePolicy(1)
	require.NoError(t, err)
	normalized, err := document.NormalizeEvidenceV1(source, policy)
	require.NoError(t, err)
	assert.Empty(t, normalized.Units[0].Text)
}

func TestNormalizeEvidenceV1AppliesCharacterLimitAfterCanonicalization(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		maxChars int
	}{
		{name: "CRLF", text: "x\r\n", maxChars: 2},
		{name: "NFC", text: "e\u0301", maxChars: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := document.SourceEvidenceV1{
				ContractVersion: document.SourceEvidenceContractV1,
				Completeness:    document.EvidenceComplete,
				Family:          "pdf",
				UnitKind:        document.EvidenceUnitPage,
				Units: []document.SourceEvidenceUnitV1{{
					Order: 0, Text: test.text,
					Locator: document.SourceEvidenceLocatorV1{
						Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne,
						Start: 1, End: 1,
					},
				}},
			}
			policy, err := document.NewEvidencePolicy(test.maxChars)
			require.NoError(t, err)

			normalized, err := document.NormalizeEvidenceV1(source, policy)
			require.NoError(t, err)
			assert.Equal(t, test.maxChars, utf8.RuneCountInString(normalized.Units[0].Text))
		})
	}
}

func TestEvidenceV1RejectsExcessGeometry(t *testing.T) {
	t.Run("boxes", func(t *testing.T) {
		source := syntheticSourceEvidenceV1()
		box := source.Units[0].Regions[0].Geometry.Boxes[0]
		source.Units[0].Regions[0].Geometry.Boxes = make([]document.EvidenceBoxV1, 10_001)
		for index := range source.Units[0].Regions[0].Geometry.Boxes {
			source.Units[0].Regions[0].Geometry.Boxes[index] = box
		}

		require.ErrorContains(t, document.ValidateSourceEvidenceV1(source), "too many boxes")
	})

	t.Run("polygons", func(t *testing.T) {
		source := syntheticSourceEvidenceV1()
		polygon := document.EvidencePolygonV1{Points: []document.EvidencePointV1{{}, {}, {}}}
		source.Units[0].Regions[0].Geometry.Polygons = make([]document.EvidencePolygonV1, 10_001)
		for index := range source.Units[0].Regions[0].Geometry.Polygons {
			source.Units[0].Regions[0].Geometry.Polygons[index] = polygon
		}

		require.ErrorContains(t, document.ValidateSourceEvidenceV1(source), "too many polygons")
	})

	t.Run("aggregate polygon points", func(t *testing.T) {
		source := syntheticSourceEvidenceV1()
		points := make([]document.EvidencePointV1, 1_001)
		source.Units[0].Regions[0].Geometry.Polygons = make([]document.EvidencePolygonV1, 100)
		for index := range source.Units[0].Regions[0].Geometry.Polygons {
			source.Units[0].Regions[0].Geometry.Polygons[index] = document.EvidencePolygonV1{Points: points}
		}

		require.ErrorContains(t, document.ValidateSourceEvidenceV1(source), "too many polygon points")
	})

	t.Run("normalized", func(t *testing.T) {
		policy, err := document.NewEvidencePolicy(100_000)
		require.NoError(t, err)
		normalized, err := document.NormalizeEvidenceV1(syntheticSourceEvidenceV1(), policy)
		require.NoError(t, err)
		normalized.Checksum = ""
		box := normalized.Units[0].Regions[0].Geometry.Boxes[0]
		normalized.Units[0].Regions[0].Geometry.Boxes = make([]document.EvidenceBoxV1, 10_001)
		for index := range normalized.Units[0].Regions[0].Geometry.Boxes {
			normalized.Units[0].Regions[0].Geometry.Boxes[index] = box
		}

		_, _, err = document.MarshalNormalizedEvidenceV1(normalized)
		require.ErrorContains(t, err, "too many boxes")
	})
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

func TestEvidenceV1EnforcesAggregateOmissionLimit(t *testing.T) {
	const documentOmissions = 50_000
	const unitOmissions = 50_001

	t.Run("source", func(t *testing.T) {
		source := syntheticSourceEvidenceV1()
		source.Completeness = document.EvidencePartial
		source.Omissions = make([]document.SourceEvidenceOmissionV1, documentOmissions)
		source.Units[0].Omissions = make([]document.SourceEvidenceOmissionV1, unitOmissions)

		require.ErrorContains(t, document.ValidateSourceEvidenceV1(source), "total omission limit")
	})

	t.Run("normalized", func(t *testing.T) {
		policy, err := document.NewEvidencePolicy(1_000)
		require.NoError(t, err)
		normalized, err := document.NormalizeEvidenceV1(syntheticSourceEvidenceV1(), policy)
		require.NoError(t, err)

		normalized.Checksum = ""
		normalized.Completeness = document.EvidencePartial
		normalized.Omissions = make([]document.EvidenceOmissionV1, documentOmissions)
		normalized.Units[0].Omissions = make([]document.EvidenceOmissionV1, unitOmissions)

		_, _, err = document.MarshalNormalizedEvidenceV1(normalized)
		require.ErrorContains(t, err, "total omission limit")
	})
}

func TestEvidenceV1RejectsDuplicateCanonicalOmissions(t *testing.T) {
	t.Run("source normalization", func(t *testing.T) {
		source := syntheticSourceEvidenceV1()
		source.Completeness = document.EvidencePartial
		source.Omissions = []document.SourceEvidenceOmissionV1{
			{
				Kind: document.EvidenceOmissionField, Field: "natural_provenance",
				Reason: "Cafe\u0301",
			},
			{
				Kind: document.EvidenceOmissionField, Field: "natural_provenance",
				Reason: "Café",
			},
		}
		policy, err := document.NewEvidencePolicy(1_000)
		require.NoError(t, err)

		_, err = document.NormalizeEvidenceV1(source, policy)
		require.ErrorContains(t, err, "duplicate canonical omission")
	})

	t.Run("normalized manifest", func(t *testing.T) {
		source := syntheticSourceEvidenceV1()
		source.Completeness = document.EvidencePartial
		source.Omissions = []document.SourceEvidenceOmissionV1{{
			Kind: document.EvidenceOmissionField, Field: "natural_provenance", Reason: "Café",
		}}
		policy, err := document.NewEvidencePolicy(1_000)
		require.NoError(t, err)
		normalized, err := document.NormalizeEvidenceV1(source, policy)
		require.NoError(t, err)

		normalized.Checksum = ""
		normalized.Omissions = append(normalized.Omissions, normalized.Omissions[0])

		_, _, err = document.MarshalNormalizedEvidenceV1(normalized)
		require.ErrorContains(t, err, "duplicate canonical omission")
	})
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
