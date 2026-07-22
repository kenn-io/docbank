package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"mime"
	"strings"
	"time"
)

// SearchHit is a search result with its display path.
type SearchHit struct {
	Node  Node
	Path  string
	Match string
}

// SearchOptions narrows ranked search without changing its name-before-content
// ordering. TagID identifies one required assignment; MIMEType selects the
// current file version's parameter-free base media type; UnderNodeID selects
// descendants of one live directory. ModifiedSince is inclusive and
// ModifiedBefore is exclusive; both accept absolute RFC3339 timestamps.
type SearchOptions struct {
	TagID          string
	MIMEType       string
	UnderNodeID    int64
	ModifiedSince  string
	ModifiedBefore string
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
	return s.SearchPageWithOptions(ctx, query, limit, SearchOptions{})
}

// SearchPageWithOptions returns ranked live matches that satisfy every
// requested filter. Filters apply equally to name and content candidates.
func (s *Store) SearchPageWithOptions(
	ctx context.Context, query string, limit int, opts SearchOptions,
) ([]SearchHit, bool, error) {
	if limit <= 0 {
		limit = 50
	}
	if opts.TagID != "" {
		if _, err := s.TagByID(ctx, opts.TagID); err != nil {
			return nil, false, fmt.Errorf("search tag %q: %w", opts.TagID, err)
		}
	}
	normalizedMIME, err := NormalizeSearchMIMEType(opts.MIMEType)
	if err != nil {
		return nil, false, err
	}
	opts.MIMEType = normalizedMIME
	if opts.UnderNodeID < 0 {
		return nil, false, errors.New("search directory node ID must be positive")
	}
	if opts.UnderNodeID != 0 {
		directory, err := s.NodeByID(ctx, opts.UnderNodeID)
		if err != nil {
			return nil, false, fmt.Errorf("search directory node %d: %w", opts.UnderNodeID, err)
		}
		if directory.TrashedAt != nil {
			return nil, false, fmt.Errorf("search directory node %d is trashed: %w",
				opts.UnderNodeID, ErrNotFound)
		}
		if !directory.IsDir() {
			return nil, false, fmt.Errorf("search scope node %d: %w", opts.UnderNodeID, ErrNotDir)
		}
	}
	modifiedSince, modifiedBefore, err := NormalizeSearchTimeBounds(
		opts.ModifiedSince, opts.ModifiedBefore,
	)
	if err != nil {
		return nil, false, err
	}
	opts.ModifiedSince = modifiedSince
	opts.ModifiedBefore = modifiedBefore
	fq := ftsQuery(query)
	if fq == "" {
		return nil, false, nil
	}
	filterSQL, filterArgs := searchFilterSQL(opts)
	nameArgs := []any{fq}
	nameArgs = append(nameArgs, filterArgs...)
	nameArgs = append(nameArgs, fq, limit+1)
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+nodeCols+`
		FROM `+nodeFrom+`
		WHERE n.id IN (SELECT rowid FROM nodes_fts WHERE nodes_fts MATCH ?)
		  AND n.trashed_at IS NULL
		  `+filterSQL+`
		ORDER BY (SELECT rank FROM nodes_fts WHERE rowid = n.id AND nodes_fts MATCH ?),
		         n.name, n.id
		LIMIT ?`, nameArgs...)
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
	contentArgs := []any{fq}
	contentArgs = append(contentArgs, filterArgs...)
	contentArgs = append(contentArgs, remaining+len(nameHits)+1)
	rows, err = s.db.QueryContext(ctx, `
		WITH matched_blobs AS (
		  SELECT blob_hash, MIN(rank) AS best_rank
		  FROM content_fts WHERE content_fts MATCH ?
		  GROUP BY blob_hash
		)
		SELECT `+nodeCols+`
		FROM `+nodeFrom+`
		JOIN matched_blobs mb ON mb.blob_hash = cv.blob_hash
		JOIN text_searchable_versions tsv ON tsv.version_id = cv.version_id
		WHERE n.trashed_at IS NULL
		  `+filterSQL+`
		ORDER BY mb.best_rank, n.name, n.id
		LIMIT ?`, contentArgs...)
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

func searchFilterSQL(opts SearchOptions) (string, []any) {
	var (
		clauses []string
		args    []any
	)
	if opts.TagID != "" {
		clauses = append(clauses, `AND EXISTS (
			SELECT 1 FROM node_tags nt WHERE nt.node_id=n.id AND nt.tag_id=?
		)`)
		args = append(args, opts.TagID)
	}
	if opts.MIMEType != "" {
		clauses = append(clauses, `AND lower(trim(CASE
			WHEN instr(cv.mime_type, ';')=0 THEN cv.mime_type
			ELSE substr(cv.mime_type, 1, instr(cv.mime_type, ';')-1)
		END))=?`)
		args = append(args, opts.MIMEType)
	}
	if opts.UnderNodeID != 0 {
		clauses = append(clauses, `AND n.id IN (
			WITH RECURSIVE descendants(id) AS (
				SELECT id FROM nodes WHERE parent_id=?
				UNION ALL
				SELECT child.id FROM nodes child
				JOIN descendants parent ON child.parent_id=parent.id
			)
			SELECT id FROM descendants
		)`)
		args = append(args, opts.UnderNodeID)
	}
	if opts.ModifiedSince != "" {
		clauses = append(clauses, `AND n.modified_at>=?`)
		args = append(args, opts.ModifiedSince)
	}
	if opts.ModifiedBefore != "" {
		clauses = append(clauses, `AND n.modified_at<?`)
		args = append(args, opts.ModifiedBefore)
	}
	return strings.Join(clauses, "\n"), args
}

// NormalizeSearchTimeBounds accepts optional absolute RFC3339 timestamps and
// returns canonical UTC bounds. The half-open interval makes adjacent searches
// compose without duplicate boundary results.
func NormalizeSearchTimeBounds(modifiedSince, modifiedBefore string) (string, string, error) {
	since, err := normalizeSearchTimestamp("modified_since", modifiedSince)
	if err != nil {
		return "", "", err
	}
	before, err := normalizeSearchTimestamp("modified_before", modifiedBefore)
	if err != nil {
		return "", "", err
	}
	if since != "" && before != "" && since >= before {
		return "", "", errors.New("modified_since must be earlier than modified_before")
	}
	return since, before, nil
}

func normalizeSearchTimestamp(field, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", fmt.Errorf("%s %q must be an absolute RFC3339 timestamp: %w", field, value, err)
	}
	return parsed.UTC().Format(timestampLayout), nil
}

// NormalizeSearchMIMEType accepts one parameter-free media type and returns
// its canonical base spelling. Stored parameters do not participate in search
// filtering because they describe representation details, not the format.
func NormalizeSearchMIMEType(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	mediaType, params, err := mime.ParseMediaType(value)
	if err != nil {
		return "", fmt.Errorf("search MIME type %q is invalid: %w", value, err)
	}
	if len(params) != 0 {
		return "", fmt.Errorf(
			"search MIME type %q must not include parameters; use %q", value, mediaType,
		)
	}
	if strings.Contains(mediaType, "*") {
		return "", fmt.Errorf("search MIME type %q must not contain wildcards", value)
	}
	return mediaType, nil
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
