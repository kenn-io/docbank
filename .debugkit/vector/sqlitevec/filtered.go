package sqlitevec

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/kit/search/sqlquery"
	"go.kenn.io/kit/vector"
)

// FilteredQuery describes an exact search over current, eligible source rows.
type FilteredQuery struct {
	// ResultLimit is a positive bound on returned chunks, or groups when
	// GroupColumn is set. It is applied after freshness, source filtering,
	// and grouping. There is no global nearest-neighbor candidate window.
	ResultLimit int
	// SourcePredicate filters the source table aliased as d. SQL is trusted;
	// user values belong in Args. The predicate runs before distance ranking.
	SourcePredicate sqlquery.Predicate
	// GroupColumn optionally names one source column whose equal values form
	// a group. The closest chunk represents each group, retaining its document
	// and chunk identity. NULL values form one group, as in SQL GROUP BY.
	GroupColumn string
}

// BuildFilteredQuery returns doc_key, chunk_index, revision, and distance in
// ascending cosine distance order, breaking ties by document key and chunk
// index. Filtering and optional grouping precede ResultLimit, so ineligible
// documents and long groups cannot consume the result window.
//
// This exact query computes distances for all eligible, current chunks. Its
// cost grows with that set rather than ResultLimit. Execute with a bounded
// context. Use BuildCandidateQuery when a bounded raw KNN window is desired.
//
// Generation lookup and execution use the caller's handle, which can be one
// transaction. Execute the returned query on the same handle. Outer queries
// must specify their own ordering and preserve argument order.
func (s *Store[K, G]) BuildFilteredQuery(ctx context.Context, db sqlquery.Queryer, gen G, query vector.Vector, q FilteredQuery) (sqlquery.Query, error) {
	if q.ResultLimit <= 0 {
		return sqlquery.Query{}, errors.New("sqlitevec: filtered result limit must be positive")
	}
	if q.GroupColumn != "" && !identifierPattern.MatchString(q.GroupColumn) {
		return sqlquery.Query{}, errors.New("sqlitevec: invalid group column")
	}
	predicate := strings.TrimSpace(q.SourcePredicate.SQL)
	if predicate == "" && len(q.SourcePredicate.Args) != 0 {
		return sqlquery.Query{}, errors.New("sqlitevec: source predicate arguments require SQL")
	}
	ordinal, dimension, err := s.lookupGenerationOn(ctx, db, gen)
	if err != nil {
		return sqlquery.Query{}, err
	}
	if len(query) != dimension {
		return sqlquery.Query{}, fmt.Errorf("query has %d dimensions, generation expects %d", len(query), dimension)
	}
	expr, value, err := vectorValue(query)
	if err != nil {
		return sqlquery.Query{}, fmt.Errorf("serialize query: %w", err)
	}
	group := ""
	if q.GroupColumn != "" {
		group = fmt.Sprintf(`, d."%s" AS group_key`, q.GroupColumn)
	}
	text := fmt.Sprintf(`WITH eligible AS MATERIALIZED (
    SELECT c.doc_key, c.chunk_index, stamp.revision, c.vec_rowid%s
      FROM %s c
      JOIN %s d ON d.%s = c.doc_key
      LEFT JOIN %s stamp ON stamp.ordinal = c.ordinal AND stamp.doc_key = c.doc_key
     WHERE c.ordinal = ? AND %s`, group, s.chunksTable(), s.schema.DocsTable,
		s.schema.IDColumn, s.stampsTable(), s.coveredPredicate("d", "stamp"))
	args := []any{ordinal}
	if predicate != "" {
		text += " AND (" + predicate + ")"
		args = append(args, q.SourcePredicate.Args...)
	}
	text += fmt.Sprintf(`
), scored AS MATERIALIZED (
    SELECT e.*, vec_distance_cosine(v.embedding, %s) AS distance
      FROM eligible e CROSS JOIN %s v ON v.rowid = e.vec_rowid
)`, expr, s.vecTable(ordinal))
	args = append(args, value)
	from := "scored"
	if q.GroupColumn != "" {
		text += `, ranked AS (
    SELECT *, row_number() OVER (PARTITION BY group_key ORDER BY distance, doc_key, chunk_index) AS group_rank
      FROM scored
)`
		from = "ranked WHERE group_rank = 1"
	}
	text += " SELECT doc_key, chunk_index, revision, distance FROM " + from + " ORDER BY distance, doc_key, chunk_index LIMIT ?"
	args = append(args, q.ResultLimit)
	return sqlquery.Query{SQL: text, Args: args}, nil
}
