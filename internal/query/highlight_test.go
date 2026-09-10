package query

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHighlightSetIdentityVectors(t *testing.T) {
	for _, vector := range loadIdentityFixture(t).HighlightSets {
		t.Run(vector.Name, func(t *testing.T) {
			value, err := ParseHighlightSet([]byte(vector.InputJSON))
			require.NoError(t, err)
			encoded, err := CanonicalHighlightSet(value)
			require.NoError(t, err)
			assert.Equal(t, vector.CanonicalUTF8, string(encoded))
			fingerprint, err := HighlightSetFingerprint(value)
			require.NoError(t, err)
			assert.Equal(t, vector.Fingerprint, fingerprint)
		})
	}
}

func TestHighlightSetRejectsMalformedOrQueryPayloads(t *testing.T) {
	tests := map[string]string{
		"query kind confusion": `{"v":1,"text":"query"}`,
		"missing terms":        `{"v":1}`,
		"null terms":           `{"v":1,"terms":null}`,
		"empty terms":          `{"v":1,"terms":[]}`,
		"duplicate text":       `{"v":1,"terms":[{"text":"same","color":"#ffffff"},{"text":"same","color":"#000000"}]}`,
		"uppercase color":      `{"v":1,"terms":[{"text":"term","color":"#ABCDEF"}]}`,
		"regex member":         `{"v":1,"terms":[{"text":"term","color":"#abcdef","regex":true}]}`,
		"null term text":       `{"v":1,"terms":[{"text":null,"color":"#abcdef"}]}`,
		"lone surrogate":       `{"v":1,"terms":[{"text":"\ud800","color":"#abcdef"}]}`,
	}
	for _, testCase := range loadIdentityFixture(t).InvalidHighlightSets {
		tests[testCase.Name] = testCase.JSONText
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseHighlightSet([]byte(raw))
			require.Error(t, err)
		})
	}
}

func TestHighlightSetEnforcesTermBounds(t *testing.T) {
	fixture := loadIdentityFixture(t)
	terms := strings.Repeat(`{"text":"x","color":"#abcdef"},`, fixture.Bounds.HighlightMaxTerms) + `{"text":"last","color":"#abcdef"}`
	_, err := ParseHighlightSet([]byte(`{"v":1,"terms":[` + terms + `]}`))
	require.Error(t, err)

	_, err = ParseHighlightSet([]byte(`{"v":1,"terms":[{"text":"` + strings.Repeat("😀", fixture.Bounds.HighlightMaxTermScalars+1) + `","color":"#abcdef"}]}`))
	require.Error(t, err)

	_, err = ParseHighlightSet([]byte(`{"v":1,"terms":[{"text":"","color":"#abcdef"}]}`))
	require.Error(t, err)

	maxTerm := `{"v":1,"terms":[{"text":"` + strings.Repeat("x", fixture.Bounds.HighlightMaxTermScalars) + `","color":"#abcdef"}]}`
	_, err = ParseHighlightSet([]byte(maxTerm))
	require.NoError(t, err)

	maxTerms := make([]string, fixture.Bounds.HighlightMaxTerms)
	oversizedTerms := make([]string, fixture.Bounds.HighlightMaxTerms)
	for index := range maxTerms {
		maxTerms[index] = fmt.Sprintf(`{"text":"%02d","color":"#abcdef"}`, index)
		oversizedTerms[index] = fmt.Sprintf(`{"text":"%s%02d","color":"#abcdef"}`,
			strings.Repeat("😀", fixture.Bounds.OversizedHighlightAstralScalars), index)
	}
	_, err = ParseHighlightSet([]byte(`{"v":1,"terms":[` + strings.Join(maxTerms, ",") + `]}`))
	require.NoError(t, err)
	_, err = ParseHighlightSet([]byte(`{"v":1,"terms":[` + strings.Join(oversizedTerms, ",") + `]}`))
	require.Error(t, err)
}
