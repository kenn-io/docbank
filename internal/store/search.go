package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// SearchHit is a search result with its display path.
type SearchHit struct {
	Node  Node
	Path  string
	Match string
}

const (
	SearchMatchName    = "name"
	SearchMatchContent = "content"
)

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

// SearchPage returns live name matches in their established order, followed
// by content-only matches. Keeping the two ranks separate preserves the
// deterministic name-search contract: enabling extraction never reorders or
// hides a filename match that the same limit returned before.
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
		FROM `+nodeFrom+`
		WHERE n.id IN (SELECT rowid FROM nodes_fts WHERE nodes_fts MATCH ?)
		  AND n.trashed_at IS NULL
		ORDER BY (SELECT rank FROM nodes_fts WHERE rowid = n.id AND nodes_fts MATCH ?),
		         n.name, n.id
		LIMIT ?`, fq, fq, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("searching %q: %w", query, err)
	}
	nameHits, err := scanSearchRows(rows, SearchMatchName, query)
	if err != nil {
		return nil, false, err
	}
	if len(nameHits) > limit {
		nameHits = nameHits[:limit]
		if err := s.addSearchPaths(ctx, nameHits); err != nil {
			return nil, false, err
		}
		return nameHits, true, nil
	}

	// Content may also match a node already returned by name. Over-fetch by
	// the complete name set so duplicate filtering cannot conceal truncation.
	remaining := limit - len(nameHits)
	rows, err = s.db.QueryContext(ctx, `
		WITH matched_blobs AS (
		  SELECT blob_hash, MIN(rank) AS best_rank
		  FROM content_fts WHERE content_fts MATCH ?
		  GROUP BY blob_hash
		)
		SELECT `+nodeCols+`
		FROM `+nodeFrom+`
		JOIN matched_blobs mb ON mb.blob_hash = cv.blob_hash
		WHERE n.trashed_at IS NULL
		ORDER BY mb.best_rank, n.name, n.id
		LIMIT ?`, fq, remaining+len(nameHits)+1)
	if err != nil {
		return nil, false, fmt.Errorf("searching extracted content for %q: %w", query, err)
	}
	contentHits, err := scanSearchRows(rows, SearchMatchContent, query)
	if err != nil {
		return nil, false, err
	}
	seen := make(map[int64]struct{}, len(nameHits))
	for _, hit := range nameHits {
		seen[hit.Node.ID] = struct{}{}
	}
	filtered := contentHits[:0]
	for _, hit := range contentHits {
		if _, exists := seen[hit.Node.ID]; exists {
			continue
		}
		filtered = append(filtered, hit)
	}
	truncated := len(filtered) > remaining
	if truncated {
		filtered = filtered[:remaining]
	}
	hits := make([]SearchHit, 0, len(nameHits)+len(filtered))
	hits = append(hits, nameHits...)
	hits = append(hits, filtered...)
	if err := s.addSearchPaths(ctx, hits); err != nil {
		return nil, false, err
	}
	return hits, truncated, nil
}

func scanSearchRows(rows *sql.Rows, match, query string) ([]SearchHit, error) {
	defer func() { _ = rows.Close() }()
	var hits []SearchHit
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		hits = append(hits, SearchHit{Node: n, Match: match})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("searching %q: %w", query, err)
	}
	return hits, nil
}

func (s *Store) addSearchPaths(ctx context.Context, hits []SearchHit) error {
	for i := range hits {
		p, err := s.Path(ctx, hits[i].Node.ID)
		if err != nil {
			return err
		}
		hits[i].Path = p
	}
	return nil
}
