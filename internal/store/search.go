package store

import (
	"cmp"
	"container/heap"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.kenn.io/docbank/document"
	querypkg "go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/vectorindex"
	"go.kenn.io/kit/search/lexical"
	"go.kenn.io/kit/search/sqlitefts"
	"go.kenn.io/kit/search/sqlquery"
)

// MaxSearchSourceFenceIDs is the shared hard bound for exact search authority.
const MaxSearchSourceFenceIDs = 4096

// SearchHit is a search result with its display path.
type SearchHit struct {
	Node  Node
	Path  string
	Match string
}

// ExplainedLexicalCandidate adds stable evidence identity and a bounded
// display excerpt for files under the requested lexical ordering.
type ExplainedLexicalCandidate struct {
	Node         Node
	Path         string
	Match        string
	EvidenceKind string
	BuildID      string
	SegmentID    string
	BlobHash     string
	Excerpt      string
	Locator      document.EvidenceLocatorV1
}

// SearchMediaEvidence retains the exact immutable artifacts needed to map one
// rendition-chunk embedding back to its normalized evidence unit.
type SearchMediaEvidence struct {
	BuildID               string
	GenerationBlobHash    string
	GenerationEncodedSize int64
	GenerationChecksum    string
	EvidenceFingerprint   string
	EvidenceEncodedSize   int64
	InputCount            int
}

const maxExplainedSearchExcerptRunes = 512

// currentRenditionChunkAuthoritySQL is the shared serving predicate for a
// rendition-chunk embedding. Every caller supplies embedding_sets as es and
// embedding_input_generations as eig.
const currentRenditionChunkAuthoritySQL = `EXISTS (
	SELECT 1 FROM rendition_heads current_rh
	JOIN rendition_attachments current_ra ON current_ra.attachment_id=current_rh.attachment_id
	 AND current_ra.content_version_id=current_rh.content_version_id
	 AND current_ra.profile_fingerprint=current_rh.profile_fingerprint
	JOIN rendition_builds current_rb ON current_rb.build_id=current_ra.build_id
	JOIN blobs current_evidence_blob ON current_evidence_blob.hash=eig.evidence_fingerprint
	WHERE current_rh.content_version_id=es.content_version_id
	 AND current_rh.profile_fingerprint=es.profile_fingerprint
	 AND current_rh.attachment_id=eig.attachment_id
	 AND current_rb.evidence_checksum=eig.evidence_fingerprint
)`

