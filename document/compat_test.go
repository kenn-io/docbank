package document

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/internal/compattest"
)

type normalizationCompatibilityCase struct {
	Name             string             `json:"name"`
	MaxDocumentChars int                `json:"max_document_chars"`
	Source           SourceDocument     `json:"source"`
	Expected         NormalizedDocument `json:"expected"`
}

func TestNormalizeDocumentVersionThreePreservesVersionTwoEvidence(t *testing.T) {
	bundle, _, err := compattest.Load()
	require.NoError(t, err)
	section, ok := bundle.Sections["normalization_v2"]
	require.True(t, ok)
	assert.Equal(t, "go.kenn.io/docbank/document", section.Owner)

	var cases []normalizationCompatibilityCase
	// Public evidence types intentionally do not define a JSON serialization API.
	// The frozen test bundle uses their Go field names from the baseline.
	//nolint:musttag
	require.NoError(t, json.Unmarshal(section.Cases, &cases))
	require.Len(t, cases, 2)
	for _, testCase := range cases {
		t.Run(testCase.Name, func(t *testing.T) {
			policy, err := NewNormalizePolicy(testCase.MaxDocumentChars)
			require.NoError(t, err)
			actual, err := NormalizeDocument(testCase.Source, policy)
			require.NoError(t, err)
			require.NoError(t, ValidateNormalizedDocument(actual))
			assert.Equal(t, 3, actual.PolicyVersion)
			assert.Equal(t, testCase.Expected.Family, actual.Family)
			assert.Equal(t, testCase.Expected.UnitKind, actual.UnitKind)
			expectedUnits := append([]NormalizedUnit(nil), testCase.Expected.Units...)
			actualUnits := append([]NormalizedUnit(nil), actual.Units...)
			for index := range actualUnits {
				assert.NotEqual(t, expectedUnits[index].Checksum, actualUnits[index].Checksum)
				expectedUnits[index].Checksum = ""
				actualUnits[index].Checksum = ""
			}
			assert.Equal(t, expectedUnits, actualUnits)
			expectedChunks := append([]Chunk(nil), testCase.Expected.Chunks...)
			actualChunks := append([]Chunk(nil), actual.Chunks...)
			for index := range actualChunks {
				assert.NotEqual(t, expectedChunks[index].Checksum, actualChunks[index].Checksum)
				expectedChunks[index].Checksum = ""
				actualChunks[index].Checksum = ""
			}
			assert.Equal(t, expectedChunks, actualChunks)
			assert.Equal(t, testCase.Expected.Truncated, actual.Truncated)
			assert.NotEqual(t, testCase.Expected.Checksum, actual.Checksum)
		})
	}
}
