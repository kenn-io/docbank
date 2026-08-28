package retrieval

import (
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateExpansionVariantsRejectsMalformedProviderOutput(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		variants []string
	}{
		{name: "empty", variants: []string{" "}},
		{name: "duplicate", variants: []string{"expanded", "expanded"}},
		{name: "original query", variants: []string{"original"}},
		{name: "over limit", variants: []string{"one", "two", "three"}},
		{name: "oversized text", variants: []string{strings.Repeat("x", maxProviderQueryBytes+1)}},
		{name: "invalid UTF-8", variants: []string{string([]byte{0xff})}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := validateExpansionVariants("original", test.variants, 2)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "original")
			assert.NotContains(t, err.Error(), "expanded")
		})
	}
}

func TestValidateExpansionVariantsSortsBoundedVariants(t *testing.T) {
	t.Parallel()

	variants, err := validateExpansionVariants("original", []string{"zeta", "alpha"}, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"alpha", "zeta"}, variants)
}

func TestValidateRerankingScoresRequiresOneFiniteScoreForEveryAllowedCandidate(t *testing.T) {
	t.Parallel()

	first := DocumentIdentity{VaultID: "vault", NodeID: 1, ContentVersionID: "version-one"}
	second := DocumentIdentity{VaultID: "vault", NodeID: 2, ContentVersionID: "version-two"}
	unknown := DocumentIdentity{VaultID: "vault", NodeID: 3, ContentVersionID: "version-three"}
	for _, test := range []struct {
		name   string
		scores []RerankScore
	}{
		{name: "missing", scores: []RerankScore{{Document: first, Score: 1}}},
		{name: "duplicate", scores: []RerankScore{{Document: first, Score: 1}, {Document: first, Score: 2}}},
		{name: "unknown", scores: []RerankScore{{Document: first, Score: 1}, {Document: unknown, Score: 2}}},
		{name: "not a number", scores: []RerankScore{{Document: first, Score: 1}, {Document: second, Score: math.NaN()}}},
		{name: "infinite", scores: []RerankScore{{Document: first, Score: 1}, {Document: second, Score: math.Inf(1)}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validateRerankScores([]DocumentIdentity{first, second}, test.scores)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "version-one")
			assert.NotContains(t, err.Error(), "version-two")
		})
	}
}