// SearchExplainedLexicalCandidates prefers filenames unless contentFirst is set.
// Content selection and evidence resolution share one lexical-generation read.
func (s *Store) SearchExplainedLexicalCandidates(ctx context.Context, query string, limit int,
	opts SearchOptions, contentFirst bool,
) ([]ExplainedLexicalCandidate, bool, error) {
	if limit <= 0 {
		limit = 50
	}
	var err error
	opts, err = s.NormalizeSearchOptions(ctx, opts)
	if err != nil {
		return nil, false, err
	}
	if opts.PhotoScope != nil {
		request := *opts.PhotoScope
		if opts.PhotoLexicalQuery == nil {
			return nil, false, ErrInvalidPhotoQuery
		}
		request.Query = *opts.PhotoLexicalQuery
		request.Query.Mode = querypkg.ModeLexical
		request.Query.Sort = querypkg.Sort{Field: "relevance", Direction: "desc"}
		request.PageSize = min(limit+1, MaxPhotoMapPageSize)
		request.Stacks = false
		request.Recipes = nil
		request.Facets = nil
		page, err := s.ListPhotoAssets(ctx, request, nil)
		if err != nil {
			return nil, false, err
		}
		hits := make([]ExplainedLexicalCandidate, 0, min(limit, len(page.Items)))
		for _, row := range page.Items[:min(limit, len(page.Items))] {
			hits = append(hits, ExplainedLexicalCandidate{Node: Node{ID: row.NodeID, CurrentVersionID: row.ContentVersionID, Name: row.Name, BlobHash: row.sourceHash}, EvidenceKind: "node_name"})
		}
		return hits, page.Total > int64(limit), nil
	}
	fq := ftsQuery(query)
	if fq == "" {
		return nil, false, nil
	}
	nameFilterSQL, filterSQL, filterArgs := searchCandidateFiltersSQL(opts)
	selectedCTE, selectedJoin, attachmentSelection, selectedArgs := "", "", "", []any(nil)
	if len(opts.SelectedBuilds) != 0 {
		encoded, err := json.Marshal(opts.SelectedBuilds)
		if err != nil {
			return nil, false, err
		}
		selectedCTE = `WITH selected AS MATERIALIZED (
			SELECT json_extract(value,'$.content_version_id') content_version_id,
			json_extract(value,'$.build_id') build_id FROM json_each(?)) `
		// Check each build's pairs before catalog joins to avoid repeating the fence scan.
		selectedJoin = ` CROSS JOIN selected ON selected.build_id=rendition_lexical_fts.build_id `
		attachmentSelection = ` AND a.content_version_id=selected.content_version_id `
		selectedArgs = []any{string(encoded)}
	}
	var nameHits []SearchHit
	if len(opts.SelectedBuilds) == 0 {
		names, err := nameSearchCandidates(fq)
		if err != nil {
			return nil, false, err
		}
		nameArgs := names.Args
		nameArgs = append(nameArgs, filterArgs...)
		nameArgs = append(nameArgs, limit+1)
		rows, err := s.db.QueryContext(ctx, nameSearchPageSQL(names.SQL,
			"AND n.kind='file' AND cv.version_id IS NOT NULL "+nameFilterSQL), nameArgs...)
		if err != nil {
			return nil, false, err
		}
		nameHits, err = scanSearchRows(rows, SearchMatchName, query)
		if err != nil {
			return nil, false, err
		}
		if !contentFirst && len(nameHits) > limit {
			nameHits = nameHits[:limit]
			if err := s.addSearchPaths(ctx, nameHits); err != nil {
				return nil, false, err
			}
			return explainedNameCandidates(nameHits), true, nil
		}
		if err := s.addSearchPaths(ctx, nameHits); err != nil {
			return nil, false, err
		}
	}
	remaining := limit - len(nameHits)
	if contentFirst {
		remaining = limit
	}
	nameSeen := make(map[int64]struct{}, len(nameHits))
	for _, hit := range nameHits {
		nameSeen[hit.Node.ID] = struct{}{}
	}
	var content []ExplainedLexicalCandidate
	queryContent := func(queryer metadataQuerier, generationID string) (retErr error) {
		if len(opts.SelectedBuilds) != 0 && generationID == "" {
			return errors.New("selected media builds have no lexical generation")
		}
		args := []any{fq}
		contentQuery := `SELECT ` + nodeCols + `,'' AS build_id,'' AS segment_id,
			 snippet(content_fts,2,char(1),char(2),' … ',24) AS excerpt,'' AS locator_json
			FROM content_fts CROSS JOIN content_versions matched_cv ON matched_cv.blob_hash=content_fts.blob_hash
			CROSS JOIN nodes n ON n.id=matched_cv.node_id AND n.current_version_id=matched_cv.version_id
			CROSS JOIN content_versions cv ON cv.version_id=matched_cv.version_id
			CROSS JOIN text_searchable_versions tsv ON tsv.version_id=matched_cv.version_id
			WHERE content_fts MATCH ? AND n.trashed_at IS NULL ` + filterSQL + `
			ORDER BY content_fts.rank,n.name,n.id,content_fts.rowid`
		if generationID != "" {
			contentQuery = selectedCTE + `SELECT ` + nodeCols + `,rendition_lexical_fts.build_id,
				 rendition_lexical_fts.segment_id,snippet(rendition_lexical_fts,2,char(1),char(2),' … ',24),
				 ru.locator_json
				FROM rendition_lexical_fts ` + selectedJoin + `
				CROSS JOIN rendition_lexical_generation_builds gb
				 ON gb.build_id=rendition_lexical_fts.build_id
				CROSS JOIN rendition_lexical_segments ls ON ls.build_id=rendition_lexical_fts.build_id
				 AND ls.segment_id=rendition_lexical_fts.segment_id
				CROSS JOIN rendition_units ru ON ru.build_id=ls.build_id AND ru.unit_id=ls.unit_id
				JOIN rendition_attachments a
				 ON a.build_id=rendition_lexical_fts.build_id ` + attachmentSelection + `
				JOIN rendition_heads rh ON rh.content_version_id=a.content_version_id
				 AND rh.profile_fingerprint=a.profile_fingerprint AND rh.attachment_id=a.attachment_id
				JOIN content_versions cv ON cv.version_id=a.content_version_id
				JOIN nodes n ON n.id=cv.node_id AND n.current_version_id=cv.version_id
				WHERE rendition_lexical_fts MATCH ? AND gb.generation_id=?
				 AND n.trashed_at IS NULL ` + filterSQL + `
				ORDER BY rendition_lexical_fts.rank,n.name,n.id,
				 rendition_lexical_fts.build_id,rendition_lexical_fts.segment_id`
			args = slices.Concat(selectedArgs, []any{fq, generationID})
		}
		args = append(args, filterArgs...)
		rows, err := queryer.QueryContext(ctx, contentQuery, args...)
		if err != nil {
			return err
		}
		defer func() { retErr = errors.Join(retErr, rows.Close()) }()
		type candidateKey struct {
			nodeID  int64
			buildID string
		}
		seenContent := make(map[candidateKey]struct{}, remaining+1)
		for rows.Next() {
			var candidate ExplainedLexicalCandidate
			node, err := scanExplainedLexicalRow(rows, &candidate)
			if err != nil {
				return err
			}
			candidate.Node, candidate.Match = node, SearchMatchContent
			if _, duplicate := nameSeen[node.ID]; duplicate && !contentFirst {
				continue
			}
			key := candidateKey{nodeID: node.ID}
			if len(opts.SelectedBuilds) != 0 {
				key.buildID = candidate.BuildID
			}
			if _, duplicate := seenContent[key]; duplicate {
				continue
			}
			seenContent[key] = struct{}{}
			if generationID == "" {
				candidate.EvidenceKind = "content_blob"
				candidate.BlobHash = node.BlobHash
			} else {
				candidate.EvidenceKind = "rendition_segment"
			}
			content = append(content, candidate)
			if len(content) == remaining+1 {
				break
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		if len(content) == 1 {
			content[0].Path, err = pathOf(ctx, queryer, content[0].Node.ID)
			return err
		}
		ids := make([]int64, len(content))
		for i, candidate := range content {
			ids[i] = candidate.Node.ID
		}
		paths, err := s.pathsOf(ctx, queryer, ids)
		if err != nil {
			return err
		}
		for i, path := range paths {
			content[i].Path = path
		}
		return nil
	}
	err = s.withLexicalGenerationRead(ctx, func(queryer metadataQuerier, generation LexicalGeneration) error {
		return queryContent(queryer, generation.ID)
	})
	if err != nil {
		return nil, false, err
	}
	truncated := len(content) > remaining
	if truncated {
		content = content[:remaining]
	}
	if contentFirst {
		result, namesTruncated := appendNameTail(content, nameHits, limit)
		return result, truncated || namesTruncated, nil
	}
	result := explainedNameCandidates(nameHits)
	result = append(result, content...)
	return result, truncated, nil
}

func appendNameTail(content []ExplainedLexicalCandidate, nameHits []SearchHit, limit int) ([]ExplainedLexicalCandidate, bool) {
	selected := make(map[int64]struct{}, len(content))
	for _, candidate := range content {
		selected[candidate.Node.ID] = struct{}{}
	}
	for _, candidate := range explainedNameCandidates(nameHits) {
		if _, duplicate := selected[candidate.Node.ID]; duplicate {
			continue
		}
		if len(content) == limit {
			return content, true
		}
		content = append(content, candidate)
	}
	return content, false
}

func explainedNameCandidates(hits []SearchHit) []ExplainedLexicalCandidate {
	result := make([]ExplainedLexicalCandidate, len(hits))
	for index, hit := range hits {
		result[index] = ExplainedLexicalCandidate{Node: hit.Node, Path: hit.Path,
			Match: SearchMatchName, EvidenceKind: "node_name", Excerpt: boundedExplainedSearchExcerpt(hit.Node.Name)}
	}
	return result
}

func scanExplainedLexicalRow(row interface{ Scan(dest ...any) error }, candidate *ExplainedLexicalCandidate) (Node, error) {
	var node Node
	var locatorJSON string
	if err := row.Scan(&node.ID, &node.ParentID, &node.Name, &node.Kind,
		&node.CurrentVersionID, &node.BlobHash, &node.MD5, &node.Size, &node.MimeType,
		&node.Revision, &node.CreatedAt, &node.ModifiedAt, &node.TrashedAt,
		&candidate.BuildID, &candidate.SegmentID, &candidate.Excerpt, &locatorJSON); err != nil {
		return Node{}, err
	}
	if locatorJSON != "" {
		if err := json.Unmarshal([]byte(locatorJSON), &candidate.Locator); err != nil {
			return Node{}, fmt.Errorf("decoding lexical evidence locator: %w", err)
		}
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

// SearchOptions selects a filter-only page or narrows ranked search without
// changing ordinary node search ordering. TagID identifies one required
// assignment; MIMEType selects the current file version's parameter-free base
// media type; UnderNodeID selects
// descendants of one live directory. ModifiedSince is inclusive and
// ModifiedBefore is exclusive; both accept absolute RFC3339 timestamps.
type SearchOptions struct {
	PDFFilter           string
	pdfPredicate        compiledQueryFragment
	SourcePresent       bool
	VectorSpaceID       string
	EmbeddingDescriptor *document.EmbeddingDescriptor
	PhotoScope          *PhotoBrowseRequest
	// PhotoLexicalQuery retains the authored syntax independently of the vector filter scope.
	PhotoLexicalQuery *querypkg.Query
	photoFilter       compiledQueryFragment
	photoLexical      compiledQueryFragment
	SelectedBuilds    []SearchSelectedBuild
	TagID             string
	MIMEType          string
	UnderNodeID       int64
	ModifiedSince     string
	ModifiedBefore    string
	ContentVersionIDs []string
}

type SearchSelectedBuild struct {
	ContentVersionID string `json:"content_version_id"`
	BuildID          string `json:"build_id"`
}

// SearchNeedsQuery reports whether the normalized options leave an empty FTS
// query without the tag or time anchor required for a bounded filter page.
func SearchNeedsQuery(query string, opts SearchOptions) bool {
	return ftsQuery(query) == "" && opts.PDFFilter == "" && opts.TagID == "" &&
		opts.ModifiedSince == "" && opts.ModifiedBefore == ""
}

// SearchCandidateIdentity identifies one current scoped head for a final
// retrieval fence without retaining any query or document text. Node changes
// such as tag edits leave its evidence valid; each evidence item states what
// it depends on.
type SearchCandidateIdentity struct {
	BuildID          string
	NodeID           int64
	ContentVersionID string
	Evidence         []SearchEvidenceIdentity
}

// SearchCoverageSnapshot is the current semantic coverage captured with the
// final candidate/evidence fence.
type SearchCoverageSnapshot struct {
	// Populated only for exact selected-build searches, in the same snapshot.
	CompleteContentVersionIDs []string
	ScopedDocuments           int
	CompleteDocuments         int
}

// SearchCandidateRevalidation returns allowed candidates and, when semantic
// authority was supplied, coverage from the same storage snapshot.
type SearchCandidateRevalidation struct {
	Candidates []RevalidatedSearchCandidate
	Coverage   *SearchCoverageSnapshot
}

// RevalidatedSearchCandidate includes the current path captured with its evidence.
type RevalidatedSearchCandidate struct {
	SearchCandidateIdentity

	Path string
}

// SearchEvidenceIdentity is the stable authority needed to prove that one
// result still cites current serving evidence. NodeName is the name a
// node_name match was read with; a rename invalidates that evidence. Each item
// names its own file version when duplicate photos contribute different signals.
type SearchEvidenceIdentity struct {
	NodeID                 int64                       `json:"node_id"`
	ContentVersionID       string                      `json:"version_id"`
	Kind                   string                      `json:"kind"`
	NodeName               string                      `json:"node_name,omitempty"`
	VectorSpaceID          string                      `json:"vector_space_id,omitempty"`
	EmbeddingSetID         string                      `json:"embedding_set_id,omitempty"`
	InputGenerationID      string                      `json:"input_generation_id,omitempty"`
	InputID                string                      `json:"input_id,omitempty"`
	InputKind              document.EmbeddingInputKind `json:"input_kind,omitempty"`
	BuildID                string                      `json:"build_id,omitempty"`
	SegmentID              string                      `json:"segment_id,omitempty"`
	BlobHash               string                      `json:"blob_hash,omitempty"`
	SourceManifestChecksum string                      `json:"source_manifest_checksum,omitempty"`
}

type searchCandidateRevalidationJSON struct {
	BuildID          string                   `json:"build_id,omitempty"`
	NodeID           int64                    `json:"node_id"`
	ContentVersionID string                   `json:"version_id"`
	Evidence         []SearchEvidenceIdentity `json:"evidence"`
}

// RevalidateSearchCandidates retains only supplied identities that remain live,
// current, and inside the operator scope at this final check.
func (s *Store) RevalidateSearchCandidates(ctx context.Context, candidates []SearchCandidateIdentity,
	opts SearchOptions, semanticProfileFingerprint, semanticBindingID string,
) (SearchCandidateRevalidation, error) {
	if len(candidates) > document.MaxRetrievalCandidateLimit {
		return SearchCandidateRevalidation{}, errors.New("search candidate revalidation exceeds the retrieval limit")
	}
	normalized, err := s.NormalizeSearchOptions(ctx, opts)
	if err != nil {
		return SearchCandidateRevalidation{}, err
	}
	if len(candidates) == 0 && semanticProfileFingerprint == "" && semanticBindingID == "" {
		return SearchCandidateRevalidation{}, nil
	}
	payload := make([]searchCandidateRevalidationJSON, len(candidates))
	semanticSpace, semanticSource := "", ""
	hasRenditionEvidence := false
	for i, candidate := range candidates {
		if len(candidate.Evidence) == 0 || len(candidate.Evidence) > 32 {
			return SearchCandidateRevalidation{}, errors.New("search candidate evidence is invalid")
		}
		payload[i] = searchCandidateRevalidationJSON(candidate)
		for _, evidence := range candidate.Evidence {
			if evidence.NodeID <= 0 || evidence.ContentVersionID == "" {
				return SearchCandidateRevalidation{}, errors.New("search evidence document identity is incomplete")
			}
			if evidence.Kind == "rendition_segment" {
				hasRenditionEvidence = true
			}
			if evidence.Kind != "embedding" {
				continue
			}
			if evidence.VectorSpaceID == "" || evidence.SourceManifestChecksum == "" {
				return SearchCandidateRevalidation{}, errors.New("semantic search evidence authority is incomplete")
			}
			if semanticSpace == "" {
				semanticSpace, semanticSource = evidence.VectorSpaceID, evidence.SourceManifestChecksum
			}
			if semanticSpace != evidence.VectorSpaceID || semanticSource != evidence.SourceManifestChecksum {
				return SearchCandidateRevalidation{}, errors.New("semantic search evidence authority is incompatible")
			}
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return SearchCandidateRevalidation{}, err
	}
	var result SearchCandidateRevalidation
	var allowedPositions []int
	err = s.withSearchOptionsTx(ctx, normalized, func(tx *sql.Tx, normalized SearchOptions) error {
		filterSQL, filterArgs := searchFilterSQL(normalized)
		if semanticProfileFingerprint != "" || semanticBindingID != "" {
			if semanticProfileFingerprint == "" || semanticBindingID == "" {
				return errors.New("semantic search coverage authority is incomplete")
			}
			binding, fingerprints, authorityErr := embeddingProfileBindingAuthority(ctx, tx,
				semanticProfileFingerprint, semanticBindingID)
			if authorityErr != nil {
				return authorityErr
			}
			expectedSpace := fingerprints.VectorSpace[semanticBindingID]
			if normalized.VectorSpaceID != "" {
				expectedSpace = normalized.VectorSpaceID
			}
			if semanticSpace != "" && semanticSpace != expectedSpace {
				return errors.New("semantic search evidence does not match coverage authority")
			}
			semanticSpace = expectedSpace
			scoped, complete, coverageErr := semanticSearchCoverageTx(ctx, tx,
				semanticProfileFingerprint, semanticBindingID, binding.InputKind, semanticSpace, normalized)
			if coverageErr != nil {
				return coverageErr
			}
			result.Coverage = &SearchCoverageSnapshot{ScopedDocuments: scoped, CompleteDocuments: complete}
			if len(normalized.SelectedBuilds) != 0 {
				result.Coverage.CompleteContentVersionIDs, err = selectedSemanticCompleteVersionsTx(ctx, tx, semanticProfileFingerprint, semanticBindingID, binding.InputKind, semanticSpace, normalized)
				if err != nil {
					return err
				}
			}
		} else if semanticSpace != "" {
			return errors.New("semantic search evidence lacks coverage authority")
		}
		renditionEvidenceSQL := `WHEN 'rendition_segment' THEN 1`
		if hasRenditionEvidence {
			var lexicalProjection bool
			if schemaErr := tx.QueryRowContext(ctx, `SELECT EXISTS(
				SELECT 1 FROM sqlite_schema WHERE type='table' AND name='rendition_lexical_heads'
			)`).Scan(&lexicalProjection); schemaErr != nil {
				return schemaErr
			}
			if lexicalProjection {
				renditionEvidenceSQL = `WHEN 'rendition_segment' THEN NOT EXISTS (
					SELECT 1 FROM rendition_lexical_heads lh
					JOIN rendition_lexical_generation_builds gb ON gb.generation_id=lh.generation_id
					JOIN rendition_lexical_segments ls ON ls.build_id=gb.build_id
					JOIN rendition_attachments ra ON ra.build_id=ls.build_id
					JOIN rendition_heads rh ON rh.content_version_id=ra.content_version_id
					 AND rh.profile_fingerprint=ra.profile_fingerprint AND rh.attachment_id=ra.attachment_id
					WHERE lh.singleton=1 AND ra.content_version_id=source.version_id
					 AND ls.build_id=json_extract(evidence.value,'$.build_id')
					 AND ls.segment_id=json_extract(evidence.value,'$.segment_id'))`
			}
		}
		if semanticSource != "" {
			current, captureErr := s.vectorIndexSourceChecksumTx(ctx, tx, semanticSpace)
			if captureErr != nil {
				if errors.Is(captureErr, ErrNotFound) {
					return ErrVectorIndexSourceStale
				}
				return captureErr
			}
			if current != semanticSource {
				return ErrVectorIndexSourceStale
			}
		}
		var selectedArgs []any
		if len(normalized.SelectedBuilds) != 0 {
			selected, _ := json.Marshal(normalized.SelectedBuilds)
			selectedArgs = []any{string(selected)}
		}
		args := slices.Concat([]any{string(encoded)}, filterArgs, selectedArgs, normalized.photoLexical.args, []any{semanticProfileFingerprint, semanticBindingID})
		rows, queryErr := tx.QueryContext(ctx, searchCandidateRevalidationSQL(filterSQL, renditionEvidenceSQL, normalized), args...)
		if queryErr != nil {
			return queryErr
		}
		err = func() (retErr error) {
			defer func() { retErr = errors.Join(retErr, rows.Close()) }()
			for rows.Next() {
				var position int
				if scanErr := rows.Scan(&position); scanErr != nil {
					return scanErr
				}
				allowedPositions = append(allowedPositions, position)
			}
			return rows.Err()
		}()
		if err != nil {
			return err
		}
		result.Candidates = make([]RevalidatedSearchCandidate, len(allowedPositions))
		ids := make([]int64, len(allowedPositions))
		for i, position := range allowedPositions {
			candidate := candidates[position]
			ids[i] = candidate.NodeID
			result.Candidates[i].SearchCandidateIdentity = candidate
		}
		// A single survivor needs no batch query or position mapping.
		if len(ids) == 1 {
			result.Candidates[0].Path, err = pathOf(ctx, tx, ids[0])
			return err
		}
		paths, pathErr := s.pathsOf(ctx, tx, ids)
		if pathErr != nil {
			return pathErr
		}
		for i, path := range paths {
			result.Candidates[i].Path = path
		}
		return nil
	})
	if err != nil {
		return SearchCandidateRevalidation{}, err
	}
	return result, nil
}

func searchCandidateRevalidationSQL(filterSQL, renditionEvidenceSQL string, opts SearchOptions) string {
	selectedEvidenceSQL := ""
	if len(opts.SelectedBuilds) != 0 {
		selectedEvidenceSQL = ` OR NOT EXISTS (SELECT 1 FROM json_each(?) selected
   WHERE json_extract(selected.value,'$.content_version_id')=source.version_id
    AND json_extract(selected.value,'$.build_id')=json_extract(evidence.value,'$.build_id')) `
	}

	differentSourceSQL := "source.node_id<>scoped.node_id"
	if opts.CollapsesPhotoDuplicates() {
		differentSourceSQL += " AND source.blob_hash<>scoped.blob_hash"
	}
	return `WITH requested AS (
			SELECT CAST(key AS INTEGER) AS position,
			       CAST(json_extract(value,'$.node_id') AS INTEGER) AS node_id,
			       json_extract(value,'$.version_id') AS version_id,
			       json_extract(value,'$.evidence') AS evidence
			FROM json_each(?)
		), identities AS (
			SELECT node_id,version_id FROM requested
			UNION
			SELECT CAST(json_extract(evidence.value,'$.node_id') AS INTEGER),
			       json_extract(evidence.value,'$.version_id')
			FROM requested,json_each(requested.evidence) evidence
		), live AS (
			SELECT n.id AS node_id,cv.version_id,cv.blob_hash,n.name AS current_name
			FROM identities JOIN nodes n ON n.id=identities.node_id
			JOIN content_versions cv ON cv.node_id=n.id AND cv.version_id=n.current_version_id
			 AND cv.version_id=identities.version_id
			WHERE n.kind='file' AND n.trashed_at IS NULL ` + filterSQL + `
		), scoped AS (
			SELECT requested.*,live.blob_hash FROM requested JOIN live
			 ON live.node_id=requested.node_id AND live.version_id=requested.version_id
		)
		SELECT scoped.position FROM scoped
		WHERE NOT EXISTS (
			SELECT 1 FROM json_each(scoped.evidence) evidence
			LEFT JOIN live source ON source.node_id=CAST(json_extract(evidence.value,'$.node_id') AS INTEGER)
			 AND source.version_id=json_extract(evidence.value,'$.version_id')
			WHERE source.node_id IS NULL OR (` + differentSourceSQL + `) ` + selectedEvidenceSQL + ` OR CASE json_extract(evidence.value,'$.kind')
			` + nameEvidenceSQL(opts.photoLexical) + `
			WHEN 'content_blob' THEN NOT (
				COALESCE(json_extract(evidence.value,'$.blob_hash'),'')<>'' AND
				json_extract(evidence.value,'$.blob_hash')=source.blob_hash)
			` + renditionEvidenceSQL + `
			WHEN 'embedding' THEN NOT EXISTS (
				SELECT 1 FROM embedding_heads eh
				JOIN embedding_sets es ON es.embedding_set_id=eh.embedding_set_id
				 AND es.content_version_id=eh.content_version_id AND es.binding_id=eh.binding_id
				 AND es.input_kind=eh.input_kind AND es.vector_space_id=eh.vector_space_id
				 AND es.profile_fingerprint=eh.profile_fingerprint
				JOIN embedding_vector_rows evr ON evr.vector_set_id=es.vector_set_id
				JOIN embedding_generation_inputs egi ON egi.generation_id=es.input_generation_id
				 AND egi.input_id=evr.input_id AND egi.rendered_checksum=evr.checksum
				JOIN embedding_input_generations eig ON eig.generation_id=es.input_generation_id
				WHERE eh.content_version_id=source.version_id
				 AND eh.vector_space_id=json_extract(evidence.value,'$.vector_space_id')
				 AND eh.embedding_set_id=json_extract(evidence.value,'$.embedding_set_id')
				 AND eh.input_kind=json_extract(evidence.value,'$.input_kind')
				 AND es.profile_fingerprint=? AND es.binding_id=?
				 AND es.input_generation_id=json_extract(evidence.value,'$.input_generation_id')
                 AND (COALESCE(json_extract(evidence.value,'$.build_id'),'')='' OR EXISTS (
                  SELECT 1 FROM rendition_attachments selected_ra
                  WHERE selected_ra.attachment_id=eig.attachment_id
                   AND selected_ra.build_id=json_extract(evidence.value,'$.build_id')))
				 AND evr.input_id=json_extract(evidence.value,'$.input_id')
				 AND (es.input_kind='original_file' OR (es.input_kind='image' AND ` + currentImageEmbeddingSQL("es.content_version_id", "eig.evidence_fingerprint") + `) OR (es.input_kind='rendition_chunk' AND ` + currentRenditionChunkAuthoritySQL + `)))
			ELSE 1 END
		) ORDER BY scoped.position`
}

// SemanticSearchCandidate is one vector neighbor reduced to a current,
// scope-eligible document with bounded original-file text when available.
// Retrieval resolves rendition-chunk text from the exact retained input.
type SemanticSearchCandidate struct {
	BlobHash          string
	modifiedAt        string
	VaultID           string
	NodeID            int64
	NodeRevision      int64
	ContentVersionID  string
	Path              string
	VectorSpaceID     string
	EmbeddingSetID    string
	InputGenerationID string
	InputID           string
	InputKind         document.EmbeddingInputKind
	Score             float64
	Excerpt           string
	MediaEvidence     SearchMediaEvidence
}

// SemanticSearchResolution binds ranked candidates to the
// current-head snapshot after query embedding and vector search complete.
type SemanticSearchResolution struct {
	SourceManifestChecksum string
	Candidates             []SemanticSearchCandidate
	Truncated              bool
	// NextNeighbor is the next index in the supplied neighbors, or their length when exhausted.
	NextNeighbor int
}

// SemanticSearchAuthority pins the exact persisted vector-space descriptor and
// one active local index generation for a query. WholeSpace permits scoring the
// entire generation; otherwise ANNRows limits scoring to the source-fenced scope.
type SemanticSearchAuthority struct {
	VectorSpace     EmbeddingVectorSpaceRecord
	Lease           VectorIndexReaderLease
	InputKind       document.EmbeddingInputKind
	BindingRequired bool
	ANNRows         []vectorindex.RowIdentity
	WholeSpace      bool
}

// AcquireSemanticSearchAuthority resolves the query contract from durable E1
// authority, never from a runtime default, and leases one exact active index.
func (s *Store) AcquireSemanticSearchAuthority(ctx context.Context, profileFingerprint,
	bindingID, owner string, at time.Time, duration time.Duration, opts SearchOptions,
) (SemanticSearchAuthority, error) {
	return s.acquireSemanticSearchAuthority(ctx, profileFingerprint, bindingID, owner, at, duration, opts, nil)
}

func (s *Store) acquireSemanticSearchAuthority(ctx context.Context, profileFingerprint,
	bindingID, owner string, at time.Time, duration time.Duration, opts SearchOptions, similar *similarSourceCapture,
) (SemanticSearchAuthority, error) {
	normalized, err := s.NormalizeSearchOptions(ctx, opts)
	if err != nil {
		return SemanticSearchAuthority{}, err
	}
	binding, fingerprints, err := embeddingProfileBindingAuthority(ctx, s.db, profileFingerprint, bindingID)
	if errors.Is(err, ErrNotFound) && fingerprints.Profile == "" {
		// A configured profile can be searched before processing persists it.
		return SemanticSearchAuthority{}, ErrVectorIndexUnavailable
	}
	if err != nil {
		return SemanticSearchAuthority{}, err
	}
	vectorSpaceID := fingerprints.VectorSpace[bindingID]
	if normalized.VectorSpaceID != "" {
		vectorSpaceID = normalized.VectorSpaceID
	}
	if similar != nil {
		err := s.withReadTx(ctx, func(tx *sql.Tx) error {
			return similar.load(ctx, tx, profileFingerprint, bindingID, binding.InputKind, vectorSpaceID, normalized)
		})
		if err != nil {
			return SemanticSearchAuthority{}, err
		}
	}
	space, err := loadVectorSpaceTx(ctx, s.db, vectorSpaceID)
	if errors.Is(err, ErrNotFound) {
		return SemanticSearchAuthority{}, ErrVectorIndexUnavailable
	}
	if err != nil {
		return SemanticSearchAuthority{}, err
	}
	if space.ID != vectorSpaceID || (space.ContractVersion == EmbeddingVectorSpaceContractV1 && space.DescriptorFingerprint != binding.Descriptor.Fingerprint) ||
		space.CompatibilityID != binding.CompatibilityID || space.Dimensions != binding.Dimensions ||
		space.Metric != binding.Metric || space.Normalization != binding.Normalization ||
		space.ScalarEncoding != binding.ScalarEncoding || space.DocumentFormatter != binding.DocumentFormatter ||
		space.QueryFormatter != binding.QueryFormatter || space.ModelInputFingerprint != binding.ModelInput.Fingerprint {
		return SemanticSearchAuthority{}, errors.New("semantic search vector space does not match profile binding authority")
	}
	lease, err := s.AcquireVectorIndexGeneration(ctx, vectorSpaceID, owner, at, duration)
	if errors.Is(err, ErrNotFound) {
		return SemanticSearchAuthority{}, ErrVectorIndexUnavailable
	}
	if err != nil {
		return SemanticSearchAuthority{}, err
	}
	release := func() {
		_ = s.ReleaseVectorIndexGeneration(context.WithoutCancel(ctx), lease.ID, lease.FencingToken, at)
	}
	current, annRows, wholeSpace, err := s.semanticSearchAuthorityFence(ctx,
		profileFingerprint, bindingID, binding.InputKind, vectorSpaceID, normalized, similar)
	if err != nil || current != lease.Generation.SourceManifestChecksum {
		release()
		if err != nil {
			return SemanticSearchAuthority{}, err
		}
		return SemanticSearchAuthority{}, ErrVectorIndexSourceStale
	}
	if normalized.EmbeddingDescriptor != nil {
		descriptor := *normalized.EmbeddingDescriptor
		if descriptor.Fingerprint != binding.Descriptor.Fingerprint || !sameEmbeddingVectorSemantics(space.Descriptor, descriptor) {
			release()
			return SemanticSearchAuthority{}, errors.New("query runtime differs from profile")
		}
		space.Descriptor = descriptor
	}
	return SemanticSearchAuthority{VectorSpace: space, Lease: lease, InputKind: binding.InputKind,
		BindingRequired: binding.Activation == document.EmbeddingRequired,
		ANNRows:         annRows, WholeSpace: wholeSpace}, nil
}

func (s *Store) semanticSearchAuthorityFence(ctx context.Context, profileFingerprint, bindingID string,
	inputKind document.EmbeddingInputKind, vectorSpaceID string, opts SearchOptions, similar *similarSourceCapture,
) (source string, annRows []vectorindex.RowIdentity, wholeSpace bool, retErr error) {
	err := s.withSearchOptionsTx(ctx, opts, func(tx *sql.Tx, opts SearchOptions) error {
		var err error
		if similar != nil {
			if err := similar.load(ctx, tx, profileFingerprint, bindingID, inputKind, vectorSpaceID, opts); err != nil {
				return err
			}
		}
		source, err = s.vectorIndexSourceChecksumTx(ctx, tx, vectorSpaceID)
		if err != nil {
			return err
		}
		annRows, wholeSpace, err = semanticANNRowsForSourceTx(ctx, tx, profileFingerprint, bindingID, inputKind, vectorSpaceID, opts)
		return err
	})
	return source, annRows, wholeSpace, err
}

func semanticANNRowsForSourceTx(ctx context.Context, tx metadataQuerier, profile, binding string,
	kind document.EmbeddingInputKind, space string, opts SearchOptions,
) ([]vectorindex.RowIdentity, bool, error) {
	filterSQL, filterArgs := semanticSearchFilterSQL(opts)
	if filterSQL == "" && !opts.CollapsesPhotoDuplicates() {
		return nil, true, nil
	}
	rows, err := semanticANNRowsTx(ctx, tx, profile, binding, kind, space, opts, filterSQL, filterArgs)
	return rows, false, err
}

// semanticANNRowsTx reads eligible rows in the requested scope for SearchRows.
// This allowlist excludes other bindings, trashed, non-current,
// out-of-scope, and non-representative rows before scoring.
func semanticANNRowsTx(ctx context.Context, tx metadataQuerier, profileFingerprint, bindingID string,
	inputKind document.EmbeddingInputKind, vectorSpaceID string, opts SearchOptions, filterSQL string, filterArgs []any,
) ([]vectorindex.RowIdentity, error) {
	annRows := make([]vectorindex.RowIdentity, 0)
	var lastHash string
	var representative int64
	err := scanSemanticRows(ctx, tx, profileFingerprint, bindingID, inputKind, vectorSpaceID,
		filterSQL, filterArgs, nil, len(opts.ContentVersionIDs) != 0 && len(opts.SelectedBuilds) == 0, opts.CollapsesPhotoDuplicates(), func(key semanticEligibilityKey, nodeID int64, hash string) {
			if opts.CollapsesPhotoDuplicates() {
				if hash != lastHash {
					lastHash, representative = hash, nodeID
				}
				if nodeID != representative {
					return
				}
			}
			annRows = append(annRows, vectorindex.RowIdentity{SetID: key.VectorSetID, InputKey: key.InputID, InputChecksum: key.InputChecksum})
		})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(annRows, vectorindex.CompareIdentity)
	uniqueRows := slices.Compact(annRows)
	annRows = make([]vectorindex.RowIdentity, len(uniqueRows))
	copy(annRows, uniqueRows)
	return annRows, nil
}

// LiveSearchScopeExists reports whether normalized search filters match a live file.
func (s *Store) LiveSearchScopeExists(ctx context.Context, opts SearchOptions) (bool, error) {
	scopeSQL, args := liveSearchScopeSQL(searchFilterSQL(opts))
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 `+scopeSQL+` LIMIT 1)`, args...).Scan(&exists)
	return exists, err
}

func liveSearchScopeCount(ctx context.Context, q metadataQuerier, opts SearchOptions) (int, error) {
	scopeSQL, args := liveSearchScopeSQL(searchFilterSQL(opts))
	var count int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) `+scopeSQL, args...).Scan(&count)
	return count, err
}

func liveSearchScopeSQL(filterSQL string, filterArgs []any) (string, []any) {
	return `FROM ` + nodeFrom + `
		WHERE n.kind='file' AND n.trashed_at IS NULL AND cv.version_id IS NOT NULL ` + filterSQL, filterArgs
}

// currentEmbeddingHeadsSQL keeps coverage and semantic retrieval on the same
// current-source, binding and rendition authority.
func currentEmbeddingHeadsSQL() string {
	return `FROM ` + nodeFrom + `
		JOIN embedding_heads eh ON eh.content_version_id=cv.version_id
		JOIN embedding_sets es ON es.embedding_set_id=eh.embedding_set_id
		 AND es.content_version_id=eh.content_version_id AND es.binding_id=eh.binding_id
		 AND es.input_kind=eh.input_kind AND es.vector_space_id=eh.vector_space_id
		 AND es.profile_fingerprint=eh.profile_fingerprint
		JOIN embedding_input_generations eig ON eig.generation_id=es.input_generation_id
		WHERE n.kind='file' AND n.trashed_at IS NULL
		  AND (es.input_kind='original_file' OR (es.input_kind='image' AND ` + currentImageEmbeddingSQL("es.content_version_id", "eig.evidence_fingerprint") + `) OR (es.input_kind='rendition_chunk' AND ` + currentRenditionChunkAuthoritySQL + `))`
}

// semanticSearchFilterSQL fences retained embedding inputs to exact selected
// builds before ANN scoring and candidate reduction. It deliberately retains
// the ordinary profile/binding/current-rendition predicates.
func semanticSearchFilterSQL(opts SearchOptions) (string, []any) {
	filter, args := searchFilterSQL(opts)
	if len(opts.SelectedBuilds) == 0 {
		return filter, args
	}
	encoded, err := json.Marshal(opts.SelectedBuilds)
	if err != nil {
		panic(err)
	}
	filter += ` AND es.input_kind='rendition_chunk' AND EXISTS (
  SELECT 1 FROM json_each(?) selected
  JOIN rendition_attachments selected_ra ON selected_ra.attachment_id=eig.attachment_id
   AND selected_ra.content_version_id=es.content_version_id
  WHERE json_extract(selected.value,'$.content_version_id')=es.content_version_id
   AND json_extract(selected.value,'$.build_id')=selected_ra.build_id)`
	return filter, append(args, string(encoded))
}

func selectedSemanticCompleteVersionsTx(ctx context.Context, tx metadataQuerier, profileFingerprint, bindingID string,
	inputKind document.EmbeddingInputKind, vectorSpaceID string, opts SearchOptions,
) (_ []string, retErr error) {
	filterSQL, filterArgs := semanticSearchFilterSQL(opts)
	if len(opts.SelectedBuilds) != 0 {
		encoded, err := json.Marshal(opts.SelectedBuilds)
		if err != nil {
			return nil, err
		}
		// A content version is complete only when every selected build is embedded.
		filterSQL += ` AND NOT EXISTS (SELECT 1 FROM json_each(?) selected
   WHERE json_extract(selected.value,'$.content_version_id')=es.content_version_id
    AND NOT EXISTS (SELECT 1 FROM rendition_attachments selected_ra
     WHERE selected_ra.attachment_id=eig.attachment_id
      AND selected_ra.build_id=json_extract(selected.value,'$.build_id')))`
		filterArgs = append(filterArgs, string(encoded))
	}

	args := append([]any{profileFingerprint, bindingID, inputKind, vectorSpaceID}, filterArgs...)
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT cv.version_id `+currentEmbeddingHeadsSQL()+`
  AND eh.profile_fingerprint=? AND eh.binding_id=? AND eh.input_kind=? AND eh.vector_space_id=? `+filterSQL+` ORDER BY cv.version_id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	result := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, rows.Err()
}

func semanticSearchCoverageTx(ctx context.Context, tx metadataQuerier, profileFingerprint, bindingID string,
	inputKind document.EmbeddingInputKind, vectorSpaceID string, opts SearchOptions,
) (required, complete int, retErr error) {
	required, err := liveSearchScopeCount(ctx, tx, opts)
	if err != nil {
		return 0, 0, err
	}
	if len(opts.SelectedBuilds) != 0 {
		versions, err := selectedSemanticCompleteVersionsTx(ctx, tx, profileFingerprint, bindingID, inputKind, vectorSpaceID, opts)
		return required, len(versions), err
	}
	filterSQL, filterArgs := searchFilterSQL(opts)
	args := append([]any{profileFingerprint, bindingID, inputKind, vectorSpaceID}, filterArgs...)
	err = tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT n.id) `+currentEmbeddingHeadsSQL()+`
  AND eh.profile_fingerprint=? AND eh.binding_id=? AND eh.input_kind=? AND eh.vector_space_id=? `+filterSQL, args...).Scan(&complete)
	return required, complete, err
}

// ResolveSemanticCandidates applies current-version, live-head, and operator
// scope fencing before reducing ordered vector rows to bounded documents.
func (s *Store) ResolveSemanticCandidates(ctx context.Context, profileFingerprint, bindingID string,
	inputKind document.EmbeddingInputKind, vectorSpaceID, expectedSourceManifest string,
	neighbors []vectorindex.Neighbor, limit int, opts SearchOptions, excludedNodes map[int64]struct{},
) (_ SemanticSearchResolution, retErr error) {
	if err := validateCatalogSHA256(vectorSpaceID, "semantic search vector-space ID"); err != nil {
		return SemanticSearchResolution{}, err
	}
	if err := validateCatalogSHA256(expectedSourceManifest, "semantic search source manifest"); err != nil {
		return SemanticSearchResolution{}, err
	}
	if limit < 1 || limit > document.MaxRetrievalCandidateLimit {
		return SemanticSearchResolution{}, fmt.Errorf("semantic search limit must be between 1 and %d", document.MaxRetrievalCandidateLimit)
	}
	normalized, err := s.NormalizeSearchOptions(ctx, opts)
	if err != nil {
		return SemanticSearchResolution{}, err
	}
	var result SemanticSearchResolution
	err = s.withSearchOptionsTx(ctx, normalized, func(tx *sql.Tx, normalized SearchOptions) error {
		current, captureErr := s.vectorIndexSourceChecksumTx(ctx, tx, vectorSpaceID)
		if captureErr != nil {
			if errors.Is(captureErr, ErrNotFound) {
				return ErrVectorIndexSourceStale
			}
			return captureErr
		}
		if current != expectedSourceManifest {
			return ErrVectorIndexSourceStale
		}
		result.SourceManifestChecksum = current
		return s.semanticCandidatesTx(ctx, tx, profileFingerprint, bindingID, inputKind, vectorSpaceID, neighbors, limit, normalized, excludedNodes, &result)
	})
	if err != nil {
		return SemanticSearchResolution{}, err
	}
	return result, nil
}

func (s *Store) semanticCandidatesTx(ctx context.Context, tx metadataQuerier, profileFingerprint, bindingID string,
	inputKind document.EmbeddingInputKind, vectorSpaceID string, neighbors []vectorindex.Neighbor, limit int,
	normalized SearchOptions, excludedNodes map[int64]struct{}, result *SemanticSearchResolution,
) error {
	var err error
	filterSQL, filterArgs := semanticSearchFilterSQL(normalized)
	eligible := make(map[semanticEligibilityKey]SemanticSearchCandidate)
	representatives := make(map[string]int64)
	prefix, walked := min(limit+1, len(neighbors)), 0
	for {
		members, err := loadSemanticMemberships(ctx, tx, profileFingerprint, bindingID, inputKind, vectorSpaceID,
			filterSQL, filterArgs, semanticNeighborKeys(neighbors[walked:prefix]))
		if err != nil {
			return err
		}
		if normalized.CollapsesPhotoDuplicates() {
			hashes := []string{}
			for _, entries := range members {
				for _, entry := range entries {
					if _, seen := representatives[entry.BlobHash]; !seen {
						representatives[entry.BlobHash] = 0
						hashes = append(hashes, entry.BlobHash)
					}
				}
			}
			err := scanSemanticRows(ctx, tx, profileFingerprint, bindingID, inputKind, vectorSpaceID,
				filterSQL, filterArgs, hashes, false, true, func(_ semanticEligibilityKey, nodeID int64, hash string) {
					if representatives[hash] == 0 {
						representatives[hash] = nodeID
					}
				})
			if err != nil {
				return err
			}
		}
		for key, entries := range members {
			for _, entry := range entries {
				if !normalized.CollapsesPhotoDuplicates() || entry.NodeID == representatives[entry.BlobHash] {
					eligible[key] = entry
				}
			}
		}
		result.Candidates, result.Truncated, result.NextNeighbor = reduceSemanticCandidates(s.vaultID, vectorSpaceID,
			neighbors[:prefix], limit, eligible, excludedNodes)
		if result.Truncated || prefix == len(neighbors) {
			break
		}
		walked, prefix = prefix, min(prefix*2, len(neighbors))
	}
	for index := range result.Candidates {
		candidate := &result.Candidates[index]
		candidate.Path, err = pathOf(ctx, tx, candidate.NodeID)
		if err != nil {
			return err
		}
		if candidate.InputKind == document.EmbeddingInputOriginalFile {
			candidate.Excerpt, err = searchExcerptByVersionTx(ctx, tx, candidate.ContentVersionID)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func searchExcerptByVersionTx(ctx context.Context, queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, versionID string) (string, error) {
	var excerpt string
	err := queryer.QueryRowContext(ctx, `SELECT substr(text, 1, ?) FROM extracted_text
		WHERE status='ok' AND blob_hash=(SELECT blob_hash FROM content_versions WHERE version_id=? LIMIT 1)
		ORDER BY extractor LIMIT 1`, maxExplainedSearchExcerptRunes, versionID).Scan(&excerpt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return boundedExplainedSearchExcerpt(excerpt), nil
}

type semanticEligibilityKey struct {
	VectorSetID   string `json:"VectorSetID"`
	InputID       string `json:"InputID"`
	InputChecksum string `json:"InputChecksum"`
}

// semanticNeighborKeys preserves the unique row identities returned by vector search.
func semanticNeighborKeys(neighbors []vectorindex.Neighbor) []semanticEligibilityKey {
	keys := make([]semanticEligibilityKey, len(neighbors))
	for i, neighbor := range neighbors {
		keys[i] = semanticEligibilityKey{neighbor.SetID, neighbor.InputKey, neighbor.InputChecksum}
	}
	return keys
}

// semanticMembershipSQL shares current-source and input authority across keyed
// candidate loads and streaming row scans. The caller chooses the driving index.
func semanticMembershipSQL(driver string) string {
	return driver + `
 JOIN content_versions cv ON cv.version_id=es.content_version_id
 JOIN nodes n ON n.id=cv.node_id
 JOIN embedding_heads eh ON eh.content_version_id=es.content_version_id
  AND eh.binding_id=es.binding_id AND eh.input_kind=es.input_kind
  AND eh.embedding_set_id=es.embedding_set_id AND eh.vector_space_id=es.vector_space_id
  AND eh.profile_fingerprint=es.profile_fingerprint
 JOIN embedding_vector_rows evr ON evr.vector_set_id=es.vector_set_id
 JOIN embedding_generation_inputs egi ON egi.generation_id=es.input_generation_id
  AND egi.input_id=evr.input_id AND egi.rendered_checksum=evr.checksum
 JOIN embedding_input_generations eig ON eig.generation_id=es.input_generation_id
 LEFT JOIN rendition_attachments ra ON ra.attachment_id=eig.attachment_id
  AND ra.content_version_id=es.content_version_id AND ra.profile_fingerprint=es.profile_fingerprint
 LEFT JOIN blobs evidence_blob ON evidence_blob.hash=eig.evidence_fingerprint
 WHERE es.vector_space_id=?
  AND es.profile_fingerprint=? AND es.binding_id=? AND es.input_kind=?
  AND n.current_version_id=es.content_version_id AND n.trashed_at IS NULL
  AND (es.input_kind='original_file' OR (es.input_kind='image' AND ` + currentImageEmbeddingSQL("es.content_version_id", "eig.evidence_fingerprint") + `) OR (es.input_kind='rendition_chunk' AND ` + currentRenditionChunkAuthoritySQL + `))
 `
}

// scanSemanticRows reads the whole space when blobHashes is nil, else only
// rows whose current content has one of those hashes.
func scanSemanticRows(ctx context.Context, tx metadataQuerier, profile, binding string,
	kind document.EmbeddingInputKind, space, filterSQL string, filterArgs []any,
	blobHashes []string, versionFence, representativeOrder bool, visit func(semanticEligibilityKey, int64, string),
) (retErr error) {
	driver := `FROM embedding_sets es`
	prefix := ""
	var args []any
	if blobHashes != nil {
		encoded, err := json.Marshal(blobHashes)
		if err != nil {
			return err
		}
		args = append(args, string(encoded))
		driver = `FROM json_each(?) k CROSS JOIN content_versions cv_key INDEXED BY content_versions_blob ON cv_key.blob_hash=k.value
   CROSS JOIN embedding_sets es ON es.content_version_id=cv_key.version_id`
	} else if versionFence {
		// Explicit version fences are bounded to 4096 IDs. Other filters keep
		// the embedding scan so unembedded files do not drive their cost.
		prefix = `WITH scoped_versions AS MATERIALIZED (
   SELECT cv.version_id FROM content_versions cv
   CROSS JOIN nodes n ON n.id=cv.node_id WHERE 1=1 ` + filterSQL + `) `
		driver = `FROM scoped_versions scope CROSS JOIN embedding_sets es INDEXED BY embedding_sets_content_version
   ON es.content_version_id=scope.version_id`
		args = append(args, filterArgs...)
		filterSQL, filterArgs = "", nil
	}
	args = append(args, space, profile, binding, kind)
	args = append(args, filterArgs...)
	order := ""
	if representativeOrder {
		order = " ORDER BY cv.blob_hash," + DuplicateRepresentativeOrder
	}
	rows, err := tx.QueryContext(ctx, prefix+`SELECT evr.vector_set_id,evr.input_id,evr.checksum,n.id AS node_id,cv.blob_hash `+
		semanticMembershipSQL(driver)+filterSQL+order, args...)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	for rows.Next() {
		var key semanticEligibilityKey
		var nodeID int64
		var hash string
		if err := rows.Scan(&key.VectorSetID, &key.InputID, &key.InputChecksum, &nodeID, &hash); err != nil {
			return err
		}
		visit(key, nodeID, hash)
	}
	return rows.Err()
}

func loadSemanticMemberships(ctx context.Context, tx metadataQuerier, profileFingerprint, bindingID string,
	inputKind document.EmbeddingInputKind, vectorSpaceID, filterSQL string, filterArgs []any, keys []semanticEligibilityKey,
) (_ map[semanticEligibilityKey][]SemanticSearchCandidate, retErr error) {
	encoded, err := json.Marshal(keys)
	if err != nil {
		return nil, err
	}
	args := append([]any{string(encoded), vectorSpaceID, profileFingerprint, bindingID, inputKind}, filterArgs...)
	rows, err := tx.QueryContext(ctx, `SELECT n.id,n.revision,n.current_version_id,
			es.embedding_set_id,es.input_generation_id,es.input_kind,
			evr.vector_set_id,evr.input_id,evr.checksum,
			COALESCE(ra.build_id,''),COALESCE(eig.generation_blob_hash,''),
			eig.generation_encoded_size,eig.generation_checksum,eig.evidence_fingerprint,
			COALESCE(evidence_blob.size,0),eig.input_count,cv.blob_hash,n.modified_at
  `+semanticMembershipSQL(`FROM json_each(?) k CROSS JOIN embedding_sets es INDEXED BY embedding_sets_vector_set ON es.vector_set_id=json_extract(k.value,'$.VectorSetID')`)+
		` AND evr.input_id=json_extract(k.value,'$.InputID') AND evr.checksum=json_extract(k.value,'$.InputChecksum')`+
		filterSQL+` ORDER BY es.vector_set_id,es.embedding_set_id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	eligible := make(map[semanticEligibilityKey][]SemanticSearchCandidate)
	for rows.Next() {
		var (
			entry SemanticSearchCandidate
			key   semanticEligibilityKey
		)
		if err := rows.Scan(&entry.NodeID, &entry.NodeRevision, &entry.ContentVersionID, &entry.EmbeddingSetID,
			&entry.InputGenerationID, &entry.InputKind, &key.VectorSetID, &key.InputID, &key.InputChecksum,
			&entry.MediaEvidence.BuildID, &entry.MediaEvidence.GenerationBlobHash,
			&entry.MediaEvidence.GenerationEncodedSize, &entry.MediaEvidence.GenerationChecksum,
			&entry.MediaEvidence.EvidenceFingerprint, &entry.MediaEvidence.EvidenceEncodedSize,
			&entry.MediaEvidence.InputCount, &entry.BlobHash, &entry.modifiedAt); err != nil {
			return nil, err
		}
		eligible[key] = append(eligible[key], entry)
	}
	return eligible, rows.Err()
}

func reduceSemanticCandidates(vaultID, vectorSpaceID string, neighbors []vectorindex.Neighbor,
	limit int, eligible map[semanticEligibilityKey]SemanticSearchCandidate, excludedNodes map[int64]struct{},
) ([]SemanticSearchCandidate, bool, int) {
	seen := make(map[int64]struct{}, limit+1)
	candidates := make([]SemanticSearchCandidate, 0, min(limit+1, len(eligible)))
	for index, neighbor := range neighbors {
		entry, ok := eligible[semanticEligibilityKey{VectorSetID: neighbor.SetID,
			InputID: neighbor.InputKey, InputChecksum: neighbor.InputChecksum}]
		if !ok {
			continue
		}
		if _, excluded := excludedNodes[entry.NodeID]; excluded {
			continue
		}
		if _, duplicate := seen[entry.NodeID]; duplicate {
			continue
		}
		seen[entry.NodeID] = struct{}{}
		candidate := entry
		candidate.VaultID = vaultID
		candidate.VectorSpaceID = vectorSpaceID
		candidate.InputID = neighbor.InputKey
		candidate.Score = neighbor.Score
		candidates = append(candidates, candidate)
		if len(candidates) == limit+1 {
			return candidates[:limit], true, index
		}
	}
	return candidates, false, len(neighbors)
}

const (
	SearchMatchName    = "name"
	SearchMatchContent = "content"
	SearchMatchFilter  = "filter"
)

// ErrSearchQueryRequired reports an unanchored search without query text.
// Queryless search requires a tag or modification-time filter. The result
// limit bounds the response, not the database work.
var ErrSearchQueryRequired = errors.New(
	"search query is required unless a tag or modification-time bound is supplied",
)

// LexicalGeneration identifies one complete, immutable FTS projection. Rows
// remain unreachable until rendition and lexical heads are flipped together.
type LexicalGeneration struct {
	ID             string
	SegmentCount   int
	ManifestDigest string
	BuildCount     int
}

// ErrLexicalGenerationStale reports a complete immutable generation that no
// longer covers every rendition head current in the publication transaction.
// Callers may safely stage a fresh generation; no provider egress is needed.
var ErrLexicalGenerationStale = errors.New("lexical generation is stale")

// LexicalGenerationRoot is one exact immutable generation currently retained
// by in-process readers. Task 8 garbage collection consumes these roots in
// addition to the active database head.
type LexicalGenerationRoot struct {
	GenerationID string
	ReaderCount  int
}

// LexicalGenerationLease pins one exact generation until Release. Generation
// does not follow later head flips.
type LexicalGenerationLease struct {
	Generation LexicalGeneration
	store      *Store
	released   bool
}

// lexicalGenerationReaders tracks live reader leases per store. The mutex
// guards only the map and is held briefly, never while waiting on SQLite.
// Ordering against collection comes from the store transaction: a standalone lease
// is pinned inside a write transaction, so it serializes with the
// publication or purge that snapshots pins and deletes generations, and a
// read-bound lease is pinned inside the read snapshot it serves.
var lexicalGenerationReaders = struct {
	sync.Mutex

	stores map[*Store]map[string]int
}{stores: make(map[*Store]map[string]int)}

// RecordRenditionBlob grants catalog authority to one verified Docbank blob
// receipt without creating a document version or conferring visibility.
func (s *Store) RecordRenditionBlob(
	ctx context.Context, hash string, size int64, physical BlobPhysical,
) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if err := s.EnsureBlobTx(ctx, tx, hash, size, physical); err != nil {
			return err
		}
		if physical.Created {
			if _, err := tx.ExecContext(ctx, `INSERT INTO rendition_blob_staging(blob_hash)
				VALUES($1) ON CONFLICT(blob_hash) DO NOTHING`, hash); err != nil {
				return fmt.Errorf("marking rendition blob %s as uncommitted staging: %w", hash, err)
			}
		}
		return nil
	})
}

