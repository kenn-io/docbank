package retrieval

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

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

func FuzzRerankingReceiptProfileIdentity(f *testing.F) {
	f.Add("synthetic-reranker", uint8(0))
	f.Add("模型/ reranker:v2\n", uint8(1))
	f.Add(strings.Repeat("r", 4097), uint8(2))
	f.Fuzz(func(t *testing.T, profileID string, choice uint8) {
		if profileID == "" {
			t.Skip() // Enabled reranking requires a nonempty profile ID.
		}
		searcher := &Searcher{reranking: RerankingConfig{Enabled: true,
			Profile:  RerankingProfile{ID: profileID, MaxCandidates: 1},
			Provider: &stageReranker{}, Authorizer: &stageAuthorizer{},
			Deadline: time.Second, FailurePolicy: ProviderFailureDegrade}}
		report := Report{Results: []Result{{Document: DocumentIdentity{NodeID: 1}, Excerpt: "evidence"}}}
		wantID := ""
		switch choice % 4 {
		case 0:
			wantID = profileID
		case 1:
			report.Results = nil
		case 2:
			searcher.reranking.Authorizer = &stageAuthorizer{err: errors.New("denied")}
		case 3:
			searcher.reranking.Enabled = false
		}
		_, receipt, _, err := searcher.rerank(context.Background(), Query{Text: "question"}, report)
		if err != nil {
			t.Fatal(err)
		}
		if receipt == nil {
			if choice%4 != 3 {
				t.Fatal("missing reranking receipt")
			}
			return
		}
		if receipt.ProfileID != wantID {
			t.Fatalf("receipt identity = %q, want %q (outcome %s)", receipt.ProfileID, wantID, receipt.Outcome)
		}
	})
}
