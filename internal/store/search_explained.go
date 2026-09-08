package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// ExplainedLexicalCandidate adds stable evidence identity and a bounded
// display excerpt without changing the established SearchHit ordering.
type ExplainedLexicalCandidate struct {
	Node         Node
	Path         string
	Match        string
	EvidenceKind string
	BuildID      string
	SegmentID    string
	BlobHash     string
	Excerpt      string
}

const maxExplainedSearchExcerptRunes = 512

// SearchExplainedLexicalCandidates preserves SearchPageWithOptions ordering
// for files. Content selection and evidence resolution share one lexical-
// generation read snapshot.
func (s *Store) SearchExplainedLexicalCandidates(
	ctx context.Context, query string, limit int, opts SearchOptions,
) ([]ExplainedLexicalCandidate, bool, error) {
	if limit <= 0 {
		limit = 50
	}
	var err error
	opts, err = s.normalizeSearchOptions(ctx, opts)
	if err != nil {
		return nil, false, err
	}
	fq := ftsQuery(query)
	if fq == "" {
		return nil, false, ErrSearchQueryRequired
	}
	filterSQL, filterArgs := searchFilterSQL(opts)
	nameArgs := append([]any{fq}, filterArgs...)
	nameArgs = append(nameArgs, fq, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT `+nodeCols+` FROM `+nodeFrom+`
		WHERE n.id IN (SELECT rowid FROM nodes_fts WHERE nodes_fts MATCH ?)
		  AND n.kind='file' AND cv.version_id IS NOT NULL AND n.trashed_at IS NULL `+filterSQL+`
		ORDER BY (SELECT rank FROM nodes_fts WHERE rowid=n.id AND nodes_fts MATCH ?),n.name,n.id
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
		return explainedNameCandidates(nameHits), true, nil
	}
	if err := s.addSearchPaths(ctx, nameHits); err != nil {
		return nil, false, err
	}
	remaining := limit - len(nameHits)
	nameSeen := make(map[int64]struct{}, len(nameHits))
	for _, hit := range nameHits {
		nameSeen[hit.Node.ID] = struct{}{}
	}

	var content []ExplainedLexicalCandidate
	err = s.withLexicalGenerationRead(ctx, func(
		queryer metadataQuerier, generation LexicalGeneration,
	) error {
		var queryErr error
		content, queryErr = s.queryExplainedContentCandidates(ctx, queryer, generation.ID,
			fq, query, remaining, filterSQL, filterArgs, nameSeen)
		return queryErr
	})
	if errors.Is(err, ErrNotFound) {
		content, err = s.queryExplainedContentCandidates(ctx, s.db, "",
			fq, query, remaining, filterSQL, filterArgs, nameSeen)
	}
	if err != nil {
		return nil, false, err
	}
	truncated := len(content) > remaining
	if truncated {
		content = content[:remaining]
	}
	result := explainedNameCandidates(nameHits)
	result = append(result, content...)
	return result, truncated, nil
}

