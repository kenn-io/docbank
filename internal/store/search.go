package store

import (
	"context"
	"fmt"
	"strings"
)

// SearchHit is a search result with its display path.
type SearchHit struct {
	Node Node
	Path string
}

// ftsQuery converts free-form user input into a safe FTS5 query: each
// whitespace-separated term becomes a quoted prefix term. Embedded double
// quotes are doubled per FTS5 string syntax.
func ftsQuery(input string) string {
	var terms []string
	for t := range strings.FieldsSeq(input) {
		t = strings.ReplaceAll(t, `"`, `""`)
		terms = append(terms, `"`+t+`"*`)
	}
	return strings.Join(terms, " ")
}

// Search matches live node names against the query, best rank first. Callers
// that need to know whether the limit hid more matches should use SearchPage.
func (s *Store) Search(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	hits, _, err := s.SearchPage(ctx, query, limit)
	return hits, err
}

// SearchPage matches live node names against the query, best rank first, and
// reports whether at least one additional match exists beyond limit.
func (s *Store) SearchPage(ctx context.Context, query string, limit int) ([]SearchHit, bool, error) {
	if limit <= 0 {
		limit = 50
	}
	fq := ftsQuery(query)
	if fq == "" {
		return nil, false, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+nodeCols+`
		FROM nodes
		WHERE id IN (SELECT rowid FROM nodes_fts WHERE nodes_fts MATCH ?)
		  AND trashed_at IS NULL
		ORDER BY (SELECT rank FROM nodes_fts WHERE rowid = nodes.id AND nodes_fts MATCH ?),
		         name, id
		LIMIT ?`, fq, fq, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("searching %q: %w", query, err)
	}
	defer func() { _ = rows.Close() }()

	var hits []SearchHit
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, false, err
		}
		hits = append(hits, SearchHit{Node: n})
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("searching %q: %w", query, err)
	}
	truncated := len(hits) > limit
	if truncated {
		hits = hits[:limit]
	}
	for i := range hits {
		p, err := s.Path(ctx, hits[i].Node.ID)
		if err != nil {
			return nil, false, err
		}
		hits[i].Path = p
	}
	return hits, truncated, nil
}
