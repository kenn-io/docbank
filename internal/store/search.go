package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/vectorindex"
)

// SearchHit is a search result with its display path.
type SearchHit struct {
	Node  Node
	Path  string
	Match string
}

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

// SearchExplainedLexicalCandidates preserves SearchPageWithOptions ordering.
// Content selection and evidence resolution share one lexical-generation read.
func (s *Store) SearchExplainedLexicalCandidates(ctx context.Context, query string, limit int,
	opts SearchOptions,
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
		return nil, false, nil
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
		return nil, false, err
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
	queryContent := func(queryer metadataQuerier, generationID string) (retErr error) {
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
				 rendition_lexical_fts.segment_id,snippet(rendition_lexical_fts,3,char(1),char(2),' … ',24)
				FROM rendition_lexical_fts
				JOIN rendition_attachments a ON a.build_id=rendition_lexical_fts.build_id
				JOIN rendition_heads rh ON rh.content_version_id=a.content_version_id
				 AND rh.profile_fingerprint=a.profile_fingerprint AND rh.attachment_id=a.attachment_id
				JOIN content_versions cv ON cv.version_id=a.content_version_id
				JOIN nodes n ON n.id=cv.node_id AND n.current_version_id=cv.version_id
				WHERE rendition_lexical_fts MATCH ? AND rendition_lexical_fts.generation_id=?
				 AND n.trashed_at IS NULL ` + filterSQL + `
				ORDER BY rendition_lexical_fts.rank,n.name,n.id,
				 rendition_lexical_fts.build_id,rendition_lexical_fts.segment_id`
			args = append(args, generationID)
		}
		args = append(args, filterArgs...)
		rows, err := queryer.QueryContext(ctx, contentQuery, args...)
		if err != nil {
			return err
		}
		defer func() { retErr = errors.Join(retErr, rows.Close()) }()
		seenContent := make(map[int64]struct{}, remaining+1)
		for rows.Next() {
			var candidate ExplainedLexicalCandidate
			node, err := scanExplainedLexicalRow(rows, &candidate)
			if err != nil {
				return err
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
				return err
			}
			content = append(content, candidate)
			if len(content) == remaining+1 {
				break
			}
		}
		return rows.Err()
	}
	lexical, err := s.hasLexicalProjection(ctx)
	if err != nil {
		return nil, false, err
	}
	if lexical {
		err = s.withLexicalGenerationRead(ctx, func(queryer metadataQuerier, generation LexicalGeneration) error {
			return queryContent(queryer, generation.ID)
		})
		if errors.Is(err, ErrNotFound) {
			lexical = false
		} else if err != nil {
			return nil, false, err
		}
	}
	if !lexical {
		if err := queryContent(s.db, ""); err != nil {
			return nil, false, err
		}
	}
	truncated := len(content) > remaining
	if truncated {
		content = content[:remaining]
	}
	result := explainedNameCandidates(nameHits)
	result = append(result, content...)
	return result, truncated, nil
}

func explainedNameCandidates(hits []SearchHit) []ExplainedLexicalCandidate {
	result := make([]ExplainedLexicalCandidate, len(hits))
	for index, hit := range hits {
		result[index] = ExplainedLexicalCandidate{Node: hit.Node, Path: hit.Path,
			Match: SearchMatchName, EvidenceKind: "node_name", Excerpt: hit.Node.Name}
	}
	return result
}

func scanExplainedLexicalRow(row interface{ Scan(dest ...any) error }, candidate *ExplainedLexicalCandidate) (Node, error) {
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

// SearchOptions narrows ranked search without changing its name-before-content
// ordering. TagID identifies one required assignment; MIMEType selects the
// current file version's parameter-free base media type; UnderNodeID selects
// descendants of one live directory. ModifiedSince is inclusive and
// ModifiedBefore is exclusive; both accept absolute RFC3339 timestamps.
type SearchOptions struct {
	TagID             string
	MIMEType          string
	UnderNodeID       int64
	ModifiedSince     string
	ModifiedBefore    string
	ContentVersionIDs []string
}

// SearchCandidateIdentity identifies one current scoped head for a final
// retrieval fence without retaining any query or document text.
type SearchCandidateIdentity struct {
	NodeID           int64
	NodeRevision     int64
	ContentVersionID string
	Evidence         []SearchEvidenceIdentity
}

// SearchCoverageSnapshot is the current semantic coverage captured with the
// final candidate/evidence fence.
type SearchCoverageSnapshot struct {
	ScopedDocuments   int
	CompleteDocuments int
}

// SearchCandidateRevalidation returns allowed candidates and, when semantic
// authority was supplied, coverage from the same storage snapshot.
type SearchCandidateRevalidation struct {
	Candidates []SearchCandidateIdentity
	Coverage   *SearchCoverageSnapshot
}

// SearchEvidenceIdentity is the text-free stable authority needed to prove
// that one result still cites current serving evidence.
type SearchEvidenceIdentity struct {
	Kind                   string                      `json:"kind"`
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
	NodeID           int64                    `json:"node_id"`
	NodeRevision     int64                    `json:"node_revision"`
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
	normalized, err := s.normalizeSearchOptions(ctx, opts)
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
	filterSQL, filterArgs := searchFilterSQL(normalized)
	var result SearchCandidateRevalidation
	var allowedPositions []int
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
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
					WHERE lh.singleton=1 AND ra.content_version_id=scoped.version_id
					 AND ls.build_id=json_extract(evidence.value,'$.build_id')
					 AND ls.segment_id=json_extract(evidence.value,'$.segment_id'))`
			}
		}
		if semanticSource != "" {
			current, captureErr := captureVectorIndexSourceTx(ctx, tx, semanticSpace)
			if captureErr != nil {
				if errors.Is(captureErr, ErrNotFound) {
					return ErrVectorIndexSourceStale
				}
				return captureErr
			}
			if current.ManifestChecksum != semanticSource {
				return ErrVectorIndexSourceStale
			}
		}
		args := append([]any{string(encoded)}, filterArgs...)
		args = append(args, semanticProfileFingerprint, semanticBindingID)
		rows, queryErr := tx.QueryContext(ctx, `WITH requested AS (
			SELECT CAST(key AS INTEGER) AS position,
			       CAST(json_extract(value,'$.node_id') AS INTEGER) AS node_id,
			       CAST(json_extract(value,'$.node_revision') AS INTEGER) AS node_revision,
			       json_extract(value,'$.version_id') AS version_id,
			       json_extract(value,'$.evidence') AS evidence
			FROM json_each(?)
		), scoped AS (
			SELECT requested.position,requested.node_id,requested.version_id,requested.evidence,cv.blob_hash
			FROM requested JOIN nodes n ON n.id=requested.node_id
			JOIN content_versions cv ON cv.node_id=n.id AND cv.version_id=n.current_version_id
			 AND cv.version_id=requested.version_id
			WHERE n.kind='file' AND n.revision=requested.node_revision AND n.trashed_at IS NULL `+filterSQL+`
		)
		SELECT scoped.position FROM scoped
		WHERE NOT EXISTS (
			SELECT 1 FROM json_each(scoped.evidence) evidence
			WHERE CASE json_extract(evidence.value,'$.kind')
			WHEN 'node_name' THEN 0
			WHEN 'content_blob' THEN NOT (
				json_extract(evidence.value,'$.blob_hash')<>'' AND
				json_extract(evidence.value,'$.blob_hash')=scoped.blob_hash)
			`+renditionEvidenceSQL+`
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
				WHERE eh.content_version_id=scoped.version_id
				 AND eh.vector_space_id=json_extract(evidence.value,'$.vector_space_id')
				 AND eh.embedding_set_id=json_extract(evidence.value,'$.embedding_set_id')
				 AND eh.input_kind=json_extract(evidence.value,'$.input_kind')
				 AND es.profile_fingerprint=? AND es.binding_id=?
				 AND es.input_generation_id=json_extract(evidence.value,'$.input_generation_id')
				 AND evr.input_id=json_extract(evidence.value,'$.input_id')
				 AND (es.input_kind='original_file' OR EXISTS (
					SELECT 1 FROM rendition_heads current_rh
					WHERE current_rh.content_version_id=es.content_version_id
					 AND current_rh.profile_fingerprint=es.profile_fingerprint
					 AND current_rh.attachment_id=eig.attachment_id)))
			ELSE 1 END
		) ORDER BY scoped.position`, args...)
		if queryErr != nil {
			return queryErr
		}
		return func() (retErr error) {
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
	})
	if err != nil {
		return SearchCandidateRevalidation{}, err
	}
	result.Candidates = make([]SearchCandidateIdentity, 0, len(allowedPositions))
	for _, position := range allowedPositions {
		result.Candidates = append(result.Candidates, candidates[position])
	}
	return result, nil
}

