package postgres_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/search/postgres"
	"go.kenn.io/kit/search/sqlquery"
)

func TestBuildLexicalPlacesFilterBeforeLimit(t *testing.T) {
	q, err := postgres.BuildLexical(postgres.LexicalRequest{
		Mapping: postgres.LexicalMapping{
			SourceTable: "docs",
			SourceKey:   "id",
			Vector:      "setweight(to_tsvector('simple', d.title), 'A') || setweight(to_tsvector('simple', d.body), 'B')",
		},
		Config:          "simple",
		Query:           postgres.QueryPlain,
		Text:            "alpha",
		Rank:            postgres.RankCover,
		RevisionColumn:  "revision",
		SourcePredicate: sqlquery.Predicate{SQL: "d.tenant = ?", Args: []any{"t1"}},
		ExtraSourceCols: []sqlquery.Column{{Name: "title", As: "title"}},
		CandidateLimit:  4,
	})
	require.NoError(t, err)
	assert.Contains(t, q.SQL, "ts_rank_cd(")
	assert.Contains(t, q.SQL, "setweight(to_tsvector('simple', d.title), 'A')")
	assert.Contains(t, q.SQL, `d."revision" AS revision`)
	assert.Contains(t, q.SQL, `d."title" AS "title"`)
	where := strings.Index(q.SQL, " WHERE ")
	order := strings.Index(q.SQL, " ORDER BY ")
	limit := strings.Index(q.SQL, " LIMIT ")
	require.Less(t, where, order)
	require.Less(t, order, limit)
	assert.Contains(t, q.SQL, "d.tenant = $3")
	assert.Equal(t, []any{"simple", "alpha", "t1", 4}, q.Args)
	assert.Contains(t, q.SQL, "plainto_tsquery($1::regconfig, $2)")
}

func TestBuildLexicalAcceptsCallerTSQuery(t *testing.T) {
	q, err := postgres.BuildLexical(postgres.LexicalRequest{
		Mapping: postgres.LexicalMapping{SourceTable: "docs", SourceKey: "id", Vector: "d.body_tsv"},
		TSQuery: "phraseto_tsquery('simple', ?)", TSQueryArgs: []any{"か な"}, CandidateLimit: 2,
	})
	require.NoError(t, err)
	assert.Contains(t, q.SQL, "phraseto_tsquery('simple', $1)")
	assert.Equal(t, []any{"か な", 2}, q.Args)
	_, err = postgres.BuildLexical(postgres.LexicalRequest{
		Mapping: postgres.LexicalMapping{SourceTable: "docs", SourceKey: "id", Vector: "d.body_tsv"},
		Text:    "alpha", TSQuery: "d.body_tsv", CandidateLimit: 1,
	})
	require.Error(t, err)
}

func TestBuildVectorFiltersBeforeLimitAndKeepsRevision(t *testing.T) {
	q, err := postgres.BuildVector(postgres.VectorRequest{
		Mapping:         postgres.VectorMapping{SourceTable: "docs", SourceKey: "id", VectorColumn: "embedding"},
		Distance:        postgres.DistanceCosine,
		Query:           "[1,0]",
		RevisionColumn:  "revision",
		SourcePredicate: sqlquery.Predicate{SQL: "d.tenant = ?", Args: []any{"t1"}},
		CandidateLimit:  1,
	})
	require.NoError(t, err)
	assert.Contains(t, q.SQL, `1 - (d."embedding" <=> $1::vector) AS score`)
	assert.Contains(t, q.SQL, `ORDER BY d."embedding" <=> $1::vector, doc_key ASC LIMIT $3`)
	assert.Contains(t, q.SQL, "d.tenant = $2")
	assert.Less(t, strings.Index(q.SQL, " WHERE "), strings.Index(q.SQL, " ORDER BY "))
	assert.Equal(t, []any{"[1,0]", "t1", 1}, q.Args)
	_, err = postgres.BuildVector(postgres.VectorRequest{
		Mapping: postgres.VectorMapping{SourceTable: "docs", SourceKey: "id", VectorColumn: "embedding"},
		Query:   "  ", CandidateLimit: 1,
	})
	require.Error(t, err)
}

func TestBuildersRejectAPlaceholderArgumentMismatch(t *testing.T) {
	_, err := postgres.BuildLexical(postgres.LexicalRequest{
		Mapping:         postgres.LexicalMapping{SourceTable: "docs", SourceKey: "id", Vector: "d.body_tsv"},
		Text:            "alpha",
		SourcePredicate: sqlquery.Predicate{SQL: "d.tenant = ? AND d.kind = ?", Args: []any{"t1"}},
		CandidateLimit:  1,
	})
	require.ErrorContains(t, err, "source predicate has 2 placeholders and 1 args")

	_, err = postgres.BuildLexical(postgres.LexicalRequest{
		Mapping:        postgres.LexicalMapping{SourceTable: "docs", SourceKey: "id", Vector: "d.body_tsv"},
		TSQuery:        "to_tsquery('simple', ?)",
		CandidateLimit: 1,
	})
	require.ErrorContains(t, err, "tsquery has 1 placeholders and 0 args")

	_, err = postgres.BuildVector(postgres.VectorRequest{
		Mapping:         postgres.VectorMapping{SourceTable: "docs", SourceKey: "id", VectorColumn: "embedding"},
		Query:           "[1,0]",
		SourcePredicate: sqlquery.Predicate{SQL: "d.tenant = 'x'", Args: []any{"extra"}},
		CandidateLimit:  1,
	})
	require.ErrorContains(t, err, "source predicate has 0 placeholders and 1 args")
}
