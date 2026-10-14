package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type PersonFaceMergeConflict struct {
	FaceIDs        []string
	HiddenConflict bool
}

func (e *PersonFaceMergeConflict) Error() string {
	message := "review contradictory face assignments and rejections before merging"
	if len(e.FaceIDs) > 0 {
		message += "; faces: " + strings.Join(e.FaceIDs, ", ")
	}
	if e.HiddenConflict {
		message += "; a locked Hidden photo also needs review"
	}
	return message
}
func (e *PersonFaceMergeConflict) Is(target error) bool { return target == ErrPersonMergeConflict }

type PersonMergeRejectedFaceCandidate struct {
	CandidateID         string `json:"candidate_id"`
	RetainedCandidateID string `json:"retained_candidate_id"`
}

func (s *Store) checkFaceMergeConflictsTx(ctx context.Context, tx *sql.Tx, a, b string) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT f.face_id,f.content_version_id FROM photo_faces f JOIN person_document_assertions p ON p.assertion_id=f.assertion_id JOIN person_match_candidates c ON c.reason='face_match' AND c.state='rejected' JOIN json_each(c.evidence_json) e ON json_extract(e.value,'$.evidence_id')=f.face_id WHERE p.action='assert' AND ((p.person_id=? AND c.suggested_person_id=?) OR (p.person_id=? AND c.suggested_person_id=?)) ORDER BY f.face_id`, a, b, b, a)
	if err != nil {
		return err
	}
	conflict := &PersonFaceMergeConflict{}
	found := false
	for rows.Next() {
		var id, version string
		if err = rows.Scan(&id, &version); err != nil {
			_ = rows.Close()
			return err
		}
		found = true
		err = s.facePhotoQuery(ctx, tx, version)
		if errors.Is(err, ErrHiddenLocked) {
			conflict.HiddenConflict = true
			continue
		}
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			_ = rows.Close()
			return err
		}
		if len(conflict.FaceIDs) < 20 {
			conflict.FaceIDs = append(conflict.FaceIDs, id)
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

func moveRejectedFaceCandidatesTx(ctx context.Context, tx *sql.Tx, survivor, absorbed string) ([]PersonMergeRejectedFaceCandidate, error) {
	ids, err := collectPersonMergeColumnTx(ctx, tx, `SELECT candidate_id FROM person_match_candidates WHERE suggested_person_id=? AND reason='face_match' AND state='rejected' ORDER BY candidate_id`, absorbed)
	if err != nil {
		return nil, err
	}
	var moved []PersonMergeRejectedFaceCandidate
	for _, id := range ids {
		c, err := personCandidateByIDTx(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		var retained string
		err = tx.QueryRowContext(ctx, `SELECT candidate_id FROM person_match_candidates WHERE suggested_person_id=? AND actor_key=? AND reason=? AND evidence_sha256=?`, survivor, c.ActorKey, c.Reason, c.EvidenceSHA256).Scan(&retained)
		if errors.Is(err, sql.ErrNoRows) {
			retained = id
			_, err = tx.ExecContext(ctx, `UPDATE person_match_candidates SET suggested_person_id=?,revision=revision+1 WHERE candidate_id=?`, survivor, id)
		} else if err == nil {
			if _, err = tx.ExecContext(ctx, `UPDATE person_match_candidates SET state='rejected',decided_person_id=NULL,decided_at=?,revision=revision+1 WHERE candidate_id=?`, c.DecidedAt, retained); err != nil {
				return nil, err
			}
			_, err = tx.ExecContext(ctx, `DELETE FROM person_match_candidates WHERE candidate_id=?`, id)
		}
		if err != nil {
			return nil, fmt.Errorf("moving rejected face candidate: %w", err)
		}
		moved = append(moved, PersonMergeRejectedFaceCandidate{CandidateID: id, RetainedCandidateID: retained})
	}
	return moved, nil
}