// SemanticSearchCandidate is one vector neighbor reduced to a current,
// scope-eligible document. Excerpt is intentionally empty: only an independent
// lexical lane may supply text to an explained retrieval report.
type SemanticSearchCandidate struct {
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
	Distance          float64
	Excerpt           string
}

// SemanticSearchResolution binds ranked candidates and coverage to the same
// current-head snapshot after query embedding and vector search complete.
type SemanticSearchResolution struct {
	SourceManifestChecksum string
	Candidates             []SemanticSearchCandidate
	Truncated              bool
	ScopedDocuments        int
	CompleteDocuments      int
}

// SemanticSearchAuthority pins the exact persisted vector-space descriptor and
// one active local index generation for a query. Required and Complete count
// current documents after applying the operator scope.
type SemanticSearchAuthority struct {
	VectorSpace           EmbeddingVectorSpaceRecord
	Lease                 VectorIndexReaderLease
	InputKind             document.EmbeddingInputKind
	DisclosureFingerprint string
	BindingRequired       bool
	ScopedDocuments       int
	CompleteDocuments     int
}

// AcquireSemanticSearchAuthority resolves the query contract from durable E1
// authority, never from a runtime default, and leases one exact active index.
func (s *Store) AcquireSemanticSearchAuthority(ctx context.Context, profileFingerprint,
	bindingID, owner string, at time.Time, duration time.Duration, opts SearchOptions,
) (SemanticSearchAuthority, error) {
	normalized, err := s.normalizeSearchOptions(ctx, opts)
	if err != nil {
		return SemanticSearchAuthority{}, err
	}
	binding, fingerprints, err := embeddingProfileBindingAuthority(ctx, s.db, profileFingerprint, bindingID)
	if err != nil {
		return SemanticSearchAuthority{}, err
	}
	vectorSpaceID := fingerprints.VectorSpace[bindingID]
	var space EmbeddingVectorSpaceRecord
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var loadErr error
		space, loadErr = loadVectorSpaceTx(ctx, tx, vectorSpaceID)
		return loadErr
	})
	if err != nil {
		return SemanticSearchAuthority{}, err
	}
	if space.ID != vectorSpaceID || space.DescriptorFingerprint != binding.Descriptor.Fingerprint ||
		space.CompatibilityID != binding.CompatibilityID || space.Dimensions != binding.Dimensions ||
		space.Metric != binding.Metric || space.Normalization != binding.Normalization ||
		space.ScalarEncoding != binding.ScalarEncoding || space.DocumentFormatter != binding.DocumentFormatter ||
		space.QueryFormatter != binding.QueryFormatter || space.ModelInputFingerprint != binding.ModelInput.Fingerprint {
		return SemanticSearchAuthority{}, errors.New("semantic search vector space does not match profile binding authority")
	}
	lease, err := s.AcquireVectorIndexGeneration(ctx, vectorSpaceID, owner, at, duration)
	if err != nil {
		return SemanticSearchAuthority{}, err
	}
	release := func() {
		_ = s.ReleaseVectorIndexGeneration(context.WithoutCancel(ctx), lease.ID, lease.FencingToken, at)
	}
	current, err := s.CaptureVectorIndexSource(ctx, vectorSpaceID)
	if err != nil || current.ManifestChecksum != lease.Generation.SourceManifestChecksum {
		release()
		if err != nil {
			return SemanticSearchAuthority{}, err
		}
		return SemanticSearchAuthority{}, ErrVectorIndexSourceStale
	}
	required, complete, err := s.semanticSearchCoverage(ctx, profileFingerprint,
		bindingID, binding.InputKind, vectorSpaceID, normalized)
	if err != nil {
		release()
		return SemanticSearchAuthority{}, err
	}
	return SemanticSearchAuthority{VectorSpace: space, Lease: lease, InputKind: binding.InputKind,
		DisclosureFingerprint: binding.DisclosureFingerprint,
		BindingRequired:       binding.Activation == document.EmbeddingRequired,
		ScopedDocuments:       required, CompleteDocuments: complete}, nil
}