// StageLexicalGeneration builds a complete unreachable lexical projection over
// every immutable rendition build currently staged in this vault.
func (s *Store) StageLexicalGeneration(
	ctx context.Context, generationID string,
) (LexicalGeneration, error) {
	if err := validateCatalogSHA256(generationID, "lexical generation ID"); err != nil {
		return LexicalGeneration{}, err
	}
	var generation LexicalGeneration
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var err error
		generation, err = stageLexicalGenerationTx(ctx, tx, generationID)
		return err
	})
	if err != nil {
		return LexicalGeneration{}, err
	}
	return generation, nil
}

// StageLexicalGenerationWithRoot atomically records a complete immutable
// projection and the exact fenced authority protecting it from maintenance.
func (s *Store) StageLexicalGenerationWithRoot(
	ctx context.Context, generationID string, root CurrentRenditionRoot,
) (LexicalGeneration, error) {
	if err := validateCatalogSHA256(generationID, "lexical generation ID"); err != nil {
		return LexicalGeneration{}, err
	}
	if err := validateCurrentRenditionRoot(root); err != nil {
		return LexicalGeneration{}, err
	}
	if root.TargetKind != RenditionRootLexicalGeneration || root.TargetID != generationID {
		return LexicalGeneration{}, errors.New(
			"rooted lexical generation requires a root for the staged generation")
	}
	var generation LexicalGeneration
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var err error
		generation, err = stageLexicalGenerationTx(ctx, tx, generationID)
		if err != nil {
			return err
		}
		return putCurrentRenditionRootTx(ctx, tx, root)
	})
	if err != nil {
		return LexicalGeneration{}, err
	}
	return generation, nil
}

