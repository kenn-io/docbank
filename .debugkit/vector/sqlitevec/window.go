package sqlitevec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"

	"go.kenn.io/kit/search/sqlquery"
	"go.kenn.io/kit/vector"
)

// Window contains current candidates and the score of the next raw neighbor.
// HasProbe means more raw neighbors exist, not that more eligible results exist.
// Stale or deleted documents can occupy every slot, leaving Hits empty even
// when HasProbe is true. ProbeScore never belongs to a returned hit.
type Window[K comparable] struct {
	Hits       []vector.Hit[K]
	ProbeScore float32
	HasProbe   bool
}

// CandidateQuery describes a bounded chunk search in a single generation.
type CandidateQuery struct {
	// CandidateLimit bounds raw neighbors before freshness and source filters.
	CandidateLimit int
	// ResultLimit bounds eligible results within that window. Zero returns all
	// eligible candidates; it does not expand the raw candidate window.
	ResultLimit int
	// ExtraSourceCols follow doc_key, chunk_index, revision and distance.
	ExtraSourceCols []sqlquery.Column
	// SourcePredicate runs after the raw KNN limit but before ResultLimit and
	// is passed to SQLite unchanged. EXISTS predicates can constrain related
	// rows without multiplying candidates.
	SourcePredicate sqlquery.Predicate
}

