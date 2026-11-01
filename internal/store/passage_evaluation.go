package store

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/vectorindex"
)

// EvaluationPassageFocus returns native prefix-match rune spans in each input's
// own text. A zero span means no match. Rollback removes the temporary index.
func (s *Store) EvaluationPassageFocus(ctx context.Context, query string, texts []string) (_ [][2]int, retErr error) {
	if len(texts) > 4096+100*3 || len(ftsPrefixTerms(query)) == 0 {
		return nil, errors.New("invalid evaluation focus bounds or query")
	}
	// A distinct final byte prevents a marker from overlapping its own prefix.
	marker := "\x01\x02"
	for _, text := range texts {
		if !utf8.ValidString(text) {
			return nil, errors.New("invalid passage UTF-8")
		}
		for strings.Contains(text, marker) {
			marker = "\x01" + marker
		}
	}
	focus := make([][2]int, len(texts))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `CREATE VIRTUAL TABLE temp.evaluation_passage_focus USING fts5(text)`); err != nil {
		return nil, err
	}
	for i, text := range texts {
		if _, err = tx.ExecContext(ctx, `INSERT INTO temp.evaluation_passage_focus(rowid,text) VALUES (?,?)`, i+1, text); err != nil {
			return nil, err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT rowid,highlight(evaluation_passage_focus,0,?,?) FROM temp.evaluation_passage_focus WHERE evaluation_passage_focus MATCH ?`, marker, marker+"\x03", strings.Join(ftsPrefixTerms(query), " OR "))
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	for rows.Next() {
		var row int
		var highlighted string
		if err = rows.Scan(&row, &highlighted); err != nil {
			return nil, err
		}
		start := strings.Index(highlighted, marker)
		if start >= 0 {
			tail := highlighted[start+len(marker):]
			end := strings.Index(tail, marker+"\x03")
			focus[row-1] = [2]int{utf8.RuneCountInString(highlighted[:start]), utf8.RuneCountInString(highlighted[:start]) + utf8.RuneCountInString(tail[:end])}
		}
	}
	return focus, rows.Err()
}

// ResolveEvaluationSemanticPassages uses the serving eligibility and source
// fence, retaining additional ranked chunk inputs for frozen documents only.
func (s *Store) ResolveEvaluationSemanticPassages(ctx context.Context, profile, binding string,
	inputKind document.EmbeddingInputKind, space, source string, neighbors []vectorindex.Neighbor,
	opts SearchOptions, selected map[int64]struct{}, perNode int,
) (SemanticSearchResolution, error) {
	if len(selected) > 100 || perNode < 1 || perNode > 3 {
		return SemanticSearchResolution{}, errors.New("invalid evaluation semantic bounds")
	}
	return s.resolveSemanticCandidates(ctx, profile, binding, inputKind, space, source, neighbors,
		document.MaxRetrievalCandidateLimit, opts, nil, evaluationSemanticReducer(selected, perNode))
}

func evaluationSemanticReducer(selected map[int64]struct{}, perNode int) semanticReducer {
	return func(vault, space string, neighbors []vectorindex.Neighbor, _ int,
		eligible map[semanticEligibilityKey]SemanticSearchCandidate, _ map[int64]struct{},
	) ([]SemanticSearchCandidate, bool, int) {
		counts := make(map[int64]int, len(selected))
		seen := make(map[semanticEligibilityKey]bool)
		var result []SemanticSearchCandidate
		for _, neighbor := range neighbors {
			key := semanticEligibilityKey{neighbor.SetID, neighbor.InputKey, neighbor.InputChecksum}
			entry, ok := eligible[key]
			_, wanted := selected[entry.NodeID]
			if !ok || !wanted || seen[key] || counts[entry.NodeID] >= perNode || entry.InputKind != document.EmbeddingInputRenditionChunk {
				continue
			}
			seen[key] = true
			counts[entry.NodeID]++
			entry.VaultID, entry.VectorSpaceID, entry.InputID, entry.Score = vault, space, neighbor.InputKey, neighbor.Score
			result = append(result, entry)
		}
		return result, false, len(neighbors)
	}
}

// EvaluationLexicalPassage is a ranked segment from a current rendition.
// The span is in rendition-unit coordinates, not embedding source coordinates.
type EvaluationLexicalPassage struct {
	ContentVersionID, BuildID, SegmentID, UnitID string
	UnitOrder, CharStart, CharEnd                int
	Text                                         string
}

// EvaluationLexicalFence binds a candidate version to its captured rendition.
type EvaluationLexicalFence struct {
	ContentVersionID string `json:"version"`
	BuildID          string `json:"build"`
}

// SearchEvaluationLexicalPassages returns ranked matching segments for the
// supplied current versions under the active generation lease. The caller
// must provide a deadline and disclose truncation; the cap bounds returned
// rows rather than FTS work. It is intended for offline evaluation.
func (s *Store) SearchEvaluationLexicalPassages(ctx context.Context, query string, fences []EvaluationLexicalFence, maxRows int) ([]EvaluationLexicalPassage, bool, error) {
	if ctx == nil || maxRows < 1 || maxRows > 4096 || ftsQuery(query) == "" {
		return nil, false, errors.New("query, context and a row limit from 1 to 4096 are required")
	}
	if len(fences) == 0 {
		return nil, false, nil
	}
	versionIDs := make([]string, 0, len(fences))
	for _, fence := range fences {
		if err := validateCatalogSHA256(fence.BuildID, "rendition build"); err != nil {
			return nil, false, err
		}
		versionIDs = append(versionIDs, fence.ContentVersionID)
	}
	slices.Sort(versionIDs)
	versionIDs = slices.Compact(versionIDs)
	_, err := s.NormalizeSearchOptions(ctx, SearchOptions{ContentVersionIDs: versionIDs})
	if err != nil {
		return nil, false, err
	}
	encoded, err := json.Marshal(fences)
	if err != nil {
		return nil, false, err
	}
	var passages []EvaluationLexicalPassage
	err = s.withLexicalGenerationRead(ctx, func(q metadataQuerier, generation LexicalGeneration) (retErr error) {
		if generation.ID == "" {
			return nil
		}
		rows, err := q.QueryContext(ctx, `SELECT cv.version_id, ls.build_id, ls.segment_id, ls.unit_id,
   ru.unit_order, ls.char_start, ls.char_end, ls.text
   FROM rendition_lexical_fts
   JOIN rendition_lexical_generation_builds gb ON gb.build_id=rendition_lexical_fts.build_id
   JOIN rendition_lexical_segments ls ON ls.build_id=rendition_lexical_fts.build_id AND ls.segment_id=rendition_lexical_fts.segment_id
   JOIN rendition_units ru ON ru.build_id=ls.build_id AND ru.unit_id=ls.unit_id
   JOIN rendition_attachments a ON a.build_id=ls.build_id
   JOIN rendition_heads rh ON rh.content_version_id=a.content_version_id AND rh.profile_fingerprint=a.profile_fingerprint AND rh.attachment_id=a.attachment_id
   JOIN content_versions cv ON cv.version_id=a.content_version_id
   JOIN nodes n ON n.id=cv.node_id AND n.current_version_id=cv.version_id
   WHERE rendition_lexical_fts MATCH ? AND gb.generation_id=? AND n.trashed_at IS NULL
    AND EXISTS (SELECT 1 FROM json_each(?) selected
     WHERE json_extract(selected.value, '$.version')=cv.version_id AND json_extract(selected.value, '$.build')=ls.build_id)
   ORDER BY rendition_lexical_fts.rank, n.name, n.id, ls.build_id, ls.segment_id LIMIT ?`, ftsQuery(query), generation.ID, string(encoded), maxRows+1)
		if err != nil {
			return err
		}
		defer func() { retErr = errors.Join(retErr, rows.Close()) }()
		for rows.Next() {
			var p EvaluationLexicalPassage
			if err := rows.Scan(&p.ContentVersionID, &p.BuildID, &p.SegmentID, &p.UnitID, &p.UnitOrder, &p.CharStart, &p.CharEnd, &p.Text); err != nil {
				return err
			}
			passages = append(passages, p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, false, fmt.Errorf("search evaluation lexical passages: %w", err)
	}
	truncated := len(passages) > maxRows
	if truncated {
		passages = passages[:maxRows]
	}
	return passages, truncated, nil
}

// RevalidateEvaluationLexicalFences rejects candidates whose captured build is
// no longer the active rendition for their current content version.
func (s *Store) RevalidateEvaluationLexicalFences(ctx context.Context, fences []EvaluationLexicalFence) error {
	if ctx == nil {
		return errors.New("context is required")
	}
	if len(fences) == 0 {
		return nil
	}
	versions := make([]string, 0, len(fences))
	for _, fence := range fences {
		if err := validateCatalogSHA256(fence.BuildID, "rendition build"); err != nil {
			return err
		}
		versions = append(versions, fence.ContentVersionID)
	}
	slices.Sort(versions)
	versions = slices.Compact(versions)
	if _, err := s.NormalizeSearchOptions(ctx, SearchOptions{ContentVersionIDs: versions}); err != nil {
		return err
	}
	encoded, err := json.Marshal(fences)
	if err != nil {
		return err
	}
	return s.withLexicalGenerationRead(ctx, func(q metadataQuerier, generation LexicalGeneration) error {
		if generation.ID == "" {
			return errors.New("evaluation rendition generation is no longer active")
		}
		var valid int
		err := q.QueryRowContext(ctx, `SELECT count(*) FROM json_each(?) selected WHERE EXISTS (
    SELECT 1 FROM content_versions cv
    JOIN nodes n ON n.id=cv.node_id AND n.current_version_id=cv.version_id
    JOIN rendition_attachments a ON a.content_version_id=cv.version_id
    JOIN rendition_heads rh ON rh.content_version_id=a.content_version_id AND rh.profile_fingerprint=a.profile_fingerprint AND rh.attachment_id=a.attachment_id
    JOIN rendition_lexical_generation_builds gb ON gb.build_id=a.build_id AND gb.generation_id=?
    WHERE n.trashed_at IS NULL AND cv.version_id=json_extract(selected.value,'$.version') AND a.build_id=json_extract(selected.value,'$.build')
  )`, string(encoded), generation.ID).Scan(&valid)
		if err != nil {
			return err
		}
		if valid != len(fences) {
			return errors.New("evaluation rendition fence is stale")
		}
		return nil
	})
}