func (s *Store) semanticSearchCoverage(ctx context.Context, profileFingerprint, bindingID string,
	inputKind document.EmbeddingInputKind, vectorSpaceID string, opts SearchOptions,
) (required, complete int, retErr error) {
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var coverageErr error
		required, complete, coverageErr = semanticSearchCoverageTx(ctx, tx, profileFingerprint,
			bindingID, inputKind, vectorSpaceID, opts)
		return coverageErr
	})
	return required, complete, err
}

func semanticSearchCoverageTx(ctx context.Context, tx metadataQuerier, profileFingerprint, bindingID string,
	inputKind document.EmbeddingInputKind, vectorSpaceID string, opts SearchOptions,
) (required, complete int, retErr error) {
	filterSQL, filterArgs := searchFilterSQL(opts)
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+nodeFrom+`
		WHERE n.kind='file' AND n.trashed_at IS NULL AND cv.version_id IS NOT NULL `+filterSQL,
		filterArgs...).Scan(&required); err != nil {
		return 0, 0, err
	}
	args := []any{profileFingerprint, bindingID, inputKind, vectorSpaceID}
	args = append(args, filterArgs...)
	err := tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT n.id) FROM `+nodeFrom+`
		JOIN embedding_heads eh ON eh.content_version_id=cv.version_id
		JOIN embedding_sets es ON es.embedding_set_id=eh.embedding_set_id
		 AND es.content_version_id=eh.content_version_id AND es.binding_id=eh.binding_id
		 AND es.input_kind=eh.input_kind AND es.vector_space_id=eh.vector_space_id
		 AND es.profile_fingerprint=eh.profile_fingerprint
		JOIN embedding_input_generations eig ON eig.generation_id=es.input_generation_id
		WHERE n.kind='file' AND n.trashed_at IS NULL
		  AND eh.profile_fingerprint=? AND eh.binding_id=? AND eh.input_kind=?
		  AND eh.vector_space_id=?
		  AND (eh.input_kind='original_file' OR EXISTS(
		    SELECT 1 FROM rendition_heads rh
		    WHERE rh.content_version_id=eh.content_version_id
		      AND rh.profile_fingerprint=eh.profile_fingerprint
		      AND rh.attachment_id=eig.attachment_id
		  )) `+filterSQL, args...).Scan(&complete)
	return required, complete, err
}

// ResolveSemanticCandidates applies current-version, live-head, and operator
// scope fencing before reducing ordered vector rows to bounded documents.
func (s *Store) ResolveSemanticCandidates(ctx context.Context, profileFingerprint, bindingID string,
	inputKind document.EmbeddingInputKind, vectorSpaceID, expectedSourceManifest string,
	neighbors []vectorindex.Neighbor, limit int, opts SearchOptions,
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
	normalized, err := s.normalizeSearchOptions(ctx, opts)
	if err != nil {
		return SemanticSearchResolution{}, err
	}
	filterSQL, filterArgs := searchFilterSQL(normalized)
	var result SemanticSearchResolution
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		current, captureErr := captureVectorIndexSourceTx(ctx, tx, vectorSpaceID)
		if captureErr != nil {
			if errors.Is(captureErr, ErrNotFound) {
				return ErrVectorIndexSourceStale
			}
			return captureErr
		}
		if current.ManifestChecksum != expectedSourceManifest {
			return ErrVectorIndexSourceStale
		}
		result.SourceManifestChecksum = current.ManifestChecksum
		result.ScopedDocuments, result.CompleteDocuments, err = semanticSearchCoverageTx(ctx, tx,
			profileFingerprint, bindingID, inputKind, vectorSpaceID, normalized)
		if err != nil {
			return err
		}
		eligible, loadErr := loadSemanticEligibility(ctx, tx, vectorSpaceID, filterSQL, filterArgs)
		if loadErr != nil {
			return loadErr
		}
		result.Candidates, result.Truncated = reduceSemanticCandidates(s.vaultID, vectorSpaceID,
			neighbors, limit, eligible)
		return nil
	})
	if err != nil {
		return SemanticSearchResolution{}, err
	}
	return result, nil
}

type semanticEligibilityKey struct {
	VectorSetID   string
	InputID       string
	InputChecksum string
}

type semanticEligibility struct {
	Node      Node
	Path      string
	Candidate SemanticSearchCandidate
}

// loadSemanticEligibility performs the only catalog query needed while the
// exact index's ordered neighbors are reduced. Its result is bounded by the
// active vector-space catalog (at most one million rows), not by neighbor rank.
func loadSemanticEligibility(ctx context.Context, tx metadataQuerier, vectorSpaceID, filterSQL string,
	filterArgs []any,
) (_ map[semanticEligibilityKey]semanticEligibility, retErr error) {
	args := append([]any{vectorSpaceID}, filterArgs...)
	rows, err := tx.QueryContext(ctx, `WITH RECURSIVE node_paths(id,path) AS (
		SELECT id,'' FROM nodes WHERE parent_id IS NULL
		UNION ALL
		SELECT child.id,parent.path || '/' || child.name
		FROM nodes child JOIN node_paths parent ON child.parent_id=parent.id
	)
		SELECT `+nodeCols+`,es.embedding_set_id,es.input_generation_id,es.input_kind,
			evr.vector_set_id,evr.input_id,evr.checksum,COALESCE(NULLIF(node_paths.path,''),'/')
		FROM `+nodeFrom+`
		JOIN embedding_sets es ON es.content_version_id=cv.version_id
		JOIN embedding_heads eh ON eh.content_version_id=es.content_version_id
		 AND eh.binding_id=es.binding_id AND eh.input_kind=es.input_kind
		 AND eh.embedding_set_id=es.embedding_set_id AND eh.vector_space_id=es.vector_space_id
		 AND eh.profile_fingerprint=es.profile_fingerprint
		JOIN embedding_vector_rows evr ON evr.vector_set_id=es.vector_set_id
		JOIN embedding_generation_inputs egi ON egi.generation_id=es.input_generation_id
		 AND egi.input_id=evr.input_id AND egi.rendered_checksum=evr.checksum
		JOIN embedding_input_generations eig ON eig.generation_id=es.input_generation_id
		JOIN node_paths ON node_paths.id=n.id
		WHERE es.vector_space_id=?
		  AND n.current_version_id=es.content_version_id AND n.trashed_at IS NULL
		  AND (es.input_kind='original_file' OR EXISTS(
		    SELECT 1 FROM rendition_heads rh
		    WHERE rh.content_version_id=es.content_version_id
		      AND rh.profile_fingerprint=es.profile_fingerprint
		      AND rh.attachment_id=eig.attachment_id
		  ))
		  `+filterSQL, args...)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	eligible := make(map[semanticEligibilityKey]semanticEligibility)
	for rows.Next() {
		var (
			entry semanticEligibility
			key   semanticEligibilityKey
		)
		node, scanErr := scanSemanticCandidate(rows, &entry.Candidate,
			&key.VectorSetID, &key.InputID, &key.InputChecksum, &entry.Path)
		if scanErr != nil {
			return nil, scanErr
		}
		entry.Node = node
		eligible[key] = entry
	}
	return eligible, rows.Err()
}

func reduceSemanticCandidates(vaultID, vectorSpaceID string, neighbors []vectorindex.Neighbor,
	limit int, eligible map[semanticEligibilityKey]semanticEligibility,
) ([]SemanticSearchCandidate, bool) {
	seen := make(map[int64]struct{}, limit+1)
	candidates := make([]SemanticSearchCandidate, 0, min(limit+1, len(eligible)))
	for _, neighbor := range neighbors {
		entry, ok := eligible[semanticEligibilityKey{VectorSetID: neighbor.SetID,
			InputID: neighbor.InputKey, InputChecksum: neighbor.InputChecksum}]
		if !ok {
			continue
		}
		if _, duplicate := seen[entry.Node.ID]; duplicate {
			continue
		}
		seen[entry.Node.ID] = struct{}{}
		candidate := entry.Candidate
		candidate.VaultID = vaultID
		candidate.NodeID = entry.Node.ID
		candidate.NodeRevision = entry.Node.Revision
		candidate.ContentVersionID = entry.Node.CurrentVersionID
		candidate.VectorSpaceID = vectorSpaceID
		candidate.InputID = neighbor.InputKey
		candidate.Score, candidate.Distance = neighbor.Score, neighbor.Distance
		candidate.Path = entry.Path
		candidates = append(candidates, candidate)
		if len(candidates) == limit+1 {
			return candidates[:limit], true
		}
	}
	return candidates, false
}

func scanSemanticCandidate(row interface{ Scan(dest ...any) error }, candidate *SemanticSearchCandidate,
	dest ...any,
) (Node, error) {
	var node Node
	fields := []any{&node.ID, &node.ParentID, &node.Name, &node.Kind,
		&node.CurrentVersionID, &node.BlobHash, &node.MD5, &node.Size, &node.MimeType,
		&node.Revision, &node.CreatedAt, &node.ModifiedAt, &node.TrashedAt,
		&candidate.EmbeddingSetID, &candidate.InputGenerationID, &candidate.InputKind}
	err := row.Scan(append(fields, dest...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return Node{}, ErrNotFound
	}
	if err != nil {
		return Node{}, fmt.Errorf("scanning semantic search candidate: %w", err)
	}
	return node, nil
}

const (
	SearchMatchName    = "name"
	SearchMatchContent = "content"
)

// LexicalGeneration identifies one complete, immutable FTS projection. Rows
// remain unreachable until rendition and lexical heads are flipped together.
type LexicalGeneration struct {
	ID             string
	SegmentCount   int
	ManifestDigest string
	BuildCount     int
	BuildDigest    string
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

var lexicalGenerationReaders = struct {
	sync.Mutex

	stores map[*Store]map[string]int
}{stores: make(map[*Store]map[string]int)}

const lexicalProjectionSchema = `
CREATE TABLE IF NOT EXISTS rendition_lexical_generations (
    generation_id TEXT PRIMARY KEY,
    segment_count INTEGER NOT NULL CHECK (segment_count >= 0),
    build_count   INTEGER NOT NULL CHECK (build_count >= 0),
    built_at      TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS rendition_lexical_generation_manifests (
    generation_id  TEXT PRIMARY KEY REFERENCES rendition_lexical_generations(generation_id),
    manifest_digest TEXT NOT NULL CHECK (length(manifest_digest) = 64),
    build_digest    TEXT NOT NULL CHECK (length(build_digest) = 64)
);
CREATE TABLE IF NOT EXISTS rendition_lexical_generation_builds (
    generation_id TEXT NOT NULL REFERENCES rendition_lexical_generations(generation_id)
        ON DELETE CASCADE,
    build_id      TEXT NOT NULL REFERENCES rendition_builds(build_id),
    PRIMARY KEY (generation_id, build_id)
);
CREATE VIRTUAL TABLE IF NOT EXISTS rendition_lexical_fts USING fts5(
    generation_id UNINDEXED,
    build_id      UNINDEXED,
    segment_id    UNINDEXED,
    text
);
CREATE TABLE IF NOT EXISTS rendition_lexical_heads (
    singleton     INTEGER PRIMARY KEY CHECK (singleton = 1),
    generation_id TEXT NOT NULL REFERENCES rendition_lexical_generations(generation_id)
);`

func ensureLexicalProjectionTx(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, lexicalProjectionSchema); err != nil {
		return fmt.Errorf("initializing lexical projection: %w", err)
	}
	return nil
}

// RecordRenditionBlob grants catalog authority to one verified Docbank blob
// receipt without creating a document version or conferring visibility.
func (s *Store) RecordRenditionBlob(
	ctx context.Context, hash string, size int64, physical BlobPhysical,
) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if err := s.EnsureBlobTx(tx, hash, size, physical); err != nil {
			return err
		}
		if physical.Created {
			if _, err := tx.ExecContext(ctx, `INSERT INTO rendition_blob_staging(blob_hash)
				VALUES(?) ON CONFLICT(blob_hash) DO NOTHING`, hash); err != nil {
				return fmt.Errorf("marking rendition blob %s as uncommitted staging: %w", hash, err)
			}
		}
		return nil
	})
}

// StageLexicalGeneration builds a complete unreachable FTS projection over
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
	if err := ensureLexicalProjectionTx(ctx, tx); err != nil {
		return LexicalGeneration{}, err
	}
	segments, err := readCatalogLexicalManifestRowsTx(ctx, tx, "")
	if err != nil {
		return LexicalGeneration{}, err
	}
	buildIDs, err := lexicalCatalogBuildIDsTx(ctx, tx)
	if err != nil {
		return LexicalGeneration{}, err
	}
	return stageLexicalGenerationRowsTx(ctx, tx, generationID, segments, buildIDs)
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
	return stageLexicalGenerationRowsTx(ctx, tx, generationID, segments, buildIDs)
}

func stageLexicalGenerationRowsTx(
	ctx context.Context, tx *sql.Tx, generationID string,
	segments []lexicalManifestRow, buildIDs []string,
) (LexicalGeneration, error) {
	stored, err := loadAndValidateLexicalGenerationTx(ctx, tx, generationID)
	if err == nil {
		return stored, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return LexicalGeneration{}, err
	}
	generation := LexicalGeneration{
		ID: generationID, SegmentCount: len(segments), ManifestDigest: lexicalManifestDigest(segments),
		BuildCount: len(buildIDs), BuildDigest: lexicalBuildDigest(buildIDs),
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM rendition_lexical_fts WHERE generation_id=?`, generationID,
	); err != nil {
		return LexicalGeneration{}, fmt.Errorf("clearing interrupted lexical generation %s: %w", generationID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rendition_lexical_generations(generation_id,segment_count,build_count,built_at)
		VALUES(?,?,?,?)`, generation.ID, generation.SegmentCount, generation.BuildCount, nowRFC3339(),
	); err != nil {
		return LexicalGeneration{}, fmt.Errorf("completing lexical generation %s: %w", generationID, err)
	}
	for _, buildID := range buildIDs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rendition_lexical_generation_builds(generation_id,build_id)
			VALUES(?,?)`, generationID, buildID); err != nil {
			return LexicalGeneration{}, fmt.Errorf(
				"recording lexical generation %s build %s: %w", generationID, buildID, err)
		}
	}
	for _, segment := range segments {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rendition_lexical_fts(generation_id,build_id,segment_id,text)
			VALUES(?,?,?,?)`, generationID, segment.buildID, segment.segmentID, segment.text,
		); err != nil {
			return LexicalGeneration{}, fmt.Errorf("building lexical generation %s: %w", generationID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rendition_lexical_generation_manifests(
			generation_id,manifest_digest,build_digest
		) VALUES(?,?,?)`, generation.ID, generation.ManifestDigest, generation.BuildDigest,
	); err != nil {
		return LexicalGeneration{}, fmt.Errorf("recording lexical generation %s manifest: %w", generationID, err)
	}
	return loadAndValidateLexicalGenerationTx(ctx, tx, generationID)
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
			WHERE s.build_id=?
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

