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
	PhotoNodeID     int64              `json:"photo_node_id"`
	PhotoRecordedAt string             `json:"photo_recorded_at"`
	Preview         *PersonFacePreview `json:"preview"`
	Hidden          bool               `json:"hidden"`
}
type FaceSuggestionSet struct {
	Set   EmbeddingSetRecord
	Faces []FaceSuggestionFace
}

func (s *Store) faceSuggestionInputsTx(ctx context.Context, tx *sql.Tx) ([]FaceSuggestionSet, error) {
	var result []FaceSuggestionSet
	ids, err := stringColumnTx(ctx, tx, "face vector heads", `SELECT h.embedding_set_id FROM embedding_heads h JOIN embedding_sets es ON es.embedding_set_id=h.embedding_set_id WHERE h.input_kind='face' ORDER BY h.embedding_set_id`)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+photoFaceColumns+` FROM photo_faces WHERE content_version_id IN (SELECT content_version_id FROM embedding_heads WHERE input_kind='face') ORDER BY face_id`)
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
	if err := s.readFaceDecisionsQuery(ctx, tx, faces, ""); err != nil {
		return nil, err
	}
	metadata := map[string]FaceSuggestionFace{}
	rows, err = tx.QueryContext(ctx, `SELECT f.face_id,v.node_id,v.recorded_at,COALESCE(p.person_id,''),COALESCE(p.display_name,''),a.hidden_at IS NOT NULL FROM photo_faces f JOIN content_versions v ON v.version_id=f.content_version_id JOIN photo_files member ON member.node_id=v.node_id JOIN photo_assets a ON a.asset_id=member.asset_id LEFT JOIN person_document_assertions assertion ON assertion.assertion_id=f.assertion_id AND assertion.action='assert' LEFT JOIN persons p ON p.person_id=assertion.person_id AND p.state<>'retired' WHERE f.content_version_id IN (SELECT content_version_id FROM embedding_heads WHERE input_kind='face') ORDER BY f.face_id`)
	if err != nil {
		return nil, err
	}
	defer func(rows *sql.Rows) { _ = rows.Close() }(rows)
	for rows.Next() {
		var face FaceSuggestionFace
		if err := rows.Scan(&face.FaceID, &face.PhotoNodeID, &face.PhotoRecordedAt, &face.PersonID, &face.PersonName, &face.Hidden); err != nil {
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
	for _, id := range ids {
		set, err := loadEmbeddingSetTx(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		eligible, err := embeddingSetEligibleTx(ctx, tx, set)
		if err != nil {
			return nil, err
		}
		if !eligible {
			continue
		}
		item := FaceSuggestionSet{Set: set, Faces: byVersion[set.ContentVersionID]}
		result = append(result, item)
	}
	return result, nil
}

// ReplaceFaceSuggestions preserves rejected evidence and supersedes open groups
// whose membership changed. Publishing and queue admission share one transaction.
func (s *Store) ReplaceFaceSuggestions(ctx context.Context, signature string, candidates []PersonMatchCandidate) error {
	return s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		current, err := s.faceMatchingInputsTx(ctx, tx)
		if err != nil {
			return err
		}
		if current.Signature != signature {
			return ErrStaleRevision
		}
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
			if err != nil {
				return err
			}
			if !retained {
				break
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
		visible += ` AND EXISTS(SELECT 1 FROM person_candidate_faces e JOIN photo_faces f ON f.face_id=e.face_id AND f.content_version_id=e.content_version_id JOIN content_versions cv ON cv.version_id=f.content_version_id JOIN nodes n ON n.id=cv.node_id JOIN photo_files pf ON pf.node_id=n.id JOIN photo_assets a ON a.asset_id=pf.asset_id WHERE e.candidate_id=person_match_candidates.candidate_id AND f.assertion_id IS NULL AND a.kind='photo' AND a.excluded_at IS NULL AND ` + currentPhotoSourcePredicate("pf", "n", "f.content_version_id") + `)`
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
				if err := tx.QueryRowContext(ctx, `SELECT cv.node_id,cv.recorded_at,pf.asset_id,pf.rotation,(a.hidden_at IS NOT NULL) FROM content_versions cv JOIN photo_files pf ON pf.node_id=cv.node_id JOIN photo_assets a ON a.asset_id=pf.asset_id WHERE cv.version_id=$1`, o.ContentVersionID).Scan(&face.PhotoNodeID, &face.PhotoRecordedAt, &face.Preview.AssetID, &face.Preview.Rotation, &face.Hidden); err != nil {
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