func (s *Store) queryExplainedContentCandidates(
	ctx context.Context, queryer metadataQuerier, generationID, fq, query string,
	remaining int, filterSQL string, filterArgs []any, nameSeen map[int64]struct{},
) (_ []ExplainedLexicalCandidate, retErr error) {
	args := []any{fq}
	contentQuery := `SELECT ` + nodeCols + `,'' AS build_id,'' AS segment_id,
		 snippet(content_fts,2,char(1),char(2),' … ',24) AS excerpt
		FROM content_fts JOIN content_versions matched_cv ON matched_cv.blob_hash=content_fts.blob_hash
		JOIN nodes n ON n.id=matched_cv.node_id AND n.current_version_id=matched_cv.version_id
		JOIN content_versions cv ON cv.version_id=matched_cv.version_id
		JOIN text_searchable_versions tsv ON tsv.version_id=matched_cv.version_id
		WHERE content_fts MATCH ? AND n.trashed_at IS NULL ` + filterSQL + `
		ORDER BY content_fts.rank,n.name,n.id,content_fts.rowid`
	if generationID != "" {
		contentQuery = `SELECT ` + nodeCols + `,rendition_lexical_fts.build_id,
			 rendition_lexical_fts.segment_id,snippet(rendition_lexical_fts,2,char(1),char(2),' … ',24)
			FROM rendition_lexical_fts
			JOIN rendition_lexical_generation_builds gb
			 ON gb.build_id=rendition_lexical_fts.build_id
			JOIN rendition_attachments a ON a.build_id=rendition_lexical_fts.build_id
			JOIN rendition_heads rh ON rh.content_version_id=a.content_version_id
			 AND rh.profile_fingerprint=a.profile_fingerprint AND rh.attachment_id=a.attachment_id
			JOIN content_versions cv ON cv.version_id=a.content_version_id
			JOIN nodes n ON n.id=cv.node_id AND n.current_version_id=cv.version_id
			WHERE rendition_lexical_fts MATCH ? AND gb.generation_id=?
			 AND n.trashed_at IS NULL ` + filterSQL + `
			ORDER BY rendition_lexical_fts.rank,n.name,n.id,
			 rendition_lexical_fts.build_id,rendition_lexical_fts.segment_id`
		args = append(args, generationID)
	}
	args = append(args, filterArgs...)
	rows, err := queryer.QueryContext(ctx, contentQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("searching extracted content for %q: %w", query, err)
	}
	defer func() {
		retErr = errors.Join(retErr, rows.Close())
	}()
	seenContent := make(map[int64]struct{}, remaining+1)
	var content []ExplainedLexicalCandidate
	for rows.Next() {
		var candidate ExplainedLexicalCandidate
		node, err := scanExplainedLexicalRow(rows, &candidate)
		if err != nil {
			return nil, err
		}
		candidate.Node, candidate.Match = node, SearchMatchContent
		if _, duplicate := nameSeen[node.ID]; duplicate {
			continue
		}
		if _, duplicate := seenContent[node.ID]; duplicate {
			continue
		}
		seenContent[node.ID] = struct{}{}
		if generationID == "" {
			candidate.EvidenceKind = "content_blob"
			candidate.BlobHash = node.BlobHash
		} else {
			candidate.EvidenceKind = "rendition_segment"
		}
		candidate.Path, err = pathOf(ctx, queryer, node.ID)
		if err != nil {
			return nil, err
		}
		content = append(content, candidate)
		if len(content) == remaining+1 {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return content, nil
}

func explainedNameCandidates(hits []SearchHit) []ExplainedLexicalCandidate {
	result := make([]ExplainedLexicalCandidate, len(hits))
	for index, hit := range hits {
		result[index] = ExplainedLexicalCandidate{Node: hit.Node, Path: hit.Path,
			Match: SearchMatchName, EvidenceKind: "node_name", Excerpt: hit.Node.Name}
	}
	return result
}

func scanExplainedLexicalRow(
	row interface{ Scan(dest ...any) error }, candidate *ExplainedLexicalCandidate,
) (Node, error) {
	var node Node
	if err := row.Scan(&node.ID, &node.ParentID, &node.Name, &node.Kind,
		&node.CurrentVersionID, &node.BlobHash, &node.MD5, &node.Size, &node.MimeType,
		&node.Revision, &node.CreatedAt, &node.ModifiedAt, &node.TrashedAt,
		&candidate.BuildID, &candidate.SegmentID, &candidate.Excerpt); err != nil {
		return Node{}, err
	}
	candidate.Excerpt = boundedExplainedSearchExcerpt(candidate.Excerpt)
	return node, nil
}

func boundedExplainedSearchExcerpt(value string) string {
	runes := []rune(value)
	if len(runes) > maxExplainedSearchExcerptRunes {
		match := max(slices.Index(runes, rune(1)), 0)
		start := max(0, min(match-maxExplainedSearchExcerptRunes/2,
			len(runes)-maxExplainedSearchExcerptRunes))
		runes = runes[start : start+maxExplainedSearchExcerptRunes]
	}
	runes = slices.DeleteFunc(runes, func(value rune) bool { return value == 1 || value == 2 })
	return string(runes)
}