func readLexicalManifestRowsTx(
	ctx context.Context, tx metadataQuerier, generationID, buildID string,
) ([]lexicalManifestRow, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if buildID == "" {
		rows, err = tx.QueryContext(ctx, `
			SELECT build_id,segment_id,text FROM rendition_lexical_fts
			WHERE generation_id=?`, generationID)
	} else {
		rows, err = tx.QueryContext(ctx, `
			SELECT build_id,segment_id,text FROM rendition_lexical_fts
			WHERE generation_id=? AND build_id=?`, generationID, buildID)
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

func lexicalCatalogBuildIDsTx(ctx context.Context, tx metadataQuerier) ([]string, error) {
	return lexicalBuildIDsTx(ctx, tx,
		`SELECT build_id FROM rendition_builds ORDER BY build_id`, nil)
}

func lexicalGenerationBuildIDsTx(
	ctx context.Context, tx metadataQuerier, generationID string,
) ([]string, error) {
	return lexicalBuildIDsTx(ctx, tx, `
		SELECT build_id FROM rendition_lexical_generation_builds
		WHERE generation_id=? ORDER BY build_id`, []any{generationID})
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
	var manifestDigest, buildDigest sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT g.segment_count,g.build_count,m.manifest_digest,m.build_digest
		FROM rendition_lexical_generations g
		LEFT JOIN rendition_lexical_generation_manifests m
		  ON m.generation_id=g.generation_id
		WHERE g.generation_id=?`, generationID,
	).Scan(&generation.SegmentCount, &generation.BuildCount, &manifestDigest, &buildDigest)
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
	buildIDs, err := lexicalGenerationBuildIDsTx(ctx, tx, generationID)
	if err != nil {
		return LexicalGeneration{}, err
	}
	if !manifestDigest.Valid || !buildDigest.Valid ||
		len(segments) != generation.SegmentCount ||
		len(buildIDs) != generation.BuildCount ||
		lexicalManifestDigest(segments) != manifestDigest.String ||
		lexicalBuildDigest(buildIDs) != buildDigest.String {
		return LexicalGeneration{}, fmt.Errorf(
			"lexical generation %s has a different immutable manifest", generationID)
	}
	generation.ManifestDigest = manifestDigest.String
	generation.BuildDigest = buildDigest.String
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
		SELECT g.generation_id,g.segment_count,m.manifest_digest,g.build_count,m.build_digest
		FROM rendition_lexical_heads h
		JOIN rendition_lexical_generations g ON g.generation_id=h.generation_id
		JOIN rendition_lexical_generation_manifests m ON m.generation_id=g.generation_id
		WHERE h.singleton=1`).Scan(
		&generation.ID, &generation.SegmentCount, &generation.ManifestDigest,
		&generation.BuildCount, &generation.BuildDigest,
	)
	if errors.Is(err, sql.ErrNoRows) || isMissingLexicalSchema(err) {
		return LexicalGeneration{}, ErrNotFound
	}
	if err != nil {
		return LexicalGeneration{}, fmt.Errorf("reading active lexical generation: %w", err)
	}
	return generation, nil
}

// AcquireLexicalGeneration pins the exact generation selected by the current
// lexical head. The caller must release the returned lease.
func (s *Store) AcquireLexicalGeneration(ctx context.Context) (*LexicalGenerationLease, error) {
	return s.acquireLexicalGeneration(ctx, s.db)
}

func (s *Store) acquireLexicalGeneration(
	ctx context.Context, queryer rowQuerier,
) (*LexicalGenerationLease, error) {
	lexicalGenerationReaders.Lock()
	defer lexicalGenerationReaders.Unlock()

	generation, err := readActiveLexicalGeneration(ctx, queryer)
	if err != nil {
		return nil, err
	}
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
			_, err := conn.ExecContext(context.Background(), "ROLLBACK")
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
	if err != nil {
		return err
	}
	if err := fn(conn, lease.Generation); err != nil {
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

	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if err := ensureLexicalProjectionTx(ctx, tx); err != nil {
			return err
		}
		if _, err := loadAndValidateLexicalGenerationTx(ctx, tx, generationID); errors.Is(err, ErrNotFound) {
			return fmt.Errorf("lexical generation %s: %w", generationID, ErrNotFound)
		} else if err != nil {
			return err
		}
		if err := ensureProcessingProfileTx(ctx, tx, normalized.Profile); err != nil {
			return err
		}
		build, err := loadRenditionBuild(ctx, tx, normalized.BuildID)
		if err != nil {
			return fmt.Errorf("reading rendition build %s: %w", normalized.BuildID, err)
		}
		if build.VaultID != normalized.VaultID {
			return errors.New("rendition attachment and build belong to different vaults")
		}
		if build.RenditionRequestFingerprint != normalized.Profile.RenditionRequestFingerprint ||
			build.EvidenceLexicalFingerprint != normalized.Profile.EvidenceLexicalFingerprint {
			return errors.New("rendition attachment profile does not match build component identity")
		}
		if err := validateRenditionArtifactRolesForProfile(normalized.Profile, build); err != nil {
			return err
		}
		var sourceSHA256 string
		if err := tx.QueryRowContext(ctx,
			`SELECT blob_hash FROM content_versions WHERE version_id=?`, normalized.ContentVersionID,
		).Scan(&sourceSHA256); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("content version %s: %w", normalized.ContentVersionID, ErrNotFound)
		} else if err != nil {
			return fmt.Errorf("reading content version %s: %w", normalized.ContentVersionID, err)
		}
		if sourceSHA256 != build.SourceSHA256 {
			return errors.New("rendition attachment source does not match content version")
		}
		suppressed, err := derivativeAttachmentSuppressedTx(
			ctx, tx, build.SourceSHA256, normalized.ContentVersionID,
			normalized.Profile.Fingerprint, build.ID)
		if err != nil {
			return fmt.Errorf("checking rendition attachment purge suppression: %w", err)
		}
		if suppressed {
			return fmt.Errorf("rendition attachment for build %s has an active purge suppression", build.ID)
		}
		if err := validateRenditionBuildStateTx(ctx, tx, build.ID); err != nil {
			return err
		}
		var containsBuild bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM rendition_lexical_generation_builds
			WHERE generation_id=? AND build_id=?
		)`, generationID, build.ID).Scan(&containsBuild); err != nil {
			return fmt.Errorf("checking lexical generation %s build membership: %w", generationID, err)
		}
		if !containsBuild {
			return fmt.Errorf("lexical generation %s does not exactly contain build %s",
				generationID, build.ID)
		}
		expectedBuildRows, err := readCatalogLexicalManifestRowsTx(ctx, tx, build.ID)
		if err != nil {
			return err
		}
		indexedBuildRows, err := readLexicalManifestRowsTx(ctx, tx, generationID, build.ID)
		if err != nil {
			return err
		}
		if len(indexedBuildRows) != len(expectedBuildRows) ||
			lexicalManifestDigest(indexedBuildRows) != lexicalManifestDigest(expectedBuildRows) {
			return fmt.Errorf("lexical generation %s does not exactly contain build %s",
				generationID, build.ID)
		}

		result, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO rendition_attachments(
				attachment_id,vault_uid,content_version_id,build_id,profile_fingerprint,
				retention_disclosure_fingerprint,attachment_policy_fingerprint,
				consent_fingerprint,rendition_disclosure_fingerprint,trust_boundary,attached_at
			) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			normalized.ID, normalized.VaultID, normalized.ContentVersionID, normalized.BuildID,
			normalized.Profile.Fingerprint, normalized.Profile.RetentionDisclosureFingerprint,
			normalized.Profile.AttachmentPolicyFingerprint, normalized.Profile.ConsentFingerprint,
			normalized.Profile.RenditionDisclosureFingerprint, normalized.Profile.TrustBoundary,
			normalized.AttachedAt,
		)
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
			if !reflect.DeepEqual(stored, normalized) {
				return fmt.Errorf("rendition attachment %s names different immutable metadata", normalized.ID)
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rendition_heads(content_version_id,profile_fingerprint,attachment_id,published_at)
			VALUES(?,?,?,?)
			ON CONFLICT(content_version_id,profile_fingerprint) DO UPDATE SET
				attachment_id=excluded.attachment_id,published_at=excluded.published_at`,
			head.ContentVersionID, head.ProcessingProfileFingerprint,
			head.AttachmentID, head.PublishedAt,
		); err != nil {
			return fmt.Errorf("publishing rendition head: %w", err)
		}
		if err := validateLexicalGenerationCoversCurrentHeadsTx(ctx, tx, generationID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rendition_lexical_heads(singleton,generation_id) VALUES(1,?)
			ON CONFLICT(singleton) DO UPDATE SET generation_id=excluded.generation_id`,
			generationID,
		); err != nil {
			return fmt.Errorf("publishing lexical head: %w", err)
		}
		return nil
	})
}

type renditionPublicationPair struct {
	attachment RenditionAttachmentRecord
	head       RenditionHeadRecord
}

// publishRenditionAttachmentsAndLexicalHeadsTx is the multi-waiter form used
// by durable rendition jobs. Every attachment/head pair and the lexical head
// become visible in this caller-owned transaction or none of them do.
func publishRenditionAttachmentsAndLexicalHeadsTx(
	ctx context.Context, tx *sql.Tx, pairs []renditionPublicationPair, generationID string,
) error {
	if len(pairs) == 0 {
		return errors.New("rendition publication requires at least one attachment")
	}
	if err := validateCatalogSHA256(generationID, "lexical generation ID"); err != nil {
		return err
	}
	if err := ensureLexicalProjectionTx(ctx, tx); err != nil {
		return err
	}
	if _, err := loadAndValidateLexicalGenerationTx(ctx, tx, generationID); errors.Is(err, ErrNotFound) {
		return fmt.Errorf("lexical generation %s: %w", generationID, ErrNotFound)
	} else if err != nil {
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
		build, err := loadRenditionBuild(ctx, tx, normalized.BuildID)
		if err != nil {
			return fmt.Errorf("reading rendition build %s: %w", normalized.BuildID, err)
		}
		if build.VaultID != normalized.VaultID {
			return errors.New("rendition attachment and build belong to different vaults")
		}
		if build.RenditionRequestFingerprint != normalized.Profile.RenditionRequestFingerprint ||
			build.EvidenceLexicalFingerprint != normalized.Profile.EvidenceLexicalFingerprint {
			return errors.New("rendition attachment profile does not match build component identity")
		}
		if err := validateRenditionArtifactRolesForProfile(normalized.Profile, build); err != nil {
			return err
		}
		var sourceSHA256 string
		if err := tx.QueryRowContext(ctx,
			`SELECT blob_hash FROM content_versions WHERE version_id=?`, normalized.ContentVersionID,
		).Scan(&sourceSHA256); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("content version %s: %w", normalized.ContentVersionID, ErrNotFound)
		} else if err != nil {
			return fmt.Errorf("reading content version %s: %w", normalized.ContentVersionID, err)
		}
		if sourceSHA256 != build.SourceSHA256 {
			return errors.New("rendition attachment source does not match content version")
		}
		suppressed, err := derivativeAttachmentSuppressedTx(
			ctx, tx, build.SourceSHA256, normalized.ContentVersionID,
			normalized.Profile.Fingerprint, build.ID)
		if err != nil {
			return fmt.Errorf("checking rendition attachment purge suppression: %w", err)
		}
		if suppressed {
			return fmt.Errorf("rendition attachment for build %s has an active purge suppression", build.ID)
		}
		if err := validateRenditionBuildStateTx(ctx, tx, build.ID); err != nil {
			return err
		}
		var containsBuild bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM rendition_lexical_generation_builds
			WHERE generation_id=? AND build_id=?
		)`, generationID, build.ID).Scan(&containsBuild); err != nil {
			return fmt.Errorf("checking lexical generation %s build membership: %w", generationID, err)
		}
		if !containsBuild {
			return fmt.Errorf("lexical generation %s does not exactly contain build %s",
				generationID, build.ID)
		}
		expectedBuildRows, err := readCatalogLexicalManifestRowsTx(ctx, tx, build.ID)
		if err != nil {
			return err
		}
		indexedBuildRows, err := readLexicalManifestRowsTx(ctx, tx, generationID, build.ID)
		if err != nil {
			return err
		}
		if len(indexedBuildRows) != len(expectedBuildRows) ||
			lexicalManifestDigest(indexedBuildRows) != lexicalManifestDigest(expectedBuildRows) {
			return fmt.Errorf("lexical generation %s does not exactly contain build %s",
				generationID, build.ID)
		}
		result, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO rendition_attachments(
				attachment_id,vault_uid,content_version_id,build_id,profile_fingerprint,
				retention_disclosure_fingerprint,attachment_policy_fingerprint,
				consent_fingerprint,rendition_disclosure_fingerprint,trust_boundary,attached_at
			) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
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
			if !reflect.DeepEqual(stored, normalized) {
				return fmt.Errorf("rendition attachment %s names different immutable metadata", normalized.ID)
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rendition_heads(content_version_id,profile_fingerprint,attachment_id,published_at)
			VALUES(?,?,?,?)
			ON CONFLICT(content_version_id,profile_fingerprint) DO UPDATE SET
				attachment_id=excluded.attachment_id,published_at=excluded.published_at`,
			pair.head.ContentVersionID, pair.head.ProcessingProfileFingerprint,
			pair.head.AttachmentID, pair.head.PublishedAt); err != nil {
			return fmt.Errorf("publishing rendition head: %w", err)
		}
	}
	if err := validateLexicalGenerationCoversCurrentHeadsTx(ctx, tx, generationID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rendition_lexical_heads(singleton,generation_id) VALUES(1,?)
		ON CONFLICT(singleton) DO UPDATE SET generation_id=excluded.generation_id`,
		generationID); err != nil {
		return fmt.Errorf("publishing lexical head: %w", err)
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
	for _, buildID := range buildIDs {
		if !slices.Contains(generationBuildIDs, buildID) {
			return fmt.Errorf("lexical generation %s does not cover published rendition build %s (omits current rendition head build %s): %w",
				generationID, buildID, buildID, ErrLexicalGenerationStale)
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
	var err error
	opts, err = s.normalizeSearchOptions(ctx, opts)
	if err != nil {
		return nil, false, err
	}
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
	lexical, err := s.hasLexicalProjection(ctx)
	if err != nil {
		return nil, false, err
	}
	var contentHits []SearchHit
	queryContent := func(queryer metadataQuerier, generationID string) error {
		contentArgs := []any{fq}
		contentQuery := `
			WITH matched_blobs AS (
			  SELECT blob_hash, MIN(rank) AS best_rank
			  FROM content_fts WHERE content_fts MATCH ?
			  GROUP BY blob_hash
			)
			SELECT ` + nodeCols + `
			FROM ` + nodeFrom + `
			JOIN matched_blobs mb ON mb.blob_hash = cv.blob_hash
			JOIN text_searchable_versions tsv ON tsv.version_id = cv.version_id
			WHERE n.trashed_at IS NULL
			  ` + filterSQL + `
			ORDER BY mb.best_rank, n.name, n.id
			LIMIT ?`
		if generationID != "" {
			// Selection, attachment resolution, and row consumption share
			// this reader's one immutable publication snapshot. Once a
			// lexical head exists, legacy content_fts is a non-serving cache.
			contentQuery = `
				WITH matched_versions(version_id,best_rank) AS (
				  SELECT a.content_version_id, MIN(rendition_lexical_fts.rank)
				  FROM rendition_lexical_fts
				  JOIN rendition_attachments a ON a.build_id=rendition_lexical_fts.build_id
				  JOIN rendition_heads rh
				    ON rh.content_version_id=a.content_version_id
				   AND rh.profile_fingerprint=a.profile_fingerprint
				   AND rh.attachment_id=a.attachment_id
				  WHERE rendition_lexical_fts MATCH ?
				    AND rendition_lexical_fts.generation_id=?
				  GROUP BY a.content_version_id
				)
				SELECT ` + nodeCols + `
				FROM ` + nodeFrom + `
				JOIN matched_versions mv ON mv.version_id=cv.version_id
				WHERE n.trashed_at IS NULL
				  ` + filterSQL + `
				ORDER BY mv.best_rank,n.name,n.id
				LIMIT ?`
			contentArgs = append(contentArgs, generationID)
		}
		contentArgs = append(contentArgs, filterArgs...)
		contentArgs = append(contentArgs, remaining+len(nameHits)+1)
		rows, err := queryer.QueryContext(ctx, contentQuery, contentArgs...)
		if err != nil {
			return fmt.Errorf("searching extracted content for %q: %w", query, err)
		}
		contentHits, err = scanSearchRows(rows, SearchMatchContent, query)
		return err
	}
	if lexical {
		err = s.withLexicalGenerationRead(ctx, func(
			queryer metadataQuerier, generation LexicalGeneration,
		) error {
			return queryContent(queryer, generation.ID)
		})
		if errors.Is(err, ErrNotFound) {
			lexical = false
		} else if err != nil {
			return nil, false, err
		}
	}
	if !lexical {
		if err := queryContent(s.db, ""); err != nil {
			return nil, false, err
		}
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

func (s *Store) normalizeSearchOptions(ctx context.Context, opts SearchOptions) (SearchOptions, error) {
	if len(opts.ContentVersionIDs) > 4096 {
		return SearchOptions{}, errors.New("search source fence exceeds 4096 content versions")
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
		if _, err := s.TagByID(ctx, opts.TagID); err != nil {
			return SearchOptions{}, fmt.Errorf("search tag %q: %w", opts.TagID, err)
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
		directory, err := s.NodeByID(ctx, opts.UnderNodeID)
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

func (s *Store) hasLexicalProjection(ctx context.Context) (bool, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS(
		  SELECT 1 FROM sqlite_schema
		  WHERE type='table' AND name='rendition_lexical_heads'
		)`).Scan(&exists); err != nil {
		return false, fmt.Errorf("checking lexical projection: %w", err)
	}
	return exists, nil
}

func isMissingLexicalSchema(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table: rendition_lexical_heads")
}

func searchFilterSQL(opts SearchOptions) (string, []any) {
	var (
		clauses []string
		args    []any
	)
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