func stageLexicalGenerationTx(
	ctx context.Context, tx *sql.Tx, generationID string,
) (LexicalGeneration, error) {
	segments, err := readCatalogLexicalManifestRowsTx(ctx, tx, "")
	if err != nil {
		return LexicalGeneration{}, err
	}
	buildIDs, err := lexicalCatalogBuildIDsTx(ctx, tx)
	if err != nil {
		return LexicalGeneration{}, err
	}
	return stageLexicalGenerationRowsTx(ctx, tx, generationID, buildIDs, segments)
}

func stageLexicalGenerationExcludingTx(
	ctx context.Context, tx *sql.Tx, excludedBuildIDs map[string]struct{},
) (LexicalGeneration, error) {
	segments, err := readCatalogLexicalManifestRowsTx(ctx, tx, "")
	if err != nil {
		return LexicalGeneration{}, err
	}
	segments = slices.DeleteFunc(segments, func(row lexicalManifestRow) bool {
		_, excluded := excludedBuildIDs[row.buildID]
		return excluded
	})
	buildIDs, err := lexicalCatalogBuildIDsTx(ctx, tx)
	if err != nil {
		return LexicalGeneration{}, err
	}
	buildIDs = slices.DeleteFunc(buildIDs, func(buildID string) bool {
		_, excluded := excludedBuildIDs[buildID]
		return excluded
	})
	if len(buildIDs) == 0 {
		return LexicalGeneration{}, nil
	}
	generationID := lexicalReplacementGenerationID(segments, buildIDs)
	return stageLexicalGenerationRowsTx(ctx, tx, generationID, buildIDs, segments)
}

func stageLexicalGenerationRowsTx(
	ctx context.Context, tx *sql.Tx, generationID string,
	buildIDs []string,
	segments []lexicalManifestRow,
) (LexicalGeneration, error) {
	stored, err := loadAndValidateLexicalGenerationTx(ctx, tx, generationID)
	if err == nil {
		return stored, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return LexicalGeneration{}, err
	}
	if _, err := extendLexicalStreamTx(ctx, tx, uuid.New().String(), LexicalGeneration{}, buildIDs, segments, generationID, nowRFC3339()); err != nil {
		return LexicalGeneration{}, err
	}
	return loadAndValidateLexicalGenerationTx(ctx, tx, generationID)
}

