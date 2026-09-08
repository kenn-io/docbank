package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"

	"go.kenn.io/docbank/document"
)

// SearchCandidateIdentity identifies one current scoped head for a final
// retrieval fence without retaining any query or document text.
type SearchCandidateIdentity struct {
	NodeID           int64
	NodeRevision     int64
	ContentVersionID string
	PathChecksum     string
	Evidence         []SearchEvidenceIdentity
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

type searchCandidateRevalidationJSON struct {
	NodeID           int64                    `json:"node_id"`
	NodeRevision     int64                    `json:"node_revision"`
	ContentVersionID string                   `json:"version_id"`
	PathChecksum     string                   `json:"path_checksum"`
	Evidence         []SearchEvidenceIdentity `json:"evidence"`
}

// SearchPathChecksum derives transient comparison authority from the exact
// canonical display path bytes.
func SearchPathChecksum(path string) string {
	digest := sha256.Sum256([]byte(path))
	return hex.EncodeToString(digest[:])
}

// RevalidateSearchCandidates retains only supplied identities that remain live,
// current, on the same canonical path, and inside the operator scope.
func (s *Store) RevalidateSearchCandidates(ctx context.Context, candidates []SearchCandidateIdentity,
	opts SearchOptions, semanticProfileFingerprint, semanticBindingID string,
) (SearchCandidateRevalidation, error) {
	if len(candidates) > document.MaxRetrievalCandidateLimit {
		return SearchCandidateRevalidation{}, errors.New("search candidate revalidation exceeds the retrieval limit")
	}
	payload := make([]searchCandidateRevalidationJSON, len(candidates))
	semanticSpace, semanticSource := "", ""
	for i, candidate := range candidates {
		if err := validateCatalogSHA256(candidate.PathChecksum, "search candidate path checksum"); err != nil {
			return SearchCandidateRevalidation{}, err
		}
		if len(candidate.Evidence) == 0 || len(candidate.Evidence) > 32 {
			return SearchCandidateRevalidation{}, errors.New("search candidate evidence is invalid")
		}
		payload[i] = searchCandidateRevalidationJSON(candidate)
		for _, evidence := range candidate.Evidence {
			if err := validateSearchEvidenceIdentity(evidence); err != nil {
				return SearchCandidateRevalidation{}, err
			}
			if evidence.Kind != "embedding" {
				continue
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
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		normalized, normalizeErr := s.normalizeSearchOptionsWithQuerier(ctx, tx, opts)
		if normalizeErr != nil {
			return normalizeErr
		}
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
		if len(candidates) == 0 {
			return nil
		}

		filterSQL, filterArgs := searchFilterSQL(normalized)
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
		SELECT scoped.position,scoped.node_id FROM scoped
		WHERE NOT EXISTS (
			SELECT 1 FROM json_each(scoped.evidence) evidence
			WHERE CASE json_extract(evidence.value,'$.kind')
			WHEN 'node_name' THEN 0
			WHEN 'content_blob' THEN NOT (
				COALESCE(json_extract(evidence.value,'$.blob_hash'),'')<>'' AND
				COALESCE(json_extract(evidence.value,'$.blob_hash'),'')=scoped.blob_hash)
			WHEN 'rendition_segment' THEN NOT EXISTS (
				SELECT 1 FROM rendition_lexical_heads lh
				JOIN rendition_lexical_generation_builds gb ON gb.generation_id=lh.generation_id
				JOIN rendition_lexical_index li ON li.build_id=gb.build_id
				JOIN rendition_attachments ra ON ra.build_id=li.build_id
				JOIN rendition_heads rh ON rh.content_version_id=ra.content_version_id
				 AND rh.profile_fingerprint=ra.profile_fingerprint AND rh.attachment_id=ra.attachment_id
				WHERE lh.singleton=1 AND ra.content_version_id=scoped.version_id
				 AND li.build_id=json_extract(evidence.value,'$.build_id')
				 AND li.segment_id=json_extract(evidence.value,'$.segment_id'))
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
		type allowedCandidate struct {
			position int
			nodeID   int64
		}
		var allowed []allowedCandidate
		if readErr := func() (retErr error) {
			defer func() { retErr = errors.Join(retErr, rows.Close()) }()
			for rows.Next() {
				var candidate allowedCandidate
				if scanErr := rows.Scan(&candidate.position, &candidate.nodeID); scanErr != nil {
					return scanErr
				}
				allowed = append(allowed, candidate)
			}
			return rows.Err()
		}(); readErr != nil {
			return readErr
		}
		result.Candidates = make([]SearchCandidateIdentity, 0, len(allowed))
		for _, allowedCandidate := range allowed {
			path, pathErr := pathOf(ctx, tx, allowedCandidate.nodeID)
			if pathErr != nil {
				return pathErr
			}
			candidate := candidates[allowedCandidate.position]
			if SearchPathChecksum(path) == candidate.PathChecksum {
				result.Candidates = append(result.Candidates, candidate)
			}
		}
		return nil
	})
	if err != nil {
		return SearchCandidateRevalidation{}, err
	}
	return result, nil
}

func validateSearchEvidenceIdentity(evidence SearchEvidenceIdentity) error {
	switch evidence.Kind {
	case "node_name":
		return nil
	case "content_blob":
		if evidence.BlobHash == "" {
			return errors.New("search candidate content blob evidence is incomplete")
		}
		if err := validateCatalogSHA256(evidence.BlobHash, "search candidate content blob evidence"); err != nil {
			return err
		}
		return nil
	case "rendition_segment":
		if evidence.BuildID == "" || evidence.SegmentID == "" {
			return errors.New("search candidate rendition evidence is incomplete")
		}
		return nil
	case "embedding":
		if evidence.VectorSpaceID == "" || evidence.EmbeddingSetID == "" ||
			evidence.InputGenerationID == "" || evidence.InputID == "" || evidence.InputKind == "" ||
			evidence.SourceManifestChecksum == "" {
			return errors.New("semantic search evidence authority is incomplete")
		}
		for _, field := range []struct {
			value   string
			subject string
		}{
			{evidence.VectorSpaceID, "semantic search evidence vector-space ID"},
			{evidence.EmbeddingSetID, "semantic search evidence embedding-set ID"},
			{evidence.InputGenerationID, "semantic search evidence input-generation ID"},
			{evidence.SourceManifestChecksum, "semantic search evidence source manifest"},
		} {
			if err := validateCatalogSHA256(field.value, field.subject); err != nil {
				return err
			}
		}
		if evidence.InputKind != document.EmbeddingInputOriginalFile &&
			evidence.InputKind != document.EmbeddingInputRenditionChunk {
			return errors.New("semantic search evidence input kind is invalid")
		}
		return nil
	default:
		return errors.New("search candidate evidence kind is unknown")
	}
}
