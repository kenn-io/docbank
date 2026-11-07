package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"

	"go.kenn.io/docbank/document"
)

type FaceSuggestionFace struct {
	PhotoFace

	PersonID        string             `json:"person_id,omitzero"`
	PersonName      string             `json:"person_name,omitzero"`
	PhotoRecordedAt string             `json:"photo_recorded_at"`
	Preview         *PersonFacePreview `json:"preview"`
	Hidden          bool               `json:"hidden"`
}
type FaceSuggestionSet struct {
	Set   EmbeddingSetRecord
	Faces []FaceSuggestionFace
}

func (s *Store) faceSuggestionInputsTx(ctx context.Context, tx *sql.Tx, faceID, candidates string, args []any) ([]FaceSuggestionSet, error) {
	versions := `SELECT h.content_version_id FROM embedding_heads h WHERE h.input_kind='face'`
	if faceID != "" {
		namedPeople := ""
		if candidates != "" {
			namedPeople = " AND pa.person_id IN (SELECT persons.person_id FROM persons WHERE " + candidates + "persons.state<>'retired')"
		}
		versions += ` AND (h.content_version_id=(SELECT content_version_id FROM photo_faces WHERE face_id=?) OR EXISTS(SELECT 1 FROM photo_faces named JOIN person_document_assertions pa ON pa.assertion_id=named.assertion_id AND pa.action='assert' JOIN persons p ON p.person_id=pa.person_id AND p.state<>'retired' WHERE named.content_version_id=h.content_version_id` + namedPeople + `))`
		args = append([]any{faceID}, args...)
	}
	ids, err := stringColumnTx(ctx, tx, "face vector heads", `SELECT h.embedding_set_id FROM embedding_heads h JOIN embedding_sets es ON es.embedding_set_id=h.embedding_set_id WHERE h.input_kind='face' AND h.content_version_id IN (`+versions+`) ORDER BY h.embedding_set_id`, args...)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+photoFaceColumns+` FROM photo_faces WHERE content_version_id IN (`+versions+`) ORDER BY face_id`, args...)
	if err != nil {
		return nil, err
	}
	defer func(rows *sql.Rows) { _ = rows.Close() }(rows)
	var faces []PhotoFace
	for rows.Next() {
		face, err := scanPhotoFace(rows)
		if err != nil {
			return nil, err
		}
		faces = append(faces, face)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.readFaceDecisionsQuery(ctx, tx, faces, ` AND f.content_version_id IN (`+versions+`)`, args...); err != nil {
		return nil, err
	}
	metadata := map[string]FaceSuggestionFace{}
	rows, err = tx.QueryContext(ctx, `SELECT f.face_id,v.recorded_at,COALESCE(p.person_id,''),COALESCE(p.display_name,''),a.hidden_at IS NOT NULL FROM photo_faces f JOIN content_versions v ON v.version_id=f.content_version_id JOIN photo_files member ON member.node_id=v.node_id JOIN photo_assets a ON a.asset_id=member.asset_id LEFT JOIN person_document_assertions assertion ON assertion.assertion_id=f.assertion_id AND assertion.action='assert' LEFT JOIN persons p ON p.person_id=assertion.person_id AND p.state<>'retired' WHERE f.content_version_id IN (`+versions+`) ORDER BY f.face_id`, args...)
	if err != nil {
		return nil, err
	}
	defer func(rows *sql.Rows) { _ = rows.Close() }(rows)
	for rows.Next() {
		var face FaceSuggestionFace
		if err := rows.Scan(&face.FaceID, &face.PhotoRecordedAt, &face.PersonID, &face.PersonName, &face.Hidden); err != nil {
			return nil, err
		}
		metadata[face.FaceID] = face
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	byVersion := map[string][]FaceSuggestionFace{}
	for _, face := range faces {
		f := metadata[face.FaceID]
		f.PhotoFace = face
		byVersion[face.ContentVersionID] = append(byVersion[face.ContentVersionID], f)
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	quality, err := document.CurrentPhotoQualityFingerprints()
	if err != nil {
		return nil, err
	}
	setRows, err := tx.QueryContext(ctx, `SELECT es.embedding_set_id,es.vault_uid,es.binding_id,es.input_kind,es.content_version_id,es.profile_fingerprint,es.embedding_input_fingerprint,es.vector_space_id,es.created_at,
 g.generation_id,COALESCE(g.generation_blob_hash,''),g.generation_encoded_size,g.generation_checksum,g.source_version_id,g.profile_fingerprint,g.evidence_fingerprint,g.tokenizer_fingerprint,g.chunk_policy_fingerprint,g.formatter_fingerprint,g.attachment_context_fingerprint,COALESCE(g.attachment_id,''),g.input_count,g.created_at,
 vs.vector_set_id,vs.contract_version,vs.vector_space_id,vs.payload_blob_hash,vs.payload_size,vs.payload_checksum,vs.manifest_checksum,vs.row_count,vs.dimensions,
 `+embeddingAttachmentEligibilitySQL("es.content_version_id", "es.profile_fingerprint", "g.attachment_id")+`,EXISTS(SELECT 1 FROM content_versions v JOIN nodes n ON n.id=v.node_id JOIN photo_files f ON f.node_id=n.id JOIN photo_assets a ON a.asset_id=f.asset_id WHERE v.version_id=es.content_version_id AND n.current_version_id=v.version_id AND a.kind='photo' AND f.role IN ('image','raw') AND `+liveIncludedDisplayPredicate+`)
 FROM embedding_sets es JOIN embedding_input_generations g ON g.generation_id=es.input_generation_id JOIN embedding_vector_sets vs ON vs.vector_set_id=es.vector_set_id
 WHERE es.embedding_set_id IN (SELECT value FROM json_each($1)) ORDER BY es.embedding_set_id`, string(encoded))
	if err != nil {
		return nil, err
	}
	defer func() { _ = setRows.Close() }()
	type authority struct {
		set            EmbeddingSetRecord
		inputs         int
		live, attached bool
	}
	records := make(map[string]*authority, len(ids))
	for setRows.Next() {
		a := new(authority)
		r := &a.set
		g, v := &r.InputGeneration, &r.VectorSet
		if err := setRows.Scan(&r.ID, &r.VaultID, &r.BindingID, &r.InputKind, &r.ContentVersionID, &r.ProcessingProfileFingerprint, &r.EmbeddingInputFingerprint, &r.VectorSpace.ID, &r.CreatedAt,
			&g.ID, &g.GenerationBlobHash, &g.GenerationEncodedSize, &g.GenerationChecksum, &g.SourceVersionID, &g.ProcessingProfileFingerprint, &g.EvidenceFingerprint, &g.TokenizerFingerprint, &g.ChunkPolicyFingerprint, &g.FormatterFingerprint, &g.AttachmentContextFingerprint, &g.AttachmentID, &a.inputs, &g.CreatedAt,
			&v.ID, &v.ContractVersion, &v.VectorSpaceID, &v.PayloadBlobHash, &v.PayloadSize, &v.PayloadChecksum, &v.ManifestChecksum, &v.RowCount, &v.Dimensions, &a.attached, &a.live); err != nil {
			return nil, err
		}
		records[r.ID] = a
	}
	if err := setRows.Err(); err != nil {
		return nil, err
	}
	if err := setRows.Close(); err != nil {
		return nil, err
	}
	inputRows, err := tx.QueryContext(ctx, `SELECT es.embedding_set_id,0 AS kind,i.input_id AS row_id,i.input_id,i.rendered_checksum,0 AS dimensions,i.input_order AS ordinal FROM embedding_sets es JOIN embedding_generation_inputs i ON i.generation_id=es.input_generation_id WHERE es.embedding_set_id IN (SELECT value FROM json_each($1))
 UNION ALL SELECT es.embedding_set_id,1,r.row_id,r.input_id,r.checksum,r.dimensions,r.row_order FROM embedding_sets es JOIN embedding_vector_rows r ON r.vector_set_id=es.vector_set_id WHERE es.embedding_set_id IN (SELECT value FROM json_each($1)) ORDER BY 1,2,7`, string(encoded))
	if err != nil {
		return nil, err
	}
	defer func() { _ = inputRows.Close() }()
	for inputRows.Next() {
		var id string
		var kind, order int
		var row EmbeddingVectorRowRecord
		if err := inputRows.Scan(&id, &kind, &row.RowID, &row.InputID, &row.Checksum, &row.Dimensions, &order); err != nil {
			return nil, err
		}
		a := records[id]
		if a == nil {
			continue
		}
		if kind == 0 {
			a.set.InputGeneration.Inputs = append(a.set.InputGeneration.Inputs, EmbeddingInputReference{ID: row.InputID, RenderedChecksum: row.Checksum})
		} else {
			a.set.VectorSet.rows = append(a.set.VectorSet.rows, row)
		}
	}
	if err := inputRows.Err(); err != nil {
		return nil, err
	}
	if err := inputRows.Close(); err != nil {
		return nil, err
	}
	spaces := map[string]EmbeddingVectorSpaceRecord{}
	result := make([]FaceSuggestionSet, 0, len(ids))
	for _, id := range ids {
		a := records[id]
		if a == nil {
			continue
		}
		r := a.set
		g, v := r.InputGeneration, r.VectorSet
		if len(g.Inputs) != a.inputs || len(g.Inputs) > maxEmbeddingCatalogRows {
			return nil, errors.New("embedding input generation count is corrupt")
		}
		if v.RowCount < 1 || v.RowCount > maxEmbeddingCatalogRows || len(v.rows) != v.RowCount || embeddingVectorManifestChecksum(v.rows) != v.ManifestChecksum {
			return nil, errors.New("embedding vector-set manifest is corrupt")
		}
		space, ok := spaces[r.VectorSpace.ID]
		if !ok {
			space, err = loadVectorSpaceTx(ctx, tx, r.VectorSpace.ID)
			if err != nil {
				return nil, err
			}
			spaces[space.ID] = space
		}
		r.VectorSpace = space
		if !a.live {
			continue
		}
		if err := validateFaceEmbeddingGeneration(g, r.BindingID); err != nil {
			return nil, err
		}
		current := byVersion[r.ContentVersionID]
		eligible := len(current) == len(g.Inputs) && a.attached
		if eligible {
			for i, face := range current {
				if g.Inputs[i].ID != face.FaceID || g.Inputs[i].RenderedChecksum != FaceEmbeddingChecksum(g.GenerationChecksum, face.PhotoFace) {
					eligible = false
					break
				}
			}
		}
		if eligible {
			result = append(result, FaceSuggestionSet{Set: r, Faces: current})
		}
	}
	versionIDs := make([]string, 0, len(result))
	for _, item := range result {
		versionIDs = append(versionIDs, item.Set.ContentVersionID)
	}
	encoded, err = json.Marshal(versionIDs)
	if err != nil {
		return nil, err
	}
	previewRows, err := tx.QueryContext(ctx, `SELECT `+visualPreviewGenerationColumns+` FROM visual_preview_generations WHERE recipe_fingerprint=$1 AND content_version_id IN (SELECT value FROM json_each($2))`, quality.GridRecipe, string(encoded))
	if err != nil {
		return nil, err
	}
	defer func() { _ = previewRows.Close() }()
	previews := map[string]VisualPreviewGeneration{}
	for previewRows.Next() {
		preview, err := scanVisualPreviewGeneration(previewRows)
		if err != nil {
			return nil, err
		}
		previews[preview.ContentVersionID] = preview
	}
	if err := previewRows.Err(); err != nil {
		return nil, err
	}
	return slices.DeleteFunc(result, func(item FaceSuggestionSet) bool {
		preview, found := previews[item.Set.ContentVersionID]
		return !found || preview.Checksum != item.Set.InputGeneration.EvidenceFingerprint || preview.Preview.State != document.VisualPreviewReady
	}), nil
}

// ReplaceFaceSuggestions preserves rejected evidence and supersedes open groups
// whose membership changed. Publishing and queue admission share one transaction.
func (s *Store) ReplaceFaceSuggestions(ctx context.Context, candidates []PersonMatchCandidate) error {
	return s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		wanted := map[string]bool{}
		for i := range candidates {
			c, err := normalizePersonCandidate(candidates[i])
			if err != nil {
				return err
			}
			candidates[i] = c
			wanted[c.ActorKey+"\x00"+c.SuggestedPersonID+"\x00"+c.EvidenceSHA256] = true
		}
		ids, err := stringColumnTx(ctx, tx, "open face candidates", `SELECT candidate_id FROM person_match_candidates WHERE state='open' AND reason='face_match'`)
		if err != nil {
			return err
		}
		for _, id := range ids {
			c, err := personCandidateByIDTx(ctx, tx, id)
			if err != nil {
				return err
			}
			if !wanted[c.ActorKey+"\x00"+c.SuggestedPersonID+"\x00"+c.EvidenceSHA256] {
				if _, err := tx.ExecContext(ctx, `UPDATE person_match_candidates SET state='superseded',revision=revision+1 WHERE candidate_id=$1`, id); err != nil {
					return err
				}
			}
		}
		for _, c := range candidates {
			opened, retained, err := s.retainNormalizedPersonCandidate(ctx, tx, c, false)
			if errors.Is(err, ErrPersonRetired) || errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if !retained {
				break
			}
			if opened.State == "linked" {
				var named bool
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM person_candidate_faces e JOIN photo_faces f ON f.face_id=e.face_id AND f.content_version_id=e.content_version_id WHERE e.candidate_id=$1 AND f.assertion_id IS NOT NULL)`, opened.CandidateID).Scan(&named); err != nil {
					return err
				}
				if named {
					continue
				}
			}
			if opened.State == "superseded" || opened.State == "linked" {
				_, full, err := openPersonCandidateCount(ctx, tx)
				if err != nil {
					return err
				}
				if full {
					break
				}
				if _, err := tx.ExecContext(ctx, `UPDATE person_match_candidates SET state='open',decided_at=NULL,decided_person_id=NULL,revision=revision+1 WHERE candidate_id=$1`, opened.CandidateID); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

type FaceCandidate struct {
	CandidateID       string               `json:"candidate_id"`
	DisplayName       string               `json:"display_name"`
	SuggestedPersonID string               `json:"suggested_person_id,omitzero"`
	Revision          int64                `json:"revision"`
	Faces             []FaceSuggestionFace `json:"faces"`
}

func (s *Store) FaceCandidates(ctx context.Context, limit, offset int) ([]FaceCandidate, int64, error) {
	if limit < 1 || limit > 250 || offset < 0 || offset > 10000 {
		return nil, 0, ErrInvalidPerson
	}
	result := []FaceCandidate{}
	var total int64
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		visible, err := s.personCandidateVisibility(ctx, tx)
		if err != nil {
			return err
		}
		_, hiddenErr := s.hiddenSession(ctx, tx)
		if hiddenErr != nil && !errors.Is(hiddenErr, ErrHiddenLocked) {
			return hiddenErr
		}
		hideLocked := hiddenErr != nil
		visible += ` AND (suggested_person_id IS NULL OR EXISTS(SELECT 1 FROM persons WHERE persons.person_id=COALESCE((SELECT surviving_person_id FROM person_aliases WHERE retired_person_id=person_match_candidates.suggested_person_id),person_match_candidates.suggested_person_id) AND persons.state<>'retired' AND ` + personVisibleSQL([3]string{"$1", "$2", "$3"}) + `))`
		locked := 0
		if hideLocked {
			locked = 1
		}
		args := []any{1, locked, locked}
		visible += ` AND (SELECT COUNT(*) FROM person_candidate_faces e JOIN photo_faces f ON f.face_id=e.face_id AND f.content_version_id=e.content_version_id JOIN content_versions cv ON cv.version_id=f.content_version_id JOIN nodes n ON n.id=cv.node_id JOIN photo_files pf ON pf.node_id=n.id JOIN photo_assets a ON a.asset_id=pf.asset_id WHERE e.candidate_id=person_match_candidates.candidate_id AND f.assertion_id IS NULL AND a.kind='photo' AND a.excluded_at IS NULL AND ` + currentPhotoSourcePredicate("pf", "n", "f.content_version_id") + ` AND NOT ` + faceSuggestionDecided("f.face_id", "f.content_version_id", "person_match_candidates.suggested_person_id") + `) >= CASE WHEN person_match_candidates.suggested_person_id IS NULL THEN 2 ELSE 1 END`
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_match_candidates WHERE state='open' AND reason='face_match' AND `+visible, args...).Scan(&total); err != nil {
			return err
		}
		ids, err := stringColumnTx(ctx, tx, "face candidates", `SELECT candidate_id FROM person_match_candidates WHERE state='open' AND reason='face_match' AND `+visible+` ORDER BY created_at,candidate_id LIMIT $4 OFFSET $5`, append(args, limit, offset)...)
		if err != nil {
			return err
		}
		quality, err := document.CurrentPhotoQualityFingerprints()
		if err != nil {
			return err
		}
		for _, id := range ids {
			c, err := personCandidateByIDTx(ctx, tx, id)
			if err != nil {
				return err
			}
			item := FaceCandidate{CandidateID: c.CandidateID, DisplayName: c.DisplayName, SuggestedPersonID: c.SuggestedPersonID, Revision: c.Revision, Faces: []FaceSuggestionFace{}}
			if c.SuggestedPersonID != "" {
				item.SuggestedPersonID, err = resolveActiveCandidatePersonTx(ctx, tx, c.SuggestedPersonID)
				if err != nil {
					return err
				}
				if err := tx.QueryRowContext(ctx, `SELECT display_name FROM persons WHERE person_id=$1`, item.SuggestedPersonID).Scan(&item.DisplayName); err != nil {
					return err
				}
			}
			occurrences, err := decodeStoredCandidateEvidence(c.Evidence)
			if err != nil {
				return err
			}
			for _, o := range occurrences {
				if err := s.currentFacePhotoQuery(ctx, tx, o.ContentVersionID); errors.Is(err, ErrNotFound) {
					continue
				} else if err != nil {
					return err
				}
				f, err := scanPhotoFace(tx.QueryRowContext(ctx, `SELECT `+photoFaceColumns+` FROM photo_faces WHERE face_id=$1 AND content_version_id=$2`, o.EvidenceID, o.ContentVersionID))
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				if err != nil {
					return err
				}
				decisionFaces := []PhotoFace{f}
				if err := s.readFaceDecisionsTx(ctx, tx, decisionFaces); err != nil {
					return err
				}
				f = decisionFaces[0]
				if f.Dismissed || slices.Contains(f.RejectedPersonIDs, c.SuggestedPersonID) {
					continue
				}
				if f.AssertionID != "" {
					continue
				}
				face := FaceSuggestionFace{PhotoFace: f, Preview: &PersonFacePreview{ContentVersionID: o.ContentVersionID, Box: new(f.Box)}}
				if err := tx.QueryRowContext(ctx, `SELECT cv.recorded_at,pf.asset_id,pf.rotation,(a.hidden_at IS NOT NULL) FROM content_versions cv JOIN photo_files pf ON pf.node_id=cv.node_id JOIN photo_assets a ON a.asset_id=pf.asset_id WHERE cv.version_id=$1`, o.ContentVersionID).Scan(&face.PhotoRecordedAt, &face.Preview.AssetID, &face.Preview.Rotation, &face.Hidden); err != nil {
					return err
				}
				preview, err := visualPreviewGenerationByRecipeTx(ctx, tx, o.ContentVersionID, quality.GridRecipe)
				if err == nil {
					face.Preview.GenerationID = preview.GenerationID
				} else if !errors.Is(err, ErrNotFound) {
					return err
				}
				item.Faces = append(item.Faces, face)
			}
			slices.SortFunc(item.Faces, func(a, b FaceSuggestionFace) int {
				if a.PhotoRecordedAt < b.PhotoRecordedAt {
					return -1
				}
				if a.PhotoRecordedAt > b.PhotoRecordedAt {
					return 1
				}
				return strings.Compare(a.FaceID, b.FaceID)
			})
			result = append(result, item)
		}
		return nil
	})
	return result, total, err
}

// NewFaceCandidate retains the canonical face evidence within the existing bound.
func NewFaceCandidate(faces []FaceSuggestionFace, person, name string) (PersonMatchCandidate, error) {
	occurrences := make([]PersonCandidateOccurrence, len(faces))
	for i, f := range faces {
		occurrences[i] = PersonCandidateOccurrence{ContentVersionID: f.ContentVersionID, Role: "depicted", EvidenceKind: "face", EvidenceID: f.FaceID}
	}
	if len(occurrences) == 0 {
		return PersonMatchCandidate{}, ErrInvalidPhotoFace
	}
	raw, err := json.Marshal(occurrences)
	if err != nil {
		return PersonMatchCandidate{}, err
	}
	if name == "" {
		name = "Unnamed person"
	}
	return PersonMatchCandidate{ActorKey: "face:" + occurrences[0].EvidenceID, DisplayName: name, SuggestedPersonID: person, Reason: personCandidateReasonFaceMatch, Evidence: raw, OccurrenceCount: int64(len(occurrences))}, nil
}