// lexicalChain extends an ordered digest with one build, including empty builds.
func lexicalChain(previous, buildID string, segments []lexicalManifestRow) string {
	hash := sha256.New()
	ordered := slices.Clone(segments)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].segmentID != ordered[j].segmentID {
			return ordered[i].segmentID < ordered[j].segmentID
		}
		return ordered[i].text < ordered[j].text
	})
	for _, field := range []string{previous, buildID} {
		_, _ = io.WriteString(hash, strconv.Itoa(len(field))+":"+field)
	}
	for _, row := range ordered {
		for _, field := range []string{row.segmentID, row.text} {
			_, _ = io.WriteString(hash, strconv.Itoa(len(field))+":"+field)
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func insertLexicalGenerationTx(ctx context.Context, tx *sql.Tx, generation LexicalGeneration, streamID, builtAt string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO rendition_lexical_generations
		(generation_id,stream_id,segment_count,build_count,built_at) VALUES($1,$2,$3,$4,$5)`, generation.ID, streamID, generation.SegmentCount, generation.BuildCount, builtAt); err != nil {
		return fmt.Errorf("recording lexical generation %s: %w", generation.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO rendition_lexical_generation_manifests
		(generation_id,manifest_digest) VALUES($1,$2)`, generation.ID, generation.ManifestDigest); err != nil {
		return fmt.Errorf("recording lexical generation %s manifest: %w", generation.ID, err)
	}
	return nil
}

// extendLexicalStreamTx shares indexing and ordered membership for staging and append.
// A supplied generation ID preserves full-stage and metadata restore identities.
func extendLexicalStreamTx(ctx context.Context, tx *sql.Tx, streamID string, from LexicalGeneration, buildIDs []string, rows []lexicalManifestRow, generationID, builtAt string) (LexicalGeneration, error) {
	byBuild := make(map[string][]lexicalManifestRow)
	for _, row := range rows {
		byBuild[row.buildID] = append(byBuild[row.buildID], row)
	}
	generation := from
	for _, buildID := range buildIDs {
		segments := byBuild[buildID]
		if err := indexLexicalSegmentsTx(ctx, tx, segments); err != nil {
			return LexicalGeneration{}, err
		}
		generation.BuildCount++
		generation.SegmentCount += len(segments)
		generation.ManifestDigest = lexicalChain(generation.ManifestDigest, buildID, segments)
		if _, err := tx.ExecContext(ctx, `INSERT INTO rendition_lexical_stream_builds
			(stream_id,ordinal,build_id) VALUES($1,$2,$3)`, streamID, generation.BuildCount, buildID); err != nil {
			return LexicalGeneration{}, fmt.Errorf("recording lexical stream %s build %s: %w", streamID, buildID, err)
		}
	}
	if generationID == "" {
		digest := sha256.Sum256([]byte("docbank-lexical-append/v1\x00" + streamID + "\x00" +
			strconv.Itoa(generation.BuildCount) + "\x00" + generation.ManifestDigest))
		generationID = hex.EncodeToString(digest[:])
	}
	generation.ID = generationID
	if err := insertLexicalGenerationTx(ctx, tx, generation, streamID, builtAt); err != nil {
		return LexicalGeneration{}, err
	}
	return generation, nil
}

func appendLexicalGenerationTx(ctx context.Context, tx *sql.Tx, buildIDs []string) (LexicalGeneration, error) {
	head, err := readActiveLexicalGeneration(ctx, tx)
	if err != nil {
		return LexicalGeneration{}, err
	}
	var streamID string
	if err := tx.QueryRowContext(ctx, `SELECT stream_id FROM rendition_lexical_generations
		WHERE generation_id=$1`, head.ID).Scan(&streamID); err != nil {
		return LexicalGeneration{}, fmt.Errorf("reading lexical head stream: %w", err)
	}
	var added []string
	for _, buildID := range buildIDs {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM rendition_lexical_stream_builds
			WHERE stream_id=$1 AND build_id=$2 AND ordinal<=$3)`, streamID, buildID, head.BuildCount).Scan(&exists); err != nil {
			return LexicalGeneration{}, fmt.Errorf("checking lexical stream membership: %w", err)
		}
		if !exists {
			added = append(added, buildID)
		}
	}
	if len(added) == 0 {
		return head, nil
	}
	var tip int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(ordinal),0)
		FROM rendition_lexical_stream_builds WHERE stream_id=$1`, streamID).Scan(&tip); err != nil {
		return LexicalGeneration{}, fmt.Errorf("reading lexical stream tip: %w", err)
	}
	if tip > head.BuildCount {
		return LexicalGeneration{}, ErrNotFound
	}
	var segments []lexicalManifestRow
	for _, buildID := range added {
		rows, err := readCatalogLexicalManifestRowsTx(ctx, tx, buildID)
		if err != nil {
			return LexicalGeneration{}, err
		}
		segments = append(segments, rows...)
	}
	return extendLexicalStreamTx(ctx, tx, streamID, head, added, segments, "", nowRFC3339())
}

// dropLexicalGenerationTx trims only the tail no retained generation can read.
func dropLexicalGenerationTx(ctx context.Context, tx *sql.Tx, generationID string) (int, error) {
	var streamID string
	if err := tx.QueryRowContext(ctx, `SELECT stream_id FROM rendition_lexical_generations
		WHERE generation_id=$1`, generationID).Scan(&streamID); err != nil {
		return 0, fmt.Errorf("reading collected lexical stream: %w", err)
	}
	for _, statement := range []string{
		`DELETE FROM rendition_lexical_generation_manifests WHERE generation_id=$1`,
		`DELETE FROM rendition_lexical_generations WHERE generation_id=$1`,
	} {
		if _, err := tx.ExecContext(ctx, statement, generationID); err != nil {
			return 0, fmt.Errorf("collecting lexical generation %s: %w", generationID, err)
		}
	}
	var cutoff int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(build_count),0)
		FROM rendition_lexical_generations WHERE stream_id=$1`, streamID).Scan(&cutoff); err != nil {
		return 0, fmt.Errorf("reading retained lexical stream cutoff: %w", err)
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM rendition_lexical_index
		WHERE build_id IN (SELECT build_id FROM rendition_lexical_stream_builds
		                   WHERE stream_id=$1 AND ordinal>$2)
		AND NOT EXISTS (SELECT 1 FROM rendition_lexical_generation_builds gb
		                WHERE gb.build_id=rendition_lexical_index.build_id)`, streamID, cutoff)
	if err != nil {
		return 0, fmt.Errorf("collecting trimmed lexical index rows: %w", err)
	}
	removed, err := rowsAffectedInt(result)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM rendition_lexical_stream_builds
		WHERE stream_id=$1 AND ordinal>$2`, streamID, cutoff); err != nil {
		return 0, fmt.Errorf("trimming lexical stream membership: %w", err)
	}
	return removed, nil
}

func lexicalReplacementGenerationID(rows []lexicalManifestRow, buildIDs []string) string {
	digest := sha256.Sum256([]byte("docbank-lexical-gc-replacement/v1\x00" +
		lexicalManifestDigest(rows) + lexicalBuildDigest(buildIDs)))
	return hex.EncodeToString(digest[:])
}

type lexicalManifestRow struct {
	buildID   string
	segmentID string
	text      string
}

func readCatalogLexicalManifestRowsTx(
	ctx context.Context, tx metadataQuerier, buildID string,
) (_ []lexicalManifestRow, retErr error) {
	query := `
		SELECT s.build_id,s.segment_id,s.text,b.provider_operation_id
		FROM rendition_lexical_segments s
		JOIN rendition_builds b ON b.build_id=s.build_id
		ORDER BY s.build_id,s.segment_order,s.segment_id`
	var (
		rows *sql.Rows
		err  error
	)
	if buildID == "" {
		rows, err = tx.QueryContext(ctx, query)
	} else {
		rows, err = tx.QueryContext(ctx, `
			SELECT s.build_id,s.segment_id,s.text,b.provider_operation_id
			FROM rendition_lexical_segments s
			JOIN rendition_builds b ON b.build_id=s.build_id
			WHERE s.build_id=$1
			ORDER BY s.build_id,s.segment_order,s.segment_id`, buildID)
	}
	if err != nil {
		return nil, fmt.Errorf("reading staged lexical manifest: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("closing staged lexical manifest: %w", err))
		}
	}()

	var result []lexicalManifestRow
	for rows.Next() {
		var (
			row               lexicalManifestRow
			providerOperation string
		)
		if err := rows.Scan(&row.buildID, &row.segmentID, &row.text, &providerOperation); err != nil {
			return nil, fmt.Errorf("reading staged lexical manifest row: %w", err)
		}
		if providerOperation == legacyPlainTextProvider && len(result) > 0 &&
			result[len(result)-1].buildID == row.buildID {
			result[len(result)-1].text += row.text
			continue
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading staged lexical manifest rows: %w", err)
	}
	return result, nil
}

// indexLexicalSegmentsTx adds segment text for builds that no generation has
// indexed yet. Builds are immutable, so rows already present are exact.
func indexLexicalSegmentsTx(
	ctx context.Context, tx *sql.Tx, segments []lexicalManifestRow,
) error {
	indexed := make(map[string]bool)
	for _, segment := range segments {
		done, known := indexed[segment.buildID]
		if !known {
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
				SELECT 1 FROM rendition_lexical_index WHERE build_id=$1)`, segment.buildID,
			).Scan(&done); err != nil {
				return fmt.Errorf("checking lexical index for build %s: %w", segment.buildID, err)
			}
			indexed[segment.buildID] = done
		}
		if done {
			continue
		}
		// The store writer lock serializes allocation. These rebuildable row IDs
		// follow SQLite's INTEGER PRIMARY KEY behavior on both backends.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rendition_lexical_index(row_id,build_id,segment_id,text)
			SELECT COALESCE(MAX(row_id),0)+1,$1,$2,$3 FROM rendition_lexical_index`,
			segment.buildID, segment.segmentID, segment.text,
		); err != nil {
			return fmt.Errorf("indexing lexical segment %s of build %s: %w",
				segment.segmentID, segment.buildID, err)
		}
	}
	return nil
}

// publishLexicalHeadTx moves the lexical head to generationID, records the
// generation it replaced as superseded, and collects every superseded
// generation nothing can read any more. Running collection inside the flip
// keeps the projection proportional to the catalog instead of to the number
// of publications.
func (s *Store) publishLexicalHeadTx(ctx context.Context, tx *sql.Tx, generationID string) error {
	var previous string
	err := tx.QueryRowContext(ctx,
		`SELECT generation_id FROM rendition_lexical_heads WHERE singleton=1`).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reading current lexical head: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rendition_lexical_heads(singleton,generation_id) VALUES(1,$1)
		ON CONFLICT(singleton) DO UPDATE SET generation_id=excluded.generation_id`,
		generationID); err != nil {
		return fmt.Errorf("publishing lexical head: %w", err)
	}
	if previous != "" && previous != generationID {
		if _, err := tx.ExecContext(ctx, `INSERT INTO rendition_lexical_superseded(generation_id)
			VALUES($1) ON CONFLICT(generation_id) DO NOTHING`, previous); err != nil {
			return fmt.Errorf("recording superseded lexical generation %s: %w", previous, err)
		}
	}
	_, err = s.collectSupersededLexicalGenerationsTx(ctx, tx)
	return err
}

