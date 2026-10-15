package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type PersonFaceMergeConflictError struct {
	FaceIDs              []string
	HiddenConflict       bool
	InaccessibleConflict bool
	More                 bool
}

func (e *PersonFaceMergeConflictError) Error() string {
	message := "review contradictory face assignments and rejections before merging"
	if len(e.FaceIDs) > 0 {
		message += "; faces: " + strings.Join(e.FaceIDs, ", ")
	}
	if e.InaccessibleConflict {
		message += "; an excluded or trashed photo also needs review"
	}
	if e.HiddenConflict {
		message += "; a locked Hidden photo also needs review"
	}
	return message
}
func (e *PersonFaceMergeConflictError) Is(target error) bool { return target == ErrPersonMergeConflict }

type PersonMergeFaceCandidate struct {
	CandidateID         string `json:"candidate_id"`
	RetainedCandidateID string `json:"retained_candidate_id"`
}

func (s *Store) checkFaceMergeConflictsTx(ctx context.Context, tx *sql.Tx, a, b string) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT f.face_id,f.content_version_id FROM photo_faces f JOIN person_document_assertions p ON p.assertion_id=f.assertion_id JOIN person_match_candidates c ON c.reason='face_match' AND c.state='rejected' JOIN person_candidate_faces e ON e.candidate_id=c.candidate_id AND e.face_id=f.face_id AND e.content_version_id=f.content_version_id WHERE (p.action='assert' OR (p.action='withdrawn' AND EXISTS(SELECT 1 FROM person_document_assertions other WHERE other.person_id=c.suggested_person_id AND other.content_version_id=p.content_version_id AND other.role=p.role AND other.action='assert'))) AND ((p.person_id=? AND c.suggested_person_id=?) OR (p.person_id=? AND c.suggested_person_id=?)) ORDER BY f.face_id`, a, b, b, a)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	conflict := &PersonFaceMergeConflictError{}
	found := false
	for rows.Next() {
		var id, version string
		if err = rows.Scan(&id, &version); err != nil {
			return err
		}
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
		if len(conflict.FaceIDs) < 20 {
			conflict.FaceIDs = append(conflict.FaceIDs, id)
		} else {
			conflict.More = true
		}
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	return conflict
}

func moveFaceCandidatesTx(ctx context.Context, tx *sql.Tx, survivor, absorbed string) ([]PersonMergeFaceCandidate, error) {
	ids, err := collectPersonMergeColumnTx(ctx, tx, `SELECT candidate_id FROM person_match_candidates WHERE suggested_person_id=? AND reason='face_match' ORDER BY candidate_id`, absorbed)
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
		err = tx.QueryRowContext(ctx, `SELECT candidate_id FROM person_match_candidates WHERE suggested_person_id=? AND actor_key=? AND reason=? AND evidence_sha256=?`, survivor, c.ActorKey, c.Reason, c.EvidenceSHA256).Scan(&retained)
		if errors.Is(err, sql.ErrNoRows) {
			retained = id
			_, err = tx.ExecContext(ctx, `UPDATE person_match_candidates SET suggested_person_id=?,state=?,decided_person_id=?,decided_at=?,revision=revision+1 WHERE candidate_id=?`, survivor, c.State, nullableString(c.DecidedPersonID), nullableString(c.DecidedAt), id)
		} else if err == nil {
			keep, e := personCandidateByIDTx(ctx, tx, retained)
			if e != nil {
				return nil, e
			}
			if c.State == personCandidateStateRejected || keep.State == personCandidateStateOpen {
				if _, err = tx.ExecContext(ctx, `UPDATE person_match_candidates SET state=?,decided_person_id=?,decided_at=?,revision=revision+1 WHERE candidate_id=?`, c.State, nullableString(c.DecidedPersonID), nullableString(c.DecidedAt), retained); err != nil {
					return nil, err
				}
			}
			_, err = tx.ExecContext(ctx, `DELETE FROM person_match_candidates WHERE candidate_id=?`, id)
		}
		if err != nil {
			return nil, fmt.Errorf("moving face candidate history: %w", err)
		}
		moved = append(moved, PersonMergeFaceCandidate{CandidateID: id, RetainedCandidateID: retained})
	}
	return moved, nil
}
