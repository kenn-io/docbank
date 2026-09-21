package eval

import (
	"context"
	"slices"
	"strings"
	"unicode/utf8"
)

const maxRerankExcerptBytes = 4 << 10

// RerankingRunner is the optional evaluator capability for a named reranker.
// Runner.Search remains the base retrieval contract for existing callers.
type RerankingRunner interface {
	Runner
	Rerank(ctx context.Context, system System, query string, candidates []Document) (RerankResult, error)
}

// RerankResult contains one score per candidate and usage for the rerank call.
type RerankResult struct {
	Scores []float64
	Usage  Usage
}

func boundedExcerpt(value string) string {
	value = strings.ToValidUTF8(value, "")
	if len(value) <= maxRerankExcerptBytes {
		return value
	}
	end := maxRerankExcerptBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

func reorderRanking(ranking []string, scores []float64, prefixCount int) []string {
	type scoredID struct {
		id    string
		score float64
	}
	prefix := make([]scoredID, prefixCount)
	for index := range prefixCount {
		prefix[index] = scoredID{id: ranking[index], score: scores[index]}
	}
	slices.SortStableFunc(prefix, func(left, right scoredID) int {
		switch {
		case left.score > right.score:
			return -1
		case left.score < right.score:
			return 1
		default:
			return 0
		}
	})
	ordered := make([]string, 0, len(ranking))
	for _, candidate := range prefix {
		ordered = append(ordered, candidate.id)
	}
	ordered = append(ordered, ranking[prefixCount:]...)
	return ordered
}
