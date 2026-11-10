package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type PersonFaceMergeConflict struct {
	FaceID, ContentVersionID string
}

type PersonFaceMergeConflictError struct {
	// Faces identifies each visible conflict on its exact version, including retained versions.
	Faces                []PersonFaceMergeConflict
	HiddenConflict       bool
	InaccessibleConflict bool
	More                 bool
}

func (e *PersonFaceMergeConflictError) Error() string {
	var message strings.Builder
	message.WriteString("review contradictory face assignments and rejections before merging")
	for _, face := range e.Faces {
		message.WriteString("; face " + face.FaceID + " on version " + face.ContentVersionID)
	}
	if e.More {
		message.WriteString("; more faces also conflict")
	}
	if e.InaccessibleConflict {
		message.WriteString("; an excluded or trashed photo also needs review")
	}
	if e.HiddenConflict {
		message.WriteString("; a locked Hidden photo also needs review")
	}
	return message.String()
}

func (e *PersonFaceMergeConflictError) Is(target error) bool { return target == ErrPersonMergeConflict }

type PersonMergeFaceCandidate struct {
	CandidateID         string `json:"candidate_id"`
	RetainedCandidateID string `json:"retained_candidate_id"`
}

func (s *Store) checkFaceMergeConflictsTx(ctx context.Context, tx *sql.Tx, a, b string) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT f.face_id,f.content_version_id FROM photo_faces f JOIN person_document_assertions p ON p.assertion_id=f.assertion_id JOIN person_match_candidates c ON c.reason='face_match' AND c.state='rejected' JOIN person_candidate_faces e ON e.candidate_id=c.candidate_id AND e.face_id=f.face_id AND e.content_version_id=f.content_version_id WHERE (p.action='assert' OR (p.action='withdrawn' AND EXISTS(SELECT 1 FROM person_document_assertions other WHERE other.person_id=c.suggested_person_id AND other.content_version_id=p.content_version_id AND other.role=p.role AND other.action='assert'))) AND ((p.person_id=$1 AND c.suggested_person_id=$2) OR (p.person_id=$3 AND c.suggested_person_id=$4)) ORDER BY f.face_id`, a, b, b, a)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var matches []PersonFaceMergeConflict
	for rows.Next() {
		var match PersonFaceMergeConflict
		if err := rows.Scan(&match.FaceID, &match.ContentVersionID); err != nil {
			return err
		}
		matches = append(matches, match)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	conflict := &PersonFaceMergeConflictError{}
	found := false
	for _, match := range matches {
		id, version := match.FaceID, match.ContentVersionID
		err = s.facePhotoQuery(ctx, tx, version)
		if errors.Is(err, ErrHiddenLocked) {
			found = true
			conflict.HiddenConflict = true
			continue
		}
		if errors.Is(err, ErrNotFound) {
			found = true
			conflict.InaccessibleConflict = true
			continue
		}
		if err != nil {
			return err
		}
		found = true
		if len(conflict.Faces) < 20 {
			conflict.Faces = append(conflict.Faces, PersonFaceMergeConflict{FaceID: id, ContentVersionID: version})
		} else {
			conflict.More = true
		}
	}
	if !found {
		return nil
	}
	return conflict
}

func moveFaceCandidatesTx(ctx context.Context, tx *sql.Tx, survivor, absorbed string) ([]PersonMergeFaceCandidate, error) {
	ids, err := collectPersonMergeColumnTx(ctx, tx, `SELECT candidate_id FROM person_match_candidates WHERE suggested_person_id=$1 AND reason='face_match' ORDER BY candidate_id`, absorbed)
	if err != nil {
		return nil, err
	}
	var moved []PersonMergeFaceCandidate
	for _, id := range ids {
		c, err := personCandidateByIDTx(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if c.State == personCandidateStateOpen {
			c.State = "superseded"
			c.DecidedAt = ""
			c.DecidedPersonID = ""
		}
		var retained string
		err = tx.QueryRowContext(ctx, `SELECT candidate_id FROM person_match_candidates WHERE suggested_person_id=$1 AND actor_key=$2 AND reason=$3 AND evidence_sha256=$4`, survivor, c.ActorKey, c.Reason, c.EvidenceSHA256).Scan(&retained)
		if errors.Is(err, sql.ErrNoRows) {
			retained = id
			_, err = tx.ExecContext(ctx, `UPDATE person_match_candidates SET suggested_person_id=$1,state=$2,decided_person_id=$3,decided_at=$4,revision=revision+1 WHERE candidate_id=$5`, survivor, c.State, nullableString(c.DecidedPersonID), nullableString(c.DecidedAt), id)
		} else if err == nil {
			keep, e := personCandidateByIDTx(ctx, tx, retained)
			if e != nil {
				return nil, e
			}
			if c.State == personCandidateStateRejected || c.State == "linked" && keep.State == personCandidateStateOpen {
				if _, err = tx.ExecContext(ctx, `UPDATE person_match_candidates SET state=$1,decided_person_id=$2,decided_at=$3,revision=revision+1 WHERE candidate_id=$4`, c.State, nullableString(c.DecidedPersonID), nullableString(c.DecidedAt), retained); err != nil {
					return nil, err
				}
			}
			_, err = tx.ExecContext(ctx, `DELETE FROM person_match_candidates WHERE candidate_id=$1`, id)
		}
		if err != nil {
			return nil, fmt.Errorf("moving face candidate history: %w", err)
		}
		moved = append(moved, PersonMergeFaceCandidate{CandidateID: id, RetainedCandidateID: retained})
	}
	return moved, nil
}