// collectSupersededLexicalGenerationsTx removes every superseded generation
// nothing can read any more: not the head, not held by an active root, not
// pinned by a live reader lease, and not the staged generation of a rendition
// job. Index rows for builds that no remaining generation names go with them.
func (s *Store) collectSupersededLexicalGenerationsTx(
	ctx context.Context, tx *sql.Tx,
) (int, error) {
	candidates, err := stringColumnTx(ctx, tx, "superseded lexical generations", `
		SELECT g.generation_id FROM rendition_lexical_superseded g
		WHERE NOT EXISTS (SELECT 1 FROM rendition_lexical_heads h
		                  WHERE h.generation_id=g.generation_id)
		  AND NOT EXISTS (SELECT 1 FROM current_rendition_roots r
		                  WHERE r.target_kind='lexical_generation' AND r.target_id=g.generation_id
		                    AND r.active=1 AND (r.expires_at IS NULL OR r.expires_at>$1))
		  AND NOT EXISTS (SELECT 1 FROM rendition_jobs j
		                  WHERE j.lexical_generation_id=g.generation_id)
		ORDER BY g.generation_id`, nowRFC3339())
	if err != nil {
		return 0, err
	}
	pinned := s.pinnedLexicalGenerationIDs()
	removed := 0
	for _, generationID := range candidates {
		if _, live := pinned[generationID]; live {
			continue
		}
		if _, err := dropLexicalGenerationTx(ctx, tx, generationID); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

func (s *Store) pinnedLexicalGenerationIDs() map[string]struct{} {
	lexicalGenerationReaders.Lock()
	defer lexicalGenerationReaders.Unlock()
	pinned := make(map[string]struct{})
	for generationID, readers := range lexicalGenerationReaders.stores[s] {
		if readers > 0 {
			pinned[generationID] = struct{}{}
		}
	}
	return pinned
}

func readLexicalManifestRowsTx(
	ctx context.Context, tx metadataQuerier, generationID, buildID string,
) ([]lexicalManifestRow, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if buildID == "" {
		rows, err = tx.QueryContext(ctx, `
			SELECT i.build_id,i.segment_id,i.text
			FROM rendition_lexical_generation_builds gb
			JOIN rendition_lexical_index i ON i.build_id=gb.build_id
			WHERE gb.generation_id=$1`, generationID)
	} else {
		rows, err = tx.QueryContext(ctx, `
			SELECT i.build_id,i.segment_id,i.text
			FROM rendition_lexical_generation_builds gb
			JOIN rendition_lexical_index i ON i.build_id=gb.build_id
			WHERE gb.generation_id=$1 AND gb.build_id=$2`, generationID, buildID)
	}
	if err != nil {
		return nil, fmt.Errorf("reading lexical generation %s manifest: %w", generationID, err)
	}
	return scanLexicalManifestRows(rows, "lexical generation "+generationID+" manifest")
}

func scanLexicalManifestRows(
	rows *sql.Rows, description string,
) (_ []lexicalManifestRow, retErr error) {
	defer func() {
		if err := rows.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("closing %s: %w", description, err))
		}
	}()
	var result []lexicalManifestRow
	for rows.Next() {
		var row lexicalManifestRow
		if err := rows.Scan(&row.buildID, &row.segmentID, &row.text); err != nil {
			return nil, fmt.Errorf("reading %s row: %w", description, err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading %s rows: %w", description, err)
	}
	return result, nil
}

func lexicalManifestDigest(rows []lexicalManifestRow) string {
	ordered := append([]lexicalManifestRow(nil), rows...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].buildID != ordered[j].buildID {
			return ordered[i].buildID < ordered[j].buildID
		}
		if ordered[i].segmentID != ordered[j].segmentID {
			return ordered[i].segmentID < ordered[j].segmentID
		}
		return ordered[i].text < ordered[j].text
	})
	hash := sha256.New()
	for _, row := range ordered {
		for _, field := range [...]string{row.buildID, row.segmentID, row.text} {
			_, _ = io.WriteString(hash, strconv.Itoa(len(field)))
			_, _ = io.WriteString(hash, ":")
			_, _ = io.WriteString(hash, field)
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func lexicalCatalogBuildIDsTx(ctx context.Context, tx *sql.Tx) ([]string, error) {
	return lexicalBuildIDsTx(ctx, tx,
		`SELECT build_id FROM rendition_builds ORDER BY build_id`, nil)
}

func lexicalGenerationBuildIDsTx(
	ctx context.Context, tx metadataQuerier, generationID string,
) ([]string, error) {
	return lexicalBuildIDsTx(ctx, tx, `
		SELECT build_id FROM rendition_lexical_generation_builds
		WHERE generation_id=$1 ORDER BY build_id`, []any{generationID})
}

func lexicalBuildIDsTx(
	ctx context.Context, tx metadataQuerier, query string, args []any,
) (_ []string, retErr error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("reading lexical generation build membership: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf(
				"closing lexical generation build membership: %w", err))
		}
	}()
	var buildIDs []string
	for rows.Next() {
		var buildID string
		if err := rows.Scan(&buildID); err != nil {
			return nil, fmt.Errorf("scanning lexical generation build membership: %w", err)
		}
		buildIDs = append(buildIDs, buildID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading lexical generation build membership: %w", err)
	}
	return buildIDs, nil
}

func lexicalBuildDigest(buildIDs []string) string {
	ordered := append([]string(nil), buildIDs...)
	sort.Strings(ordered)
	hash := sha256.New()
	for _, buildID := range ordered {
		_, _ = io.WriteString(hash, strconv.Itoa(len(buildID)))
		_, _ = io.WriteString(hash, ":")
		_, _ = io.WriteString(hash, buildID)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func loadAndValidateLexicalGenerationTx(
	ctx context.Context, tx *sql.Tx, generationID string,
) (LexicalGeneration, error) {
	generation := LexicalGeneration{ID: generationID}
	var manifestDigest sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT g.segment_count,g.build_count,m.manifest_digest
		FROM rendition_lexical_generations g
		LEFT JOIN rendition_lexical_generation_manifests m
		  ON m.generation_id=g.generation_id
		WHERE g.generation_id=$1`, generationID,
	).Scan(&generation.SegmentCount, &generation.BuildCount, &manifestDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return LexicalGeneration{}, ErrNotFound
	}
	if err != nil {
		return LexicalGeneration{}, fmt.Errorf("reading lexical generation %s: %w", generationID, err)
	}
	segments, err := readLexicalManifestRowsTx(ctx, tx, generationID, "")
	if err != nil {
		return LexicalGeneration{}, err
	}
	byBuild := make(map[string][]lexicalManifestRow)
	for _, row := range segments {
		byBuild[row.buildID] = append(byBuild[row.buildID], row)
	}
	rows, err := tx.QueryContext(ctx, `SELECT b.ordinal,b.build_id
		FROM rendition_lexical_generations g JOIN rendition_lexical_stream_builds b
		ON b.stream_id=g.stream_id AND b.ordinal<=g.build_count
		WHERE g.generation_id=$1 ORDER BY b.ordinal`, generationID)
	if err != nil {
		return LexicalGeneration{}, fmt.Errorf("reading lexical stream manifest: %w", err)
	}
	defer func() { _ = rows.Close() }()
	count := 0
	manifestChain := ""
	for rows.Next() {
		var ordinal int
		var buildID string
		if err := rows.Scan(&ordinal, &buildID); err != nil {
			return LexicalGeneration{}, fmt.Errorf("reading lexical stream manifest row: %w", err)
		}
		count++
		manifestChain = lexicalChain(manifestChain, buildID, byBuild[buildID])
		if ordinal != count {
			return LexicalGeneration{}, fmt.Errorf("lexical generation %s has a different immutable manifest", generationID)
		}
	}
	if err := rows.Err(); err != nil {
		return LexicalGeneration{}, fmt.Errorf("reading lexical stream manifest rows: %w", err)
	}
	if !manifestDigest.Valid ||
		len(segments) != generation.SegmentCount || count != generation.BuildCount ||
		manifestChain != manifestDigest.String {
		return LexicalGeneration{}, fmt.Errorf(
			"lexical generation %s has a different immutable manifest", generationID)
	}
	generation.ManifestDigest = manifestDigest.String
	return generation, nil
}

// ActiveLexicalGeneration returns the exact complete projection selected by
// the lexical head. Call AcquireLexicalGeneration when the generation must
// remain rooted after this lookup returns.
func (s *Store) ActiveLexicalGeneration(ctx context.Context) (LexicalGeneration, error) {
	return readActiveLexicalGeneration(ctx, s.db)
}

func readActiveLexicalGeneration(
	ctx context.Context, queryer rowQuerier,
) (LexicalGeneration, error) {
	var generation LexicalGeneration
	err := queryer.QueryRowContext(ctx, `
		SELECT g.generation_id,g.segment_count,m.manifest_digest,g.build_count
		FROM rendition_lexical_heads h
		JOIN rendition_lexical_generations g ON g.generation_id=h.generation_id
		JOIN rendition_lexical_generation_manifests m ON m.generation_id=g.generation_id
		WHERE h.singleton=1`).Scan(
		&generation.ID, &generation.SegmentCount, &generation.ManifestDigest,
		&generation.BuildCount,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return LexicalGeneration{}, ErrNotFound
	}
	if err != nil {
		return LexicalGeneration{}, fmt.Errorf("reading active lexical generation: %w", err)
	}
	return generation, nil
}

// AcquireLexicalGeneration pins the exact generation selected by the current
// lexical head. The caller must release the returned lease. The pin is taken
// inside a write transaction so it cannot interleave with a publication or
// purge that is deciding which generations nothing reads any more.
func (s *Store) AcquireLexicalGeneration(ctx context.Context) (*LexicalGenerationLease, error) {
	var lease *LexicalGenerationLease
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var err error
		lease, err = s.acquireLexicalGeneration(ctx, tx)
		return err
	})
	if err != nil {
		return nil, err
	}
	return lease, nil
}

func (s *Store) acquireLexicalGeneration(
	ctx context.Context, queryer rowQuerier,
) (*LexicalGenerationLease, error) {
	generation, err := readActiveLexicalGeneration(ctx, queryer)
	if err != nil {
		return nil, err
	}
	lexicalGenerationReaders.Lock()
	defer lexicalGenerationReaders.Unlock()
	readers := lexicalGenerationReaders.stores[s]
	if readers == nil {
		readers = make(map[string]int)
		lexicalGenerationReaders.stores[s] = readers
	}
	readers[generation.ID]++
	return &LexicalGenerationLease{Generation: generation, store: s}, nil
}

func (s *Store) withLexicalGenerationRead(
	ctx context.Context, fn func(queryer metadataQuerier, generation LexicalGeneration) error,
) (retErr error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquiring lexical generation connection: %w", err)
	}
	active := false
	var lease *LexicalGenerationLease
	defer func() {
		if active {
			// Roll back even when ctx is canceled so the pooled connection is clean.
			_, err := conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
			retErr = errors.Join(retErr, err)
		}
		if lease != nil {
			retErr = errors.Join(retErr, lease.Release())
		}
		retErr = errors.Join(retErr, conn.Close())
	}()
	if _, err := conn.ExecContext(ctx, "BEGIN DEFERRED"); err != nil {
		return fmt.Errorf("starting lexical generation read: %w", err)
	}
	active = true
	lease, err = s.acquireLexicalGeneration(ctx, conn)
	if errors.Is(err, ErrNotFound) {
		generationID, generationErr := collectionGenerationTx(ctx, conn)
		if generationErr != nil {
			return generationErr
		}
		if generationID != "" {
			return errors.New("lexical generation changed inside one read snapshot")
		}
		lease = nil
	} else if err != nil {
		return err
	}
	var generation LexicalGeneration
	if lease != nil {
		generation = lease.Generation
	}
	if err := fn(conn, generation); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("committing lexical generation read: %w", err)
	}
	active = false
	return nil
}

// Release removes this lease's generation root. Repeated release is safe.
func (l *LexicalGenerationLease) Release() error {
	if l == nil {
		return nil
	}
	lexicalGenerationReaders.Lock()
	defer lexicalGenerationReaders.Unlock()
	if l.released {
		return nil
	}
	l.released = true
	readers := lexicalGenerationReaders.stores[l.store]
	readers[l.Generation.ID]--
	if readers[l.Generation.ID] == 0 {
		delete(readers, l.Generation.ID)
	}
	if len(readers) == 0 {
		delete(lexicalGenerationReaders.stores, l.store)
	}
	return nil
}

// LeasedLexicalGenerationRoots returns a deterministic snapshot of exact
// generations pinned by this store's current readers.
func (s *Store) LeasedLexicalGenerationRoots() []LexicalGenerationRoot {
	lexicalGenerationReaders.Lock()
	defer lexicalGenerationReaders.Unlock()

	readers := lexicalGenerationReaders.stores[s]
	roots := make([]LexicalGenerationRoot, 0, len(readers))
	for generationID, readerCount := range readers {
		roots = append(roots, LexicalGenerationRoot{
			GenerationID: generationID,
			ReaderCount:  readerCount,
		})
	}
	sort.Slice(roots, func(i, j int) bool {
		return roots[i].GenerationID < roots[j].GenerationID
	})
	return roots
}

// PublishRenditionAndLexicalHeads inserts one version-scoped attachment and
// flips its rendition head together with the complete lexical generation.
// Any failure rolls back all three visibility changes.
func (s *Store) PublishRenditionAndLexicalHeads(
	ctx context.Context, attachment RenditionAttachmentRecord,
	head RenditionHeadRecord, generationID string,
) error {
	return s.publishRenditionAndLexicalHeads(ctx, attachment, head, generationID, nil)
}

// PublishAuthorizedRenditionAndLexicalHeads binds the verified provider
// operation and exact retained outputs to a grant in the transaction that
// makes its rendition and lexical heads visible.
func (s *Store) PublishAuthorizedRenditionAndLexicalHeads(
	ctx context.Context, attachment RenditionAttachmentRecord,
	head RenditionHeadRecord, generationID string,
	authorization ProviderOperationAuthorizationRequest,
	operation document.RenditionAuthorization,
) error {
	if authorization.PriorAuthorization == nil {
		return fmt.Errorf("publishing authorized rendition: %w", ErrProcessingConsentRequired)
	}
	operationChecksum, err := operation.Fingerprint()
	if err != nil {
		return fmt.Errorf("fingerprinting rendition authorization: %w", err)
	}
	return s.publishRenditionAndLexicalHeads(
		ctx, attachment, head, generationID, &providerPublicationAuthorization{
			consent: authorization, operationChecksum: operationChecksum,
			inputClass: string(operation.InputKind), sourceSHA256: operation.SourceSHA256,
			renditionRequestFingerprint: operation.RenditionRequestFingerprint,
		},
	)
}

type providerPublicationAuthorization struct {
	consent                     ProviderOperationAuthorizationRequest
	operationChecksum           string
	inputClass                  string
	sourceSHA256                string
	renditionRequestFingerprint string
}

func (s *Store) publishRenditionAndLexicalHeads(
	ctx context.Context, attachment RenditionAttachmentRecord,
	head RenditionHeadRecord, generationID string,
	authorization *providerPublicationAuthorization,
) error {
	normalized, err := normalizeRenditionAttachmentRecord(attachment)
	if err != nil {
		return fmt.Errorf("publishing rendition attachment: %w", err)
	}
	if err := validateRenditionHeadRecord(head); err != nil {
		return fmt.Errorf("publishing rendition head: %w", err)
	}
	if err := validateCatalogSHA256(generationID, "lexical generation ID"); err != nil {
		return err
	}
	if head.ContentVersionID != normalized.ContentVersionID ||
		head.ProcessingProfileFingerprint != normalized.Profile.Fingerprint ||
		head.AttachmentID != normalized.ID {
		return errors.New("rendition head does not resolve through its exact attachment")
	}
	if normalized.VaultID != s.vaultID {
		return fmt.Errorf("publishing rendition attachment: vault %q does not match store vault %q",
			normalized.VaultID, s.vaultID)
	}
	if authorization != nil &&
		(authorization.consent.ProfileFingerprint != normalized.Profile.Fingerprint ||
			authorization.consent.DisclosureFingerprint != normalized.Profile.RenditionDisclosureFingerprint) {
		return errors.New("provider authorization does not match rendition publication policy")
	}

	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		return s.publishRenditionAttachmentsAndLexicalHeadsTx(ctx, tx,
			[]renditionPublicationPair{{attachment: normalized, head: head}}, generationID, authorization)
	})
}

type renditionPublicationPair struct {
	attachment RenditionAttachmentRecord
	head       RenditionHeadRecord
}

// publishRenditionAttachmentsAndLexicalHeadsTx is the multi-waiter form used
// by durable rendition jobs. Every attachment/head pair and the lexical head
// become visible in this caller-owned transaction or none of them do.
func (s *Store) publishRenditionAttachmentsAndLexicalHeadsTx(
	ctx context.Context, tx *sql.Tx, pairs []renditionPublicationPair, generationID string,
	authorization *providerPublicationAuthorization,
) error {
	allEmailPDF := true
	if len(pairs) == 0 {
		return errors.New("rendition publication requires at least one attachment")
	}
	if err := validateCatalogSHA256(generationID, "lexical generation ID"); err != nil {
		return err
	}
	for _, pair := range pairs {
		normalized, err := normalizeRenditionAttachmentRecord(pair.attachment)
		if err != nil {
			return fmt.Errorf("publishing rendition attachment: %w", err)
		}
		if err := validateRenditionHeadRecord(pair.head); err != nil {
			return fmt.Errorf("publishing rendition head: %w", err)
		}
		if pair.head.ContentVersionID != normalized.ContentVersionID ||
			pair.head.ProcessingProfileFingerprint != normalized.Profile.Fingerprint ||
			pair.head.AttachmentID != normalized.ID {
			return errors.New("rendition head does not resolve through its exact attachment")
		}
		if err := ensureProcessingProfileTx(ctx, tx, normalized.Profile); err != nil {
			return err
		}
		build, err := validateRenditionPublicationTx(ctx, tx, normalized, generationID, authorization)
		if err != nil {
			return err
		}
		pdf, err := validateEmailPDFPublication(ctx, tx, normalized, build)
		if err != nil {
			return err
		}
		allEmailPDF = allEmailPDF && pdf != nil
		if authorization != nil {
			if err := s.authorizeRenditionPublicationTx(ctx, tx, build, authorization); err != nil {
				return err
			}
		}
		if err := insertRenditionAttachmentAndHeadTx(ctx, tx, normalized, pair.head); err != nil {
			return err
		}
	}
	if allEmailPDF {
		return nil
	}
	if _, err := loadAndValidateLexicalGenerationTx(ctx, tx, generationID); errors.Is(err, ErrNotFound) {
		return fmt.Errorf("lexical generation %s: %w", generationID, ErrNotFound)
	} else if err != nil {
		return err
	}
	if err := validateLexicalGenerationCoversCurrentHeadsTx(ctx, tx, generationID); err != nil {
		return err
	}
	return s.publishLexicalHeadTx(ctx, tx, generationID)
}

// validateRenditionPublicationTx proves that one attachment names an exact
// build whose source, profile, artifacts, and lexical rows all agree with the
// catalog and the generation about to become the head.
func validateRenditionPublicationTx(
	ctx context.Context, tx *sql.Tx, normalized RenditionAttachmentRecord, generationID string,
	authorization *providerPublicationAuthorization,
) (RenditionBuildRecord, error) {
	build, err := loadRenditionBuild(ctx, tx, normalized.BuildID)
	if err != nil {
		return build, fmt.Errorf("reading rendition build %s: %w", normalized.BuildID, err)
	}
	if build.VaultID != normalized.VaultID {
		return build, errors.New("rendition attachment and build belong to different vaults")
	}
	if build.RenditionRequestFingerprint != normalized.Profile.RenditionRequestFingerprint ||
		build.EvidenceLexicalFingerprint != normalized.Profile.EvidenceLexicalFingerprint {
		return build, errors.New("rendition attachment profile does not match build component identity")
	}
	if authorization != nil &&
		(authorization.operationChecksum != build.AuthorizationChecksum ||
			authorization.sourceSHA256 != build.SourceSHA256 ||
			authorization.renditionRequestFingerprint != build.RenditionRequestFingerprint) {
		return build, errors.New("provider authorization does not match rendition build operation")
	}
	if err := validateRenditionArtifactRolesForProfile(normalized.Profile, build); err != nil {
		return build, err
	}
	var sourceSHA256 string
	if err := tx.QueryRowContext(ctx,
		`SELECT blob_hash FROM content_versions WHERE version_id=$1`, normalized.ContentVersionID,
	).Scan(&sourceSHA256); errors.Is(err, sql.ErrNoRows) {
		return build, fmt.Errorf("content version %s: %w", normalized.ContentVersionID, ErrNotFound)
	} else if err != nil {
		return build, fmt.Errorf("reading content version %s: %w", normalized.ContentVersionID, err)
	}
	if sourceSHA256 != build.SourceSHA256 {
		return build, errors.New("rendition attachment source does not match content version")
	}
	suppressed, err := derivativeAttachmentSuppressedTx(
		ctx, tx, build.SourceSHA256, normalized.ContentVersionID,
		normalized.Profile.Fingerprint, build.ID)
	if err != nil {
		return build, fmt.Errorf("checking rendition attachment purge suppression: %w", err)
	}
	if suppressed {
		return build, fmt.Errorf("rendition attachment for build %s has an active purge suppression", build.ID)
	}
	if err := validateRenditionBuildStateTx(ctx, tx, build.ID); err != nil {
		return build, err
	}
	var containsBuild bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM rendition_lexical_generation_builds
		WHERE generation_id=$1 AND build_id=$2
	)`, generationID, build.ID).Scan(&containsBuild); err != nil {
		return build, fmt.Errorf("checking lexical generation %s build membership: %w", generationID, err)
	}
	if !containsBuild {
		return build, fmt.Errorf("lexical generation %s does not exactly contain build %s",
			generationID, build.ID)
	}
	expectedBuildRows, err := readCatalogLexicalManifestRowsTx(ctx, tx, build.ID)
	if err != nil {
		return build, err
	}
	indexedBuildRows, err := readLexicalManifestRowsTx(ctx, tx, generationID, build.ID)
	if err != nil {
		return build, err
	}
	if len(indexedBuildRows) != len(expectedBuildRows) ||
		lexicalManifestDigest(indexedBuildRows) != lexicalManifestDigest(expectedBuildRows) {
		return build, fmt.Errorf("lexical generation %s does not exactly contain build %s",
			generationID, build.ID)
	}
	return build, nil
}

// authorizeRenditionPublicationTx binds a provider-authorized publication to
// the consent that admitted the provider call and records the operation.
func (s *Store) authorizeRenditionPublicationTx(
	ctx context.Context, tx *sql.Tx, build RenditionBuildRecord,
	authorization *providerPublicationAuthorization,
) error {
	authority, err := normalizeConsentAuthority(authorization.consent)
	if err != nil {
		return err
	}
	if !slices.Equal(authority.inputs, []string{authorization.inputClass}) {
		return errors.New("processing consent input classes do not match rendition build input")
	}
	retained := make([]string, len(build.Artifacts))
	for index, artifact := range build.Artifacts {
		retained[index] = artifact.Role
	}
	slices.Sort(retained)
	retained = slices.Compact(retained)
	if !slices.Equal(authority.retained, retained) {
		return errors.New("processing consent retained artifact classes do not match rendition build artifacts")
	}
	if _, err := s.authorizeProviderOperationTx(
		ctx, tx, authorization.consent, time.Now().UTC(),
	); err != nil {
		return fmt.Errorf("authorizing rendition publication: %w", err)
	}
	return nil
}

// insertRenditionAttachmentAndHeadTx records the immutable attachment, or
// proves an existing one names the same metadata, then moves the head.
func insertRenditionAttachmentAndHeadTx(
	ctx context.Context, tx *sql.Tx, normalized RenditionAttachmentRecord, head RenditionHeadRecord,
) error {
	result, err := tx.ExecContext(ctx, `
			INSERT INTO rendition_attachments(
				attachment_id,vault_uid,content_version_id,build_id,profile_fingerprint,
				retention_disclosure_fingerprint,attachment_policy_fingerprint,
				consent_fingerprint,rendition_disclosure_fingerprint,trust_boundary,attached_at
			) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT DO NOTHING`,
		normalized.ID, normalized.VaultID, normalized.ContentVersionID, normalized.BuildID,
		normalized.Profile.Fingerprint, normalized.Profile.RetentionDisclosureFingerprint,
		normalized.Profile.AttachmentPolicyFingerprint, normalized.Profile.ConsentFingerprint,
		normalized.Profile.RenditionDisclosureFingerprint, normalized.Profile.TrustBoundary,
		normalized.AttachedAt)
	if err != nil {
		return fmt.Errorf("inserting rendition attachment %s: %w", normalized.ID, err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking rendition attachment %s insertion: %w", normalized.ID, err)
	}
	if inserted == 0 {
		stored, err := loadRenditionAttachment(ctx, tx, normalized.ID)
		if err != nil {
			return fmt.Errorf("reading rendition attachment %s: %w", normalized.ID, err)
		}
		// A deterministic attachment may be republished after a later head
		// superseded it. The first observation keeps its timestamp.
		normalized.AttachedAt = stored.AttachedAt
		if !reflect.DeepEqual(stored, normalized) {
			return fmt.Errorf("rendition attachment %s names different immutable metadata", normalized.ID)
		}
	}
	if binding, err := emailPDFBinding(normalized.Profile); err != nil {
		return err
	} else if binding != nil {
		if inserted != 0 {
			return recordProcessedTx(ctx, tx, normalized.ContentVersionID, "")
		}
		return nil
	}
	if err := publishRenditionHeadTx(ctx, tx, head); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM embedding_heads
		WHERE content_version_id=$1 AND profile_fingerprint=$2 AND EXISTS (
			SELECT 1 FROM embedding_sets s
			JOIN embedding_input_generations g ON g.generation_id=s.input_generation_id
			WHERE s.embedding_set_id=embedding_heads.embedding_set_id
			  AND g.attachment_id IS NOT NULL AND g.attachment_id<>$3
		)`, head.ContentVersionID, head.ProcessingProfileFingerprint, head.AttachmentID); err != nil {
		return fmt.Errorf("revoking stale chunk embedding heads: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM embedding_failures
		WHERE content_version_id=$1 AND profile_fingerprint=$2
		  AND attachment_id<>'' AND attachment_id<>$3`,
		head.ContentVersionID, head.ProcessingProfileFingerprint, head.AttachmentID); err != nil {
		return fmt.Errorf("clearing stale chunk embedding failures: %w", err)
	}
	return nil
}

func validateLexicalGenerationCoversCurrentHeadsTx(
	ctx context.Context, tx metadataQuerier, generationID string,
) error {
	buildIDs, err := currentRenditionHeadBuildIDsTx(ctx, tx)
	if err != nil {
		return err
	}
	generationBuildIDs, err := lexicalGenerationBuildIDsTx(ctx, tx, generationID)
	if err != nil {
		return err
	}
	generationBuildSet := stringSet(generationBuildIDs)
	for _, buildID := range buildIDs {
		if _, ok := generationBuildSet[buildID]; !ok {
			return fmt.Errorf("lexical generation %s does not cover published rendition build %s (current rendition head build): %w",
				generationID, buildID, ErrLexicalGenerationStale)
		}
		expected, err := readCatalogLexicalManifestRowsTx(ctx, tx, buildID)
		if err != nil {
			return err
		}
		indexed, err := readLexicalManifestRowsTx(ctx, tx, generationID, buildID)
		if err != nil {
			return err
		}
		if len(indexed) != len(expected) || lexicalManifestDigest(indexed) != lexicalManifestDigest(expected) {
			return fmt.Errorf("lexical generation %s does not exactly contain current rendition head build %s",
				generationID, buildID)
		}
	}
	return nil
}

func currentRenditionHeadBuildIDsTx(
	ctx context.Context, tx metadataQuerier,
) (_ []string, retErr error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT a.build_id
		FROM rendition_heads h
		JOIN rendition_attachments a ON a.attachment_id=h.attachment_id
		ORDER BY a.build_id`)
	if err != nil {
		return nil, fmt.Errorf("reading current rendition head builds: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	var buildIDs []string
	for rows.Next() {
		var buildID string
		if err := rows.Scan(&buildID); err != nil {
			return nil, fmt.Errorf("scanning current rendition head build: %w", err)
		}
		buildIDs = append(buildIDs, buildID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading current rendition head builds: %w", err)
	}
	return buildIDs, nil
}

// ftsQuery converts free-form user input into a safe FTS5 query: each
// whitespace-separated term becomes a quoted prefix term. Embedded double
// quotes are doubled per FTS5 string syntax.
func ftsQuery(input string) string {
	var terms []string
	for t := range strings.FieldsSeq(input) {
		// Fields are non-empty; the fixed literal analyzer has no other
		// failure mode. Keep DocBank's prefix operator outside its quotes.
		prepared, _ := lexical.Literal().PrepareLiteral(t)
		terms = append(terms, prepared.Match+"*")
	}
	return strings.Join(terms, " ")
}

// nameSearchCandidates leaves filtering and name-based ties to the outer
// query. A candidate cutoff here would discard eligible equal-score names.
func nameSearchCandidates(match string) (sqlquery.Query, error) {
	helper, err := sqlitefts.New(sqlitefts.WithIndexTable("nodes_fts"), sqlitefts.WithIndexKey("rowid"),
		sqlitefts.WithSourceTable("nodes"), sqlitefts.WithSourceKey("id"))
	if err != nil {
		return sqlquery.Query{}, fmt.Errorf("configure name search: %w", err)
	}
	query, err := helper.Build(sqlitefts.Request{Match: match, CandidateLimit: math.MaxInt})
	if err != nil {
		return sqlquery.Query{}, fmt.Errorf("build name search: %w", err)
	}
	return query, nil
}

func nameSearchPageSQL(names, filter string) string {
	// CROSS JOIN leads from name hits; paging first loads full metadata only for the selected hits.
	return `WITH name_matches AS (` + names + `), name_page AS (
		SELECT n.id,name_matches.score,n.name FROM name_matches CROSS JOIN ` + nodeFrom + `
		WHERE name_matches.doc_key=n.id AND n.trashed_at IS NULL ` + filter + `
		ORDER BY name_matches.score DESC,n.name,n.id LIMIT ?
	) SELECT ` + nodeCols + ` FROM name_page CROSS JOIN ` + nodeFrom + `
		WHERE n.id=name_page.id
		ORDER BY name_page.score DESC,name_page.name,name_page.id`
}

// SearchPage returns live name matches in their established order, followed
// by content-only matches. Keeping the two ranks separate preserves the
// deterministic name-search contract: enabling extraction never reorders or
// hides a filename match that the same limit returned before.
func (s *Store) SearchPage(ctx context.Context, query string, limit int) ([]SearchHit, bool, error) {
	return s.SearchPageWithOptions(ctx, query, limit, SearchOptions{})
}

// SearchPageWithOptions returns live matches that satisfy every requested
// filter. Blank queries select a filter-only page; other queries rank name
// matches before content matches.
func (s *Store) SearchPageWithOptions(
	ctx context.Context, query string, limit int, opts SearchOptions,
) ([]SearchHit, bool, error) {
	if limit <= 0 {
		limit = 50
	}
	var err error
	opts, err = s.NormalizeSearchOptions(ctx, opts)
	if err != nil {
		return nil, false, err
	}
	fq := ftsQuery(query)
	if SearchNeedsQuery(query, opts) {
		return nil, false, ErrSearchQueryRequired
	}
	if fq == "" {
		return s.searchFilterPage(ctx, limit, opts)
	}
	nameFilterSQL, filterSQL, filterArgs := searchCandidateFiltersSQL(opts)
	names, err := nameSearchCandidates(fq)
	if err != nil {
		return nil, false, err
	}
	nameArgs := names.Args
	nameArgs = append(nameArgs, filterArgs...)
	nameArgs = append(nameArgs, limit+1)
	rows, err := s.db.QueryContext(ctx, nameSearchPageSQL(names.SQL, nameFilterSQL), nameArgs...)
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

	seen := make(map[int64]struct{}, len(nameHits))
	for _, hit := range nameHits {
		seen[hit.Node.ID] = struct{}{}
	}
	remaining := limit - len(nameHits)
	var contentHits []SearchHit
	queryContent := func(queryer metadataQuerier, generationID string) error {
		if filterSQL == "" {
			contentHits, err = leadingContentHits(ctx, queryer, query, fq, generationID, seen, remaining+1)
			return err
		}
		// Over-fetch by the name set so overlap cannot conceal truncation.
		cte, from, rank, contentArgs := contentSearchMatches(fq, generationID, nil)
		contentQuery := `WITH ` + cte + ` SELECT ` + nodeCols + ` FROM ` + from + ` WHERE n.trashed_at IS NULL ` + filterSQL + ` ORDER BY ` + rank + `,n.name,n.id LIMIT ?`
		contentArgs = append(contentArgs, filterArgs...)
		contentArgs = append(contentArgs, remaining+len(nameHits)+1)
		rows, err := queryer.QueryContext(ctx, contentQuery, contentArgs...)
		if err != nil {
			return fmt.Errorf("searching extracted content for %q: %w", query, err)
		}
		contentHits, err = scanSearchRows(rows, SearchMatchContent, query)
		return err
	}
	err = s.withLexicalGenerationRead(ctx, func(
		queryer metadataQuerier, generation LexicalGeneration,
	) error {
		return queryContent(queryer, generation.ID)
	})
	if err != nil {
		return nil, false, err
	}
	filtered := contentHits
	if filterSQL != "" {
		filtered = contentHits[:0]
		for _, hit := range contentHits {
			if _, exists := seen[hit.Node.ID]; exists {
				continue
			}
			filtered = append(filtered, hit)
		}
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

// withSearchOptionsTx binds photo predicates to the read snapshot consuming them.
// The generation lease also keeps publication from collecting their FTS rows.
func (s *Store) withSearchOptionsTx(ctx context.Context, opts SearchOptions, fn func(*sql.Tx, SearchOptions) error) error {
	return s.withReadTx(ctx, func(tx *sql.Tx) (retErr error) {
		if opts.PhotoScope != nil {
			lease, err := s.acquireLexicalGeneration(ctx, tx)
			if err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
			if lease != nil {
				defer func() { retErr = errors.Join(retErr, lease.Release()) }()
			}
		}
		if opts.PhotoScope != nil {
			request := *opts.PhotoScope
			request.Query.Syntax = "advanced"
			var err error
			if opts.photoFilter, err = photoSearchFragment(ctx, tx, request); err != nil {
				return err
			}
			if opts.PhotoLexicalQuery != nil {
				request.Query = *opts.PhotoLexicalQuery
				if opts.photoLexical, err = photoSearchFragment(ctx, tx, request); err != nil {
					return err
				}
			}
		}

		return fn(tx, opts)
	})
}

// photoSearchFragment compiles one photo query as lexical node membership.
func photoSearchFragment(ctx context.Context, queryer rowQuerier, request PhotoBrowseRequest) (compiledQueryFragment, error) {
	request.Query.Mode = querypkg.ModeLexical
	compiled, err := (queryCompiler{photoDisplayMetadata: true, photoHidden: request.Hidden}).compile(ctx, request.Query, queryResolver{q: queryer})
	if err != nil {
		return compiledQueryFragment{}, err
	}
	generation, err := readActiveLexicalGeneration(ctx, queryer)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return compiledQueryFragment{}, err
	}
	metadata, ok := queryer.(metadataQuerier)
	if !ok {
		return compiledQueryFragment{}, ErrInvalidPhotoQuery
	}
	scope, err := newPhotoBrowseScope(ctx, metadata, compiled, generation.ID, request.Coverage, request.Hidden)
	if err != nil {
		return compiledQueryFragment{}, err
	}
	statement, args, err := scope.bind(`SELECT n.id FROM ` + photoBrowseDisplayFrom + ` WHERE ` + scope.where())
	return compiledQueryFragment{sql: statement, args: args}, err
}

// nameEvidenceSQL marks a filename match stale once the name changes or, for
// a photo search, the node no longer matches the authored query.
func nameEvidenceSQL(photoLexical compiledQueryFragment) string {
	stale := `WHEN 'node_name' THEN source.current_name IS NOT json_extract(evidence.value,'$.node_name')`
	if photoLexical.sql != "" {
		stale += ` OR source.node_id NOT IN (` + photoLexical.sql + `)`
	}
	return stale
}

// NormalizeSearchOptions validates scope identities and returns canonical filters.
func (s *Store) NormalizeSearchOptions(ctx context.Context, opts SearchOptions) (SearchOptions, error) {
	return s.normalizeSearchOptionsWithQuerier(ctx, s.db, opts)
}

func (s *Store) normalizeSearchOptionsWithQuerier(
	ctx context.Context, queryer rowQuerier, opts SearchOptions,
) (SearchOptions, error) {
	if opts.PDFFilter != "" {
		predicate, err := compilePDFFilter(ctx, opts.PDFFilter)
		if err != nil {
			return SearchOptions{}, err
		}
		opts.pdfPredicate = predicate
	}
	if len(opts.ContentVersionIDs) > MaxSearchSourceFenceIDs {
		return SearchOptions{}, fmt.Errorf("search source fence exceeds %d content versions",
			MaxSearchSourceFenceIDs)
	}
	if len(opts.ContentVersionIDs) != 0 {
		ids := slices.Clone(opts.ContentVersionIDs)
		sort.Strings(ids)
		for index, id := range ids {
			if err := validateUUIDv4(id); err != nil {
				return SearchOptions{}, errors.New("search source fence contains an invalid content version ID")
			}
			if index > 0 && ids[index-1] == id {
				return SearchOptions{}, errors.New("search source fence contains a duplicate content version ID")
			}
		}
		opts.ContentVersionIDs = ids
	}
	if opts.TagID != "" {
		if err := validateUUIDv4(opts.TagID); err != nil {
			return SearchOptions{}, fmt.Errorf("search tag %q: %w", opts.TagID, ErrNotFound)
		}
		// Scope validation needs the definition, not its assignment count.
		var exists bool
		if err := queryer.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM tags WHERE id=?)`, opts.TagID).Scan(&exists); err != nil {
			return SearchOptions{}, fmt.Errorf("search tag %q: %w", opts.TagID, err)
		}
		if !exists {
			return SearchOptions{}, fmt.Errorf("search tag %q: %w", opts.TagID, ErrNotFound)
		}
	}
	normalizedMIME, err := NormalizeSearchMIMEType(opts.MIMEType)
	if err != nil {
		return SearchOptions{}, err
	}
	opts.MIMEType = normalizedMIME
	if opts.UnderNodeID < 0 {
		return SearchOptions{}, errors.New("search directory node ID must be positive")
	}
	if opts.UnderNodeID != 0 {
		directory, err := nodeByIDQuery(ctx, queryer, opts.UnderNodeID)
		if err != nil {
			return SearchOptions{}, fmt.Errorf("search directory node %d: %w", opts.UnderNodeID, err)
		}
		if directory.TrashedAt != nil {
			return SearchOptions{}, fmt.Errorf("search directory node %d is trashed: %w",
				opts.UnderNodeID, ErrNotFound)
		}
		if !directory.IsDir() {
			return SearchOptions{}, fmt.Errorf("search scope node %d: %w", opts.UnderNodeID, ErrNotDir)
		}
	}
	modifiedSince, modifiedBefore, err := NormalizeSearchTimeBounds(
		opts.ModifiedSince, opts.ModifiedBefore,
	)
	if err != nil {
		return SearchOptions{}, err
	}
	opts.ModifiedSince = modifiedSince
	opts.ModifiedBefore = modifiedBefore
	return opts, nil
}

