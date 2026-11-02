package clickhouse_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/search/clickhouse"
	"go.kenn.io/kit/search/sqlquery"
)

func TestBuildTextUsesNativePredicatesWithoutARank(t *testing.T) {
	q, err := clickhouse.BuildText(clickhouse.TextRequest{
		SourceTable: "docs", SourceKey: "id", TextColumn: "body",
		Match: clickhouse.MatchToken, Text: "かなを探します。",
		RevisionColumn:  "revision",
		SourcePredicate: sqlquery.Predicate{SQL: "d.tenant = ?", Args: []any{"t1"}},
		ExtraSourceCols: []sqlquery.Column{{Name: "title", As: "title"}},
		CandidateLimit:  5,
	})
	require.NoError(t, err)
	assert.Contains(t, q.SQL, "hasToken(d.`body`, ?)")
	assert.NotContains(t, q.SQL, "asciiCJK")
	assert.NotContains(t, q.SQL, "bm25")
	assert.Contains(t, q.SQL, "d.`revision` AS revision")
	assert.Contains(t, q.SQL, "ORDER BY doc_key ASC LIMIT ?")
	assert.Equal(t, []any{"かなを探します。", "t1", 5}, q.Args)
	assert.Less(t, strings.Index(q.SQL, " WHERE "), strings.Index(q.SQL, " ORDER BY "))

	tokens, err := clickhouse.BuildText(clickhouse.TextRequest{
		SourceTable: "docs", SourceKey: "id", TextColumn: "body",
		Match: clickhouse.MatchAll, Tokens: []string{"か", "な"}, CandidateLimit: 3,
	})
	require.NoError(t, err)
	assert.Contains(t, tokens.SQL, "hasAllTokens(d.`body`, ?)")
	_, err = clickhouse.BuildText(clickhouse.TextRequest{
		SourceTable: "docs", SourceKey: "id", TextColumn: "body",
		Match: clickhouse.MatchToken, Tokens: []string{"か"}, CandidateLimit: 1,
	})
	require.Error(t, err)
	assert.Contains(t, clickhouse.TokenizersQuery().SQL, "system.tokenizers")

	substring, err := clickhouse.BuildText(clickhouse.TextRequest{
		SourceTable: "docs", SourceKey: "id", TextColumn: "body",
		Match: clickhouse.MatchSubstring, Text: "かな", CandidateLimit: 10,
	})
	require.NoError(t, err)
	assert.Contains(t, substring.SQL, "positionUTF8(d.`body`, ?) > 0")
	assert.Equal(t, []any{"かな", 10}, substring.Args)
	_, err = clickhouse.BuildText(clickhouse.TextRequest{
		SourceTable: "docs", SourceKey: "id", TextColumn: "body",
		Match: clickhouse.MatchSubstring, Tokens: []string{"か"}, CandidateLimit: 1,
	})
	require.Error(t, err)
}

func TestBuildVectorKeepsCandidateLimitAndProbeSetting(t *testing.T) {
	q, err := clickhouse.BuildVector(clickhouse.VectorRequest{
		SourceTable: "docs", SourceKey: "id", VectorColumn: "embedding",
		Distance: clickhouse.DistanceCosine, Query: []float32{1, 0},
		RevisionColumn: "revision", CandidateLimit: 4, Approximate: true, Probes: 64,
		SourcePredicate: sqlquery.Predicate{SQL: "d.tenant = ?", Args: []any{"t1"}},
	})
	require.NoError(t, err)
	assert.Contains(t, q.SQL, "1 - (cosineDistance(d.`embedding`, ?)) AS score")
	assert.Contains(t, q.SQL, "ORDER BY cosineDistance(d.`embedding`, ?) ASC LIMIT ?")
	assert.Contains(t, q.SQL, "SETTINGS hnsw_candidate_list_size_for_search = 64")
	assert.Equal(t, []any{[]float32{1, 0}, "t1", []float32{1, 0}, 4}, q.Args)

	_, err = clickhouse.BuildVector(clickhouse.VectorRequest{
		SourceTable: "docs", SourceKey: "id", VectorColumn: "embedding",
		Query: []float32{1}, CandidateLimit: 1, Probes: 8,
	})
	require.Error(t, err)
}

func TestBuildTextRejectsEmptyTokenList(t *testing.T) {
	_, err := clickhouse.BuildText(clickhouse.TextRequest{
		SourceTable: "docs", SourceKey: "id", TextColumn: "body",
		Tokens: []string{}, CandidateLimit: 1,
	})
	require.Error(t, err)

	tokens, err := clickhouse.BuildText(clickhouse.TextRequest{
		SourceTable: "docs", SourceKey: "id", TextColumn: "body",
		Tokens: []string{"a"}, CandidateLimit: 1,
	})
	require.NoError(t, err)
	assert.Contains(t, tokens.SQL, "hasAllTokens(d.`body`, ?)")
	assert.Equal(t, []any{[]string{"a"}, 1}, tokens.Args)

	text, err := clickhouse.BuildText(clickhouse.TextRequest{
		SourceTable: "docs", SourceKey: "id", TextColumn: "body",
		Text: "a", CandidateLimit: 1,
	})
	require.NoError(t, err)
	assert.Contains(t, text.SQL, "hasAllTokens(d.`body`, ?)")
	assert.Equal(t, []any{"a", 1}, text.Args)
}