// BuildCandidateQuery returns a composable SELECT with columns doc_key,
// chunk_index, revision, distance, then the requested source columns. Distance
// is the raw neighbor distance. Callers convert it with ScoreFromDistance
// before storing a score. Equal distances use vector rowid order within the
// retrieved window. Outer queries must specify their own ordering.
//
// BuildCandidateQuery reads gen's layout on db, the caller's handle, so the
// lookup and the returned query can share one transaction. It does not use
// the store's own connection. Execute the returned query on the same handle.
// A short result does not establish exhaustion; QueryGenerationWindow exposes
// the raw boundary when a caller needs to expand the candidate window.
func (s *Store[K, G]) BuildCandidateQuery(ctx context.Context, db sqlquery.Queryer, gen G, query vector.Vector, q CandidateQuery) (sqlquery.Query, error) {
	if q.CandidateLimit <= 0 {
		return sqlquery.Query{}, errors.New("sqlitevec: candidate limit must be positive")
	}
	if q.ResultLimit < 0 || q.ResultLimit > q.CandidateLimit {
		return sqlquery.Query{}, errors.New("sqlitevec: result limit must be between zero and candidate limit")
	}
	predicate := strings.TrimSpace(q.SourcePredicate.SQL)
	if predicate == "" && len(q.SourcePredicate.Args) != 0 {
		return sqlquery.Query{}, errors.New("sqlitevec: source predicate arguments require SQL")
	}
	aliases := map[string]bool{"doc_key": true, "chunk_index": true, "revision": true, "score": true, "distance": true, "vec_rowid": true}
	var projection, columns strings.Builder
	for _, col := range q.ExtraSourceCols {
		alias := strings.ToLower(col.As)
		if !identifierPattern.MatchString(col.Name) || !identifierPattern.MatchString(col.As) || aliases[alias] {
			return sqlquery.Query{}, fmt.Errorf("sqlitevec: invalid or conflicting source column %q AS %q", col.Name, col.As)
		}
		aliases[alias] = true
		fmt.Fprintf(&projection, ", d.\"%s\" AS \"%s\"", col.Name, col.As)
		fmt.Fprintf(&columns, ", \"%s\"", col.As)
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
	text := s.candidateCTEs(ordinal, expr, projection.String(), predicate, false)
	text += " SELECT doc_key, chunk_index, revision, distance" + columns.String() + " FROM candidates ORDER BY distance, vec_rowid"
	args := append([]any{value, q.CandidateLimit, ordinal}, q.SourcePredicate.Args...)
	if q.ResultLimit > 0 {
		text += " LIMIT ?"
		args = append(args, q.ResultLimit)
	}
	rawSQL := fmt.Sprintf(`SELECT count(*) FROM (SELECT rowid FROM %s WHERE embedding MATCH %s ORDER BY distance LIMIT ?)`, s.vecTable(ordinal), expr)
	rawArgs := []any{value, q.CandidateLimit}
	limit := q.CandidateLimit
	return sqlquery.Query{
		SQL: text, Args: args,
		RawWindow: func(ctx context.Context, db sqlquery.Queryer) (bool, error) {
			rows, err := db.QueryContext(ctx, rawSQL, rawArgs...)
			if err != nil {
				return false, fmt.Errorf("sqlitevec: probe raw window: %w", err)
			}
			defer func() { _ = rows.Close() }()
			var n int
			if rows.Next() {
				if err := rows.Scan(&n); err != nil {
					return false, fmt.Errorf("sqlitevec: scan raw window: %w", err)
				}
			}
			if err := rows.Err(); err != nil {
				return false, fmt.Errorf("sqlitevec: read raw window: %w", err)
			}
			return n >= limit, nil
		},
	}, nil
}

// QueryGenerationWindow returns at most limit current candidates plus a raw
// boundary probe, using one materialized KNN scan. Revision, score and probe
// come from the same statement. limit must be positive.
func (s *Store[K, G]) QueryGenerationWindow(ctx context.Context, gen G, query vector.Vector, limit int) (Window[K], error) {
	if limit <= 0 {
		return Window[K]{}, errors.New("sqlitevec: window limit must be positive")
	}
	if limit == math.MaxInt {
		return Window[K]{}, errors.New("sqlitevec: window limit too large")
	}
	ordinal, expr, value, err := s.prepareQuery(ctx, gen, query)
	if err != nil {
		return Window[K]{}, err
	}
	text := s.candidateCTEs(ordinal, expr, "", "", true) + `
SELECT doc_key, chunk_index, revision, distance, 0 AS is_probe, vec_rowid FROM candidates
UNION ALL
SELECT NULL, NULL, NULL, distance, 1, rowid FROM ranked WHERE raw_rank = ?
ORDER BY is_probe, distance, vec_rowid`
	rows, err := s.db.QueryContext(ctx, text, value, limit+1, ordinal, limit, limit)
	if err != nil {
		return Window[K]{}, fmt.Errorf("query generation window: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out Window[K]
	for rows.Next() {
		var doc sql.Null[K]
		var chunk sql.Null[int]
		var candidate vector.Hit[K]
		var distance float64
		var isProbe bool
		var rowid int64
		if err := rows.Scan(&doc, &chunk, &candidate.Revision, &distance, &isProbe, &rowid); err != nil {
			return Window[K]{}, fmt.Errorf("scan generation window: %w", err)
		}
		score, err := scoreFromDistance(distance)
		if err != nil {
			return Window[K]{}, err
		}
		if isProbe {
			out.ProbeScore, out.HasProbe = score, true
		} else {
			candidate.Doc, candidate.ChunkIndex = doc.V, chunk.V
			candidate.Score = score
			out.Hits = append(out.Hits, candidate)
		}
	}
	if err := rows.Err(); err != nil {
		return Window[K]{}, fmt.Errorf("read generation window: %w", err)
	}
	return out, nil
}

func (s *Store[K, G]) prepareQuery(ctx context.Context, gen G, query vector.Vector) (int64, string, any, error) {
	ordinal, dimension, err := s.lookupGeneration(ctx, gen)
	if err != nil {
		return 0, "", nil, err
	}
	if len(query) != dimension {
		return 0, "", nil, fmt.Errorf("query has %d dimensions, generation expects %d", len(query), dimension)
	}
	expr, value, err := vectorValue(query)
	if err != nil {
		return 0, "", nil, fmt.Errorf("serialize query: %w", err)
	}
	return ordinal, expr, value, nil
}

// candidateCTEs is shared by ordinary, composable and probed searches. Keep
// KNN materialized and on the outer side of the chunk-map join (see the plan
// regression test). A probe ranks the raw window before any source joins.
func (s *Store[K, G]) candidateCTEs(ordinal int64, expr, projection, predicate string, probe bool) string {
	text := fmt.Sprintf(`WITH knn AS MATERIALIZED (
    SELECT rowid, distance FROM %s WHERE embedding MATCH %s ORDER BY distance LIMIT ?
)`, s.vecTable(ordinal), expr)
	from := "knn"
	if probe {
		text += `, ranked AS MATERIALIZED (
    SELECT rowid, distance, row_number() OVER (ORDER BY distance, rowid) - 1 AS raw_rank FROM knn
)`
		from = "ranked knn"
	}
	text += fmt.Sprintf(`, candidates AS (
    SELECT c.doc_key, c.chunk_index, stamp.revision, knn.distance, knn.rowid AS vec_rowid%s
      FROM %s CROSS JOIN %s c ON c.ordinal = ? AND c.vec_rowid = knn.rowid
      JOIN %s d ON d.%s = c.doc_key
      LEFT JOIN %s stamp ON stamp.ordinal = c.ordinal AND stamp.doc_key = c.doc_key
     WHERE %s`, projection, from, s.chunksTable(), s.schema.DocsTable, s.schema.IDColumn, s.stampsTable(), s.coveredPredicate("d", "stamp"))
	if probe {
		text += " AND knn.raw_rank < ?"
	}
	if predicate != "" {
		text += " AND (" + predicate + ")"
	}
	return text + ")"
}

// lookupGenerationOn reads gen's layout through a caller's handle, which may
// be a transaction the store does not own.
func (s *Store[K, G]) lookupGenerationOn(ctx context.Context, db sqlquery.Queryer, gen G) (int64, int, error) {
	if db == nil {
		return 0, 0, errors.New("sqlitevec: query handle is required")
	}
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`SELECT ordinal, dimension FROM %s WHERE gen_key = ?`, s.generationsTable()), gen)
	if err != nil {
		return 0, 0, fmt.Errorf("sqlitevec: lookup generation %v: %w", gen, err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, 0, fmt.Errorf("sqlitevec: lookup generation %v: %w", gen, err)
		}
		return 0, 0, fmt.Errorf("generation %v: %w", gen, ErrGenerationNotFound)
	}
	var ordinal int64
	var dimension int
	if err := rows.Scan(&ordinal, &dimension); err != nil {
		return 0, 0, fmt.Errorf("sqlitevec: lookup generation %v: %w", gen, err)
	}
	return ordinal, dimension, nil
}