// searchFilterPage selects the limited page before walking its ancestry, so
// result data and paths come from one read snapshot without per-hit queries.
func (s *Store) searchFilterPage(
	ctx context.Context, limit int, opts SearchOptions,
) ([]SearchHit, bool, error) {
	filterSQL, args := searchFilterSQL(opts)
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `
		WITH RECURSIVE page AS (
			SELECT n.id, n.parent_id, n.name, n.modified_at
			FROM `+nodeFrom+`
			WHERE n.trashed_at IS NULL AND n.parent_id IS NOT NULL
			  `+filterSQL+`
			ORDER BY n.modified_at DESC, n.name, n.id
			LIMIT ?
		), ancestry(node_id, id, parent_id, path) AS (
			SELECT id, id, parent_id, name FROM page
			UNION ALL
			SELECT a.node_id, n.id, n.parent_id,
			       CASE WHEN n.name = '' THEN '/' || a.path ELSE n.name || '/' || a.path END
			FROM nodes n JOIN ancestry a ON n.id = a.parent_id
		)
		SELECT `+nodeCols+`, paths.path
		FROM `+nodeFrom+`
		JOIN page ON page.id = n.id
		JOIN ancestry paths ON paths.node_id = n.id AND paths.parent_id IS NULL
		ORDER BY page.modified_at DESC, page.name, page.id`, args...)
	if err != nil {
		return nil, false, fmt.Errorf("searching filters: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var hits []SearchHit
	for rows.Next() {
		hit := SearchHit{Match: SearchMatchFilter}
		n := &hit.Node
		if err := rows.Scan(&n.ID, &n.ParentID, &n.Name, &n.Kind,
			&n.CurrentVersionID, &n.BlobHash, &n.MD5, &n.Size, &n.MimeType,
			&n.Revision, &n.CreatedAt, &n.ModifiedAt, &n.TrashedAt, &hit.Path); err != nil {
			return nil, false, fmt.Errorf("scanning search filters: %w", err)
		}
		hits = append(hits, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("searching filters: %w", err)
	}
	truncated := len(hits) > limit
	if truncated {
		hits = hits[:limit]
	}
	return hits, truncated, nil
}

// Text search already selects candidates. Probe their memberships instead of
// collecting every node with the tag, which can dwarf a selective text match.
func searchCandidateFiltersSQL(opts SearchOptions) (names, content string, args []any) {
	tagID := opts.TagID
	opts.TagID = ""
	filter, args := searchFilterSQL(opts)
	if tagID == "" {
		return filter, filter, args
	}
	// Key the membership join by the name candidate so it can precede file metadata.
	names = filter + ` AND EXISTS (
		SELECT 1 FROM node_tags WHERE node_id=name_matches.doc_key AND tag_id=?
	)`
	// LIMIT keeps the content probe from becoming a join after file metadata.
	content = filter + ` AND EXISTS (
		SELECT 1 FROM node_tags WHERE node_id=n.id AND tag_id=? LIMIT 1
	)`
	return names, content, append(args, tagID)
}

func searchFilterSQL(opts SearchOptions) (string, []any) {
	var (
		clauses []string
		args    []any
	)
	if opts.PDFFilter != "" {
		clauses = append(clauses, "AND ("+opts.pdfPredicate.sql+")")
		args = append(args, opts.pdfPredicate.args...)
	}
	if opts.SourcePresent {
		clauses = append(clauses, "AND "+sourcePresentSQL)
	}
	if opts.photoFilter.sql != "" {
		clauses = append(clauses, `AND n.id IN (`+opts.photoFilter.sql+`)`)
		args = append(args, opts.photoFilter.args...)
	}
	if len(opts.SelectedBuilds) != 0 {
		encoded, _ := json.Marshal(opts.SelectedBuilds)
		clauses = append(clauses, `AND cv.version_id IN (SELECT json_extract(value,'$.content_version_id') FROM json_each(?))`)
		args = append(args, string(encoded))
	}
	if len(opts.ContentVersionIDs) != 0 {
		encoded, err := json.Marshal(opts.ContentVersionIDs)
		if err != nil {
			panic(err)
		}
		clauses = append(clauses, `AND cv.version_id IN (
			SELECT value FROM json_each(?)
		)`)
		args = append(args, string(encoded))
	}
	if opts.TagID != "" {
		clauses = append(clauses, `AND n.id IN (
			SELECT node_id FROM node_tags WHERE tag_id=?
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
		// Only directories can parent a result. Keep files out of the recursive
		// scope and match the immediate parent, so the scope itself stays excluded.
		// Trash marks whole subtrees; restore reattaches them under a live parent.
		// Live directories therefore suffice and use nodes_live_children.
		clauses = append(clauses, `AND n.parent_id IN (
			WITH RECURSIVE directories(id) AS (
				SELECT ?
				UNION ALL
				SELECT child.id FROM nodes child
				JOIN directories parent ON child.parent_id=parent.id
				WHERE child.kind='dir' AND child.trashed_at IS NULL
			)
			SELECT id FROM directories
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

// CollapsesPhotoDuplicates reports whether photo retrieval groups copies by content.
func (opts SearchOptions) CollapsesPhotoDuplicates() bool {
	return opts.PhotoScope != nil && opts.PhotoLexicalQuery != nil &&
		opts.PhotoLexicalQuery.Filters.CollapseDuplicates
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
	if len(hits) == 0 {
		return nil
	}
	ids := make([]int64, len(hits))
	for i := range hits {
		ids[i] = hits[i].Node.ID
	}
	paths, err := s.pathsOf(ctx, s.db, ids)
	if err != nil {
		return err
	}
	for i := range hits {
		hits[i].Path = paths[i]
	}
	return nil
}

// contentSearchSource shares FTS eligibility between grouped and streaming reads.
func contentSearchSource(match, generation string, profile *string, join string) (from, where string, args []any) {
	if generation == "" {
		return `content_fts`, `content_fts MATCH ?`, []any{match}
	}
	from = `rendition_lexical_fts
  ` + join + ` rendition_attachments a ON a.build_id=rendition_lexical_fts.build_id
  ` + join + ` rendition_heads rh ON rh.content_version_id=a.content_version_id AND rh.profile_fingerprint=a.profile_fingerprint AND rh.attachment_id=a.attachment_id`
	where = `rendition_lexical_fts MATCH ? AND EXISTS (SELECT 1 FROM rendition_lexical_generation_builds gb WHERE gb.generation_id=? AND gb.build_id=rendition_lexical_fts.build_id)`
	args = []any{match, generation}
	if profile != nil {
		where += ` AND a.profile_fingerprint=?`
		args = append(args, *profile)
	}
	return from, where, args
}

// contentSearchMatches shares retained-text ranking and publication bindings with Photos.
func contentSearchMatches(match, generation string, profile *string) (cte, from, rank string, args []any) {
	source, where, args := contentSearchSource(match, generation, profile, "JOIN")
	if generation == "" {
		return `matched_blobs AS (SELECT blob_hash,MIN(rank) best_rank FROM ` + source + ` WHERE ` + where + ` GROUP BY blob_hash)`,
			`matched_blobs mb CROSS JOIN content_versions cv ON cv.blob_hash=mb.blob_hash CROSS JOIN nodes n ON n.id=cv.node_id AND n.current_version_id=cv.version_id JOIN text_searchable_versions tsv ON tsv.version_id=cv.version_id`, `mb.best_rank`, args
	}
	return `matched_versions AS (SELECT a.content_version_id version_id,MIN(rendition_lexical_fts.rank) best_rank FROM ` + source + `
  WHERE ` + where + ` GROUP BY a.content_version_id)`,
		`matched_versions mv CROSS JOIN content_versions cv ON cv.version_id=mv.version_id CROSS JOIN nodes n ON n.id=cv.node_id AND n.current_version_id=cv.version_id`, `mv.best_rank`, args
}

type contentHitKey struct {
	id   int64
	name string
	rank float64
}

func compareContentHit(a, b contentHitKey) int {
	if order := cmp.Compare(a.rank, b.rank); order != 0 {
		return order
	}
	if order := cmp.Compare(a.name, b.name); order != 0 {
		return order
	}
	return cmp.Compare(a.id, b.id)
}

// The worst retained key stays at the root, including name and ID rank ties.
type contentHitHeap []contentHitKey

func (h *contentHitHeap) Len() int           { return len(*h) }
func (h *contentHitHeap) Less(i, j int) bool { return compareContentHit((*h)[i], (*h)[j]) > 0 }
func (h *contentHitHeap) Swap(i, j int)      { (*h)[i], (*h)[j] = (*h)[j], (*h)[i] }
func (h *contentHitHeap) Push(x any) {
	key, ok := x.(contentHitKey)
	if !ok {
		panic("store: invalid content hit heap key")
	}
	*h = append(*h, key)
}
func (h *contentHitHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

func leadingContentHits(ctx context.Context, queryer metadataQuerier, query, match, generation string, seen map[int64]struct{}, n int) ([]SearchHit, error) {
	from, where, args := contentSearchSource(match, generation, nil, "CROSS JOIN")
	rank, version := `rendition_lexical_fts.rank`, `a.content_version_id`
	if generation == "" {
		rank, version = `content_fts.rank`, `cv.version_id`
		from += ` CROSS JOIN content_versions cv ON cv.blob_hash=content_fts.blob_hash`
	} else {
		from += ` CROSS JOIN content_versions cv ON cv.version_id=a.content_version_id`
	}
	from += ` CROSS JOIN nodes n ON n.id=cv.node_id AND n.current_version_id=` + version
	if generation == "" {
		from += ` CROSS JOIN text_searchable_versions tsv ON tsv.version_id=cv.version_id`
	}
	streamRows, err := queryer.QueryContext(ctx, `SELECT n.id,n.name,`+rank+` FROM `+from+` WHERE `+where+` AND n.trashed_at IS NULL ORDER BY `+rank, args...)
	if err != nil {
		return nil, fmt.Errorf("streaming content hits: %w", err)
	}
	defer func() { _ = streamRows.Close() }()
	kept := make(contentHitHeap, 0, n)
	members := make(map[int64]struct{}, n)
	for streamRows.Next() {
		var key contentHitKey
		if err := streamRows.Scan(&key.id, &key.name, &key.rank); err != nil {
			return nil, fmt.Errorf("scanning content hit: %w", err)
		}
		if len(kept) == n && key.rank > kept[0].rank {
			break
		}
		if _, exists := seen[key.id]; exists {
			continue
		}
		if _, exists := members[key.id]; exists {
			continue
		}
		if len(kept) < n {
			heap.Push(&kept, key)
		} else if compareContentHit(key, kept[0]) < 0 {
			delete(members, kept[0].id)
			kept[0] = key
			heap.Fix(&kept, 0)
		} else {
			continue
		}
		members[key.id] = struct{}{}
	}
	if err := errors.Join(streamRows.Err(), streamRows.Close()); err != nil {
		return nil, fmt.Errorf("streaming content hits: %w", err)
	}
	slices.SortFunc(kept, compareContentHit)
	ids := make([]int64, len(kept))
	for i, key := range kept {
		ids[i] = key.id
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return nil, fmt.Errorf("encoding content hit IDs: %w", err)
	}
	rows, err := queryer.QueryContext(ctx, `SELECT `+nodeCols+` FROM json_each(?) page CROSS JOIN `+nodeFrom+` WHERE n.id=page.value ORDER BY CAST(page.key AS INTEGER)`, string(encoded))
	if err != nil {
		return nil, fmt.Errorf("loading content hits: %w", err)
	}
	return scanSearchRows(rows, SearchMatchContent, query)
}

// photoAssetTextCandidates ranks every member of the asset that owns matching
// evidence. evidenceJoin joins the index to evidence, a photo_files row, and
// extra narrows the evidence further.
func photoAssetTextCandidates(tier int, index, evidenceJoin, extra string) string {
	return ` UNION ALL SELECT member.node_id,` + strconv.Itoa(tier) + ` tier,-bm25(` + index + `) score FROM ` + index + ` ` + evidenceJoin +
		` JOIN nodes sibling ON sibling.id=evidence.node_id JOIN content_versions sibling_version ON sibling_version.version_id=sibling.current_version_id AND sibling_version.node_id=sibling.id` +
		` JOIN photo_files member ON member.asset_id=evidence.asset_id WHERE evidence.role<>'sidecar' AND sibling.kind='file' AND sibling.trashed_at IS NULL` + extra +
		` AND ` + index + ` MATCH ?`
}

// photoEvidenceCandidates ranks filename, tag-name, authored-text, and suggestion
// matches, which precede technical metadata and retained content evidence.
func photoEvidenceCandidates(namesMatch, textMatch string) (sqlquery.Query, error) {
	names, err := nameSearchCandidates(namesMatch)
	if err != nil {
		return sqlquery.Query{}, err
	}
	result := sqlquery.Query{SQL: `SELECT doc_key node_id,5 tier,score FROM (` + names.SQL + `)`, Args: names.Args}
	if textMatch == "" {
		return result, nil
	}
	result.SQL += photoAssetTextCandidates(4, "photo_authored_fts", `JOIN photo_files evidence ON evidence.node_id=photo_authored_fts.rowid`, ``) +
		photoAssetTextCandidates(4, "tag_fts", `JOIN tags t ON t.id=tag_fts.tag_id JOIN node_tags nt ON nt.tag_id=t.id JOIN photo_files evidence ON evidence.node_id=nt.node_id`, ``) +
		photoAssetTextCandidates(3, "photo_suggestion_fts", `JOIN photo_suggestions s ON s.id=photo_suggestion_fts.rowid JOIN content_versions sv ON sv.version_id=s.content_version_id JOIN photo_files evidence ON evidence.node_id=sv.node_id`,
			` AND s.content_version_id=sibling.current_version_id AND evidence.file_id=(SELECT display_file_id FROM photo_assets WHERE asset_id=evidence.asset_id)`)
	result.Args = append(result.Args, textMatch, textMatch, textMatch)
	return result, nil
}

// photoSearchCandidates adds technical metadata and retained content evidence.
func photoSearchCandidates(namesMatch, textMatch, generation string, profile *string) (sqlquery.Query, error) {
	result, err := photoEvidenceCandidates(namesMatch, textMatch)
	if err != nil || textMatch == "" {
		return result, err
	}
	helper, err := sqlitefts.New(sqlitefts.WithIndexTable("photo_metadata_fts"), sqlitefts.WithIndexKey("generation_id"), sqlitefts.WithSourceTable("photo_technical_metadata"), sqlitefts.WithSourceKey("generation_id"))
	if err != nil {
		return sqlquery.Query{}, fmt.Errorf("creating photo metadata search: %w", err)
	}
	metadata, err := helper.Build(sqlitefts.Request{Match: "text : (" + textMatch + ")", CandidateLimit: math.MaxInt})
	if err != nil {
		return sqlquery.Query{}, fmt.Errorf("building photo metadata search: %w", err)
	}
	result.SQL += ` UNION ALL SELECT n.id node_id,2 tier,matched.score FROM (` + metadata.SQL + `) matched
  JOIN source_metadata_heads h ON h.generation_id=matched.doc_key
  JOIN content_versions cv ON cv.blob_hash=h.source_sha256 JOIN nodes n ON n.id=cv.node_id AND n.current_version_id=cv.version_id`
	result.Args = append(result.Args, metadata.Args...)
	cte, from, rank, args := contentSearchMatches(textMatch, generation, profile)
	result.SQL += ` UNION ALL SELECT node_id,1 tier,score FROM (WITH ` + cte + ` SELECT n.id node_id,-` + rank + ` score FROM ` + from + `)`
	result.Args = append(result.Args, args...)
	return result, nil
}
