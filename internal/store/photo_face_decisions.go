package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"slices"
)

type PhotoFaceDecisionRequest struct {
	FaceID         string `json:"face_id,omitzero" format:"uuid"`
	PersonID       string `json:"person_id,omitzero" format:"uuid"`
	Action         string `json:"action" enum:"name,retract,reject,dismiss,key,withdraw_rejection"`
	Revision       int64  `json:"revision" minimum:"1"`
	PersonRevision int64  `json:"person_revision,omitzero"`
}

type faceDecisionPerson struct {
	PersonID  string `json:"person_id"`
	KeyFaceID string `json:"key_face_id,omitzero"`
	Revision  int64  `json:"revision"`
}

type PhotoFaceDecisionState struct {
	VersionID  string                    `json:"content_version_id"`
	Faces      []PhotoFace               `json:"faces"`
	Assertions []PersonDocumentAssertion `json:"assertions"`
	Candidates []PersonMatchCandidate    `json:"candidates"`
	People     []faceDecisionPerson      `json:"people"`
}

type PhotoFaceDecisionReceipt struct {
	ReceiptID string                 `json:"receipt_id"`
	Before    PhotoFaceDecisionState `json:"before"`
	After     PhotoFaceDecisionState `json:"after"`
	UndoOf    string                 `json:"undo_of,omitzero"`
}

type photoFaceReceiptAfter struct {
	ReceiptID string                 `json:"receipt_id"`
	After     PhotoFaceDecisionState `json:"after"`
	UndoOf    string                 `json:"undo_of,omitzero"`
}

func decodeFaceDecisionReceipt(before, after []byte, id string) (PhotoFaceDecisionReceipt, error) {
	var r PhotoFaceDecisionReceipt
	if len(before)+len(after) > maxBatchTagReceiptJSONBytes {
		return r, ErrInvalidPhotoFace
	}
	var state photoFaceReceiptAfter
	if err := json.Unmarshal(before, &r.Before, json.RejectUnknownMembers(true)); err != nil {
		return r, err
	}
	if err := json.Unmarshal(after, &state, json.RejectUnknownMembers(true)); err != nil {
		return r, err
	}
	r.ReceiptID = state.ReceiptID
	r.After = state.After
	r.UndoOf = state.UndoOf
	return r, validateFaceDecisionReceipt(r, id)
}

func (s *Store) faceDecisionStateTx(ctx context.Context, tx *sql.Tx, versionID string, personIDs []string, candidateIDs ...string) (PhotoFaceDecisionState, error) {
	state := PhotoFaceDecisionState{VersionID: versionID}
	var err error
	state.Faces, err = photoFacesQuery(ctx, tx, versionID)
	if err != nil {
		return state, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT assertion_id,content_version_id,person_id,role,action,note,recorded_at,revision FROM person_document_assertions WHERE content_version_id=? AND role='depicted' ORDER BY assertion_id LIMIT 1001`, versionID)
	if err != nil {
		return state, err
	}
	for rows.Next() {
		var a PersonDocumentAssertion
		if err = rows.Scan(&a.AssertionID, &a.ContentVersionID, &a.PersonID, &a.Role, &a.Action, &a.Note, &a.RecordedAt, &a.Revision); err != nil {
			_ = rows.Close()
			return state, err
		}
		state.Assertions = append(state.Assertions, a)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return state, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT candidate_id FROM person_match_candidates c WHERE reason='face_match' AND EXISTS(SELECT 1 FROM json_each(c.evidence_json) e WHERE json_extract(e.value,'$.content_version_id')=?) ORDER BY candidate_id LIMIT 1001`, versionID)
	if err != nil {
		return state, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			_ = rows.Close()
			return state, err
		}
		ids = append(ids, id)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return state, err
	}
	ids = append(ids, candidateIDs...)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) > 1000 || len(state.Assertions) > 1000 {
		return state, ErrInvalidPhotoFace
	}
	for _, id := range ids {
		c, e := personCandidateByIDTx(ctx, tx, id)
		if e != nil {
			return state, e
		}
		state.Candidates = append(state.Candidates, c)
	}
	personIDs = slices.Clone(personIDs)
	slices.Sort(personIDs)
	personIDs = slices.Compact(personIDs)
	for _, id := range personIDs {
		var p faceDecisionPerson
		p.PersonID = id
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(key_face_id,''),revision FROM persons WHERE person_id=?`, id).Scan(&p.KeyFaceID, &p.Revision); err != nil {
			return state, err
		}
		state.People = append(state.People, p)
	}
	return state, nil
}

func writeFaceDecisionReceiptTx(ctx context.Context, tx *sql.Tx, before, after PhotoFaceDecisionState, undo string) (PhotoFaceDecisionReceipt, error) {
	id, err := newUUIDv4()
	if err != nil {
		return PhotoFaceDecisionReceipt{}, err
	}
	// Keep newly created records as inactive rows so undo never reuses their revisions.
	for _, a := range after.Assertions {
		if !slices.ContainsFunc(before.Assertions, func(b PersonDocumentAssertion) bool { return a.AssertionID == b.AssertionID }) {
			a.Action = "withdrawn"
			a.Revision = 0
			before.Assertions = append(before.Assertions, a)
		}
	}
	for _, c := range after.Candidates {
		if !slices.ContainsFunc(before.Candidates, func(b PersonMatchCandidate) bool { return c.CandidateID == b.CandidateID }) {
			c.State = "superseded"
			c.DecidedPersonID = ""
			c.DecidedAt = ""
			c.Revision = 0
			before.Candidates = append(before.Candidates, c)
		}
	}
	slices.SortFunc(before.Assertions, func(a, b PersonDocumentAssertion) int {
		if a.AssertionID < b.AssertionID {
			return -1
		}
		if a.AssertionID > b.AssertionID {
			return 1
		}
		return 0
	})
	slices.SortFunc(before.Candidates, func(a, b PersonMatchCandidate) int {
		if a.CandidateID < b.CandidateID {
			return -1
		}
		if a.CandidateID > b.CandidateID {
			return 1
		}
		return 0
	})
	result := PhotoFaceDecisionReceipt{ReceiptID: id, Before: before, After: after, UndoOf: undo}
	rawBefore, e := json.Marshal(before)
	if e != nil {
		return result, e
	}
	afterState := photoFaceReceiptAfter{ReceiptID: id, After: after, UndoOf: undo}
	rawAfter, e := json.Marshal(afterState)
	if e != nil {
		return result, e
	}
	if len(rawBefore)+len(rawAfter) > maxBatchTagReceiptJSONBytes {
		return result, ErrInvalidPhotoFace
	}
	err = writePhotoReceiptTx(ctx, tx, photoReceipt{ReceiptID: id, Operation: "face_decision", Before: before, After: afterState, BeforeRevision: 0, AfterRevision: 1})
	return result, err
}

func (s *Store) DecidePhotoFace(ctx context.Context, versionID string, request PhotoFaceDecisionRequest) (PhotoFaceDecisionReceipt, error) {
	if request.Revision < 1 || (request.FaceID != "" && validateUUIDv4(request.FaceID) != nil) || (request.PersonID != "" && validateUUIDv4(request.PersonID) != nil) {
		return PhotoFaceDecisionReceipt{}, ErrInvalidPhotoFace
	}
	if !slices.Contains([]string{"name", "retract", "reject", "dismiss", "key", "withdraw_rejection"}, request.Action) {
		return PhotoFaceDecisionReceipt{}, ErrInvalidPhotoFace
	}
	if slices.Contains([]string{"retract", "dismiss"}, request.Action) && request.PersonID != "" || request.Action == "key" && request.PersonRevision < 1 {
		return PhotoFaceDecisionReceipt{}, ErrInvalidPhotoFace
	}
	var result PhotoFaceDecisionReceipt
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		if err := s.facePhotoQuery(ctx, tx, versionID); err != nil {
			return err
		}
		var face PhotoFace
		var err error
		if request.FaceID != "" {
			face, err = scanPhotoFace(tx.QueryRowContext(ctx, `SELECT `+photoFaceColumns+` FROM photo_faces WHERE face_id=? AND content_version_id=?`, request.FaceID, versionID))
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
			if face.Revision != request.Revision {
				return ErrStaleRevision
			}
		}
		if request.FaceID == "" && !slices.Contains([]string{"key"}, request.Action) {
			return ErrInvalidPhotoFace
		}
		if slices.Contains([]string{"name", "reject", "withdraw_rejection", "key"}, request.Action) && request.PersonID == "" {
			return ErrInvalidPhotoFace
		}
		personIDs := []string{}
		if request.PersonID != "" {
			request.PersonID, err = resolveActiveCandidatePersonTx(ctx, tx, request.PersonID)
			if err != nil {
				return err
			}
			personIDs = append(personIDs, request.PersonID)

		}
		oldPerson := ""
		oldAction := ""
		if face.AssertionID != "" {
			a, e := personAssertionByIDTx(ctx, tx, face.AssertionID)
			if e != nil {
				return e
			}
			oldPerson = a.PersonID
			oldAction = a.Action
			personIDs = append(personIDs, oldPerson)
		}
		before, err := s.faceDecisionStateTx(ctx, tx, versionID, personIDs)
		if err != nil {
			return err
		}
		if request.Action == "key" {
			if err = fencePersonTx(ctx, tx, request.PersonID, request.PersonRevision); err != nil {
				return err
			}
			if request.FaceID != "" && (oldPerson != request.PersonID || oldAction != "assert") {
				return ErrPersonIdentityConflict
			}
			if _, err = tx.ExecContext(ctx, `UPDATE persons SET key_face_id=?,revision=revision+1,updated_at=? WHERE person_id=?`, nullableString(request.FaceID), nowRFC3339(), request.PersonID); err != nil {
				return err
			}
		} else {
			retract := request.Action == "retract" || request.Action == "dismiss" || request.Action == "name" && oldPerson != "" && oldPerson != request.PersonID || (request.Action == "reject" && oldPerson == request.PersonID)
			if retract && face.AssertionID != "" {
				if _, err = tx.ExecContext(ctx, `UPDATE photo_faces SET assertion_id=NULL WHERE face_id=?`, face.FaceID); err != nil {
					return err
				}
				if _, err = tx.ExecContext(ctx, `UPDATE persons SET key_face_id=NULL,revision=revision+1,updated_at=? WHERE key_face_id=?`, nowRFC3339(), face.FaceID); err != nil {
					return err
				}
				if _, err = tx.ExecContext(ctx, `UPDATE person_document_assertions SET action='withdrawn',revision=revision+1 WHERE assertion_id=? AND NOT EXISTS(SELECT 1 FROM photo_faces f WHERE f.assertion_id=person_document_assertions.assertion_id)`, face.AssertionID); err != nil {
					return err
				}
			}
			if request.Action == "name" {

				a, e := personAssertionByIdentityTx(ctx, tx, versionID, request.PersonID, "depicted")
				if errors.Is(e, sql.ErrNoRows) {
					a, _, e = ensurePersonAssertionTx(ctx, tx, versionID, request.PersonID, "depicted")
				}
				if e != nil {
					return e
				}
				if a.Action == "suppress" { return ErrPersonIdentityConflict }
				if _, err = tx.ExecContext(ctx, `UPDATE person_document_assertions SET action='assert',revision=revision+1 WHERE assertion_id=?`, a.AssertionID); err != nil { return err }
				if request.Action == "name" {
					if _, err = tx.ExecContext(ctx, `UPDATE photo_faces SET assertion_id=? WHERE face_id=?`, a.AssertionID, face.FaceID); err != nil {
						return err
					}
					if err = clearNamedFaceCandidatesTx(ctx, tx, &before, face.FaceID, request.PersonID); err != nil {
						return err
					}
				}
			}
			if request.Action == "withdraw_rejection" {
				if err = clearNamedFaceCandidatesTx(ctx, tx, &before, face.FaceID, request.PersonID); err != nil { return err }
			}
			if request.Action == "reject" || request.Action == "dismiss" {
				raw, e := json.Marshal([]PersonCandidateOccurrence{{ContentVersionID: versionID, Role: "depicted", EvidenceKind: "face", EvidenceID: face.FaceID}})
				if e != nil {
					return e
				}
				person := request.PersonID
				if request.Action == "dismiss" {
					person = ""
				}
				c, kept, e := s.retainPersonCandidate(ctx, tx, PersonMatchCandidate{ActorKey: "face:" + face.FaceID, DisplayName: "Face", SuggestedPersonID: person, Reason: "face_match", Evidence: raw}, true)
				if e != nil {
					return e
				}
				if !kept {
					return ErrInvalidPhotoFace
				}
				if _, err = tx.ExecContext(ctx, `UPDATE person_match_candidates SET state='rejected',decided_person_id=NULL,decided_at=?,revision=revision+1 WHERE candidate_id=?`, nowRFC3339(), c.CandidateID); err != nil {
					return err
				}
			}
		}
		if face.FaceID != "" {
			if _, err = tx.ExecContext(ctx, `UPDATE photo_faces SET revision=revision+1 WHERE face_id=?`, face.FaceID); err != nil {
				return err
			}
		}
		if err = advancePersonBindingEpochForVersionTx(ctx, tx, versionID); err != nil {
			return err
		}
		after, err := s.faceDecisionStateTx(ctx, tx, versionID, personIDs, faceStateCandidateIDs(before)...)
		if err != nil {
			return err
		}
		result, err = writeFaceDecisionReceiptTx(ctx, tx, before, after, "")
		return err
	})
	return result, err
}

func (s *Store) UndoPhotoFaceDecision(ctx context.Context, id string) (PhotoFaceDecisionReceipt, error) {
	var result PhotoFaceDecisionReceipt
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		var raw, beforeRaw []byte
		err := tx.QueryRowContext(ctx, `SELECT before_json,after_json FROM photo_change_receipts WHERE receipt_id=? AND operation='face_decision'`, id).Scan(&beforeRaw, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		r, err := decodeFaceDecisionReceipt(beforeRaw, raw, id)
		if err != nil {
			return err
		}
		if err = s.facePhotoQuery(ctx, tx, r.After.VersionID); err != nil {
			return err
		}
		var ids []string
		for _, p := range r.After.People {
			ids = append(ids, p.PersonID)
		}
		current, err := s.faceDecisionStateTx(ctx, tx, r.After.VersionID, ids, faceStateCandidateIDs(r.After)...)
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound) {
			return ErrStaleRevision
		}
		if err != nil {
			return err
		}
		currentHash, err := requestDigest(current)
		if err != nil {
			return err
		}
		afterHash, err := requestDigest(r.After)
		if err != nil {
			return err
		}
		if currentHash != afterHash {
			return ErrStaleRevision
		}
		for i, f := range r.Before.Faces {
			if f.Revision == r.After.Faces[i].Revision {
				continue
			}
			if _, err = tx.ExecContext(ctx, `UPDATE photo_faces SET assertion_id=?,revision=revision+1 WHERE face_id=?`, nullableString(f.AssertionID), f.FaceID); err != nil {
				return err
			}
		}
		for i, a := range r.Before.Assertions {
			if a.Revision == r.After.Assertions[i].Revision {
				continue
			}
			if _, err = tx.ExecContext(ctx, `UPDATE person_document_assertions SET action=?=?,revision=revision+1 WHERE assertion_id=?`, a.Action, a.AssertionID); err != nil {
				return err
			}
		}
		for i, c := range r.Before.Candidates {
			if c.Revision == r.After.Candidates[i].Revision {
				continue
			}
			if _, err = tx.ExecContext(ctx, `UPDATE person_match_candidates SET actor_key=?,state=?,decided_person_id=?,decided_at=?,evidence_json=?,evidence_sha256=?,occurrence_count=?,revision=revision+1 WHERE candidate_id=?`, c.ActorKey, c.State, nullableString(c.DecidedPersonID), nullableString(c.DecidedAt), c.Evidence, c.EvidenceSHA256, c.OccurrenceCount, c.CandidateID); err != nil {
				return err
			}
		}
		for i, p := range r.Before.People {
			if p.Revision == r.After.People[i].Revision {
				continue
			}
			if _, err = tx.ExecContext(ctx, `UPDATE persons SET key_face_id=?,revision=revision+1,updated_at=? WHERE person_id=?`, nullableString(p.KeyFaceID), nowRFC3339(), p.PersonID); err != nil {
				return err
			}
		}
		if err = advancePersonBindingEpochForVersionTx(ctx, tx, r.After.VersionID); err != nil {
			return err
		}
		for _, personID := range ids {
			if err = validateMergedPhotoFaces(ctx, tx, personID); err != nil {
				return err
			}
		}
		after, err := s.faceDecisionStateTx(ctx, tx, r.After.VersionID, ids, faceStateCandidateIDs(r.After)...)
		if err != nil {
			return err
		}
		result, err = writeFaceDecisionReceiptTx(ctx, tx, current, after, id)
		return err
	})
	return result, err
}

func validateFaceDecisionReceipt(r PhotoFaceDecisionReceipt, id string) error {
	if r.ReceiptID != id || validateUUIDv4(id) != nil || validateUUIDv4(r.After.VersionID) != nil || r.Before.VersionID != r.After.VersionID || r.UndoOf != "" && validateUUIDv4(r.UndoOf) != nil || r.UndoOf == id {
		return ErrInvalidPhotoFace
	}
	if len(r.Before.Faces) != len(r.After.Faces) || len(r.Before.Assertions) != len(r.After.Assertions) || len(r.Before.Candidates) != len(r.After.Candidates) || len(r.Before.People) != len(r.After.People) || len(r.After.Faces) > 1000 || len(r.After.Assertions) > 1000 || len(r.After.Candidates) > 1000 || len(r.After.People) > 1000 {
		return ErrInvalidPhotoFace
	}
	for i, a := range r.After.Faces {
		b := r.Before.Faces[i]
		if validatePhotoFace(a) != nil || validatePhotoFace(b) != nil || b.FaceID != a.FaceID || a.ContentVersionID != r.After.VersionID || b.ContentVersionID != a.ContentVersionID || a.Box != b.Box || a.CreatedAt != b.CreatedAt || a.Revision < b.Revision || i > 0 && r.After.Faces[i-1].FaceID >= a.FaceID {
			return ErrInvalidPhotoFace
		}
	}
	for i, a := range r.After.Assertions {
		b := r.Before.Assertions[i]
		if b.Revision < 0 || a.Revision < b.Revision {
			return ErrInvalidPhotoFace
		}
		if b.Revision == 0 {
			if b.Action != "withdrawn" {
				return ErrInvalidPhotoFace
			}
			b.Revision = 1
		}
		if validatePersonAssertion(a) != nil || validatePersonAssertion(b) != nil || a.Role != "depicted" || b.Role != a.Role || b.AssertionID != a.AssertionID || a.ContentVersionID != r.After.VersionID || b.ContentVersionID != a.ContentVersionID || b.PersonID != a.PersonID || b.Note != a.Note || b.RecordedAt != a.RecordedAt || i > 0 && r.After.Assertions[i-1].AssertionID >= a.AssertionID {
			return ErrInvalidPhotoFace
		}
	}
	for i, a := range r.After.Candidates {
		b := r.Before.Candidates[i]
		if b.CandidateID != a.CandidateID || a.Revision < b.Revision || b.Revision < 0 || b.SuggestedPersonID != a.SuggestedPersonID || b.CreatedAt != a.CreatedAt || i > 0 && r.After.Candidates[i-1].CandidateID >= a.CandidateID {
			return ErrInvalidPhotoFace
		}
		if b.Revision == 0 {
			if b.State != "superseded" {
				return ErrInvalidPhotoFace
			}
			b.Revision = 1
		}
		for _, c := range []PersonMatchCandidate{a, b} {
			if c.Reason != "face_match" {
				return ErrInvalidPhotoFace
			}
			if err := validateMetadataPersonCandidate(metadataPersonMatchCandidate{Type: metadataPersonCandidateType, CandidateID: c.CandidateID, ActorKey: c.ActorKey, DisplayName: c.DisplayName, SuggestedPersonID: optionalFaceString(c.SuggestedPersonID), Reason: c.Reason, EvidenceJSON: c.Evidence, EvidenceSHA256: c.EvidenceSHA256, OccurrenceCount: c.OccurrenceCount, Revision: c.Revision, State: c.State, DecidedPersonID: optionalFaceString(c.DecidedPersonID), CreatedAt: c.CreatedAt, DecidedAt: optionalFaceString(c.DecidedAt)}); err != nil {
				return err
			}
		}
	}
	for i, a := range r.After.People {
		b := r.Before.People[i]
		if validateUUIDv4(a.PersonID) != nil || a.PersonID != b.PersonID || a.Revision < b.Revision || b.Revision < 1 || a.KeyFaceID != "" && validateUUIDv4(a.KeyFaceID) != nil || b.KeyFaceID != "" && validateUUIDv4(b.KeyFaceID) != nil || i > 0 && r.After.People[i-1].PersonID >= a.PersonID {
			return ErrInvalidPhotoFace
		}
	}
	return nil
}

func optionalFaceString(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func (s *Store) readFaceDecisionsTx(ctx context.Context, tx *sql.Tx, faces []PhotoFace) error {
	if len(faces) == 0 {
		return nil
	}
	index := make(map[string]int, len(faces))
	for i, f := range faces {
		index[f.FaceID] = i
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT f.face_id,COALESCE(c.suggested_person_id,'') FROM person_match_candidates c JOIN json_each(c.evidence_json) e JOIN photo_faces f ON f.face_id=json_extract(e.value,'$.evidence_id') WHERE c.reason='face_match' AND c.state='rejected' AND f.content_version_id=? ORDER BY 1,2`, faces[0].ContentVersionID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var faceID, personID string
		if err = rows.Scan(&faceID, &personID); err != nil {
			return err
		}
		i, ok := index[faceID]
		if !ok {
			continue
		}
		if personID == "" {
			faces[i].Dismissed = true
		} else {
			if len(faces[i].RejectedPersonIDs) == 1000 {
				return ErrInvalidPhotoFace
			}
			faces[i].RejectedPersonIDs = append(faces[i].RejectedPersonIDs, personID)
		}
	}
	return rows.Err()
}

func (s *Store) personFaceDetailTx(ctx context.Context, tx *sql.Tx, id string) ([]PersonDocumentAssertion, string, bool, error) {
	var key string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(key_face_id,'') FROM persons WHERE person_id=?`, id).Scan(&key); err != nil {
		return nil, "", false, err
	}
	if key != "" {
		var v string
		if err := tx.QueryRowContext(ctx, `SELECT content_version_id FROM photo_faces WHERE face_id=?`, key).Scan(&v); err != nil {
			return nil, "", false, err
		}
		if err := s.facePhotoQuery(ctx, tx, v); err != nil {
			if !errors.Is(err, ErrHiddenLocked) && !errors.Is(err, ErrNotFound) {
				return nil, "", false, err
			}
			key = ""
		}
	}
	visible := "1=1"
	if _, err := s.hiddenSession(ctx, tx); errors.Is(err, ErrHiddenLocked) {
		visible = `NOT EXISTS(SELECT 1 FROM content_versions v JOIN photo_files f ON f.node_id=v.node_id JOIN photo_assets a ON a.asset_id=f.asset_id WHERE v.version_id=person_document_assertions.content_version_id AND a.hidden_at IS NOT NULL)`
	} else if err != nil {
		return nil, "", false, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT assertion_id,content_version_id,person_id,role,action,note,recorded_at,revision FROM person_document_assertions WHERE person_id=? AND `+visible+` ORDER BY assertion_id LIMIT 1001`, id)
	if err != nil {
		return nil, "", false, err
	}
	var all []PersonDocumentAssertion
	for rows.Next() {
		var a PersonDocumentAssertion
		if err = rows.Scan(&a.AssertionID, &a.ContentVersionID, &a.PersonID, &a.Role, &a.Action, &a.Note, &a.RecordedAt, &a.Revision); err != nil {
			_ = rows.Close()
			return nil, "", false, err
		}
		all = append(all, a)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return nil, "", false, err
	}
	truncated := len(all) > 1000
	if truncated {
		all = all[:1000]
	}
	result := make([]PersonDocumentAssertion, 0, len(all))
	for _, a := range all {
		if a.Role == "depicted" {
			if e := s.facePhotoQuery(ctx, tx, a.ContentVersionID); e != nil {
				if errors.Is(e, ErrHiddenLocked) || errors.Is(e, ErrNotFound) {
					continue
				}
				return nil, "", false, e
			}
		}
		result = append(result, a)
	}
	return result, key, truncated, nil
}

func clearNamedFaceCandidatesTx(ctx context.Context, tx *sql.Tx, before *PhotoFaceDecisionState, faceID, personID string) error {
	for _, c := range slices.Clone(before.Candidates) {
		if c.State != "open" && c.State != "rejected" || c.SuggestedPersonID != "" && c.SuggestedPersonID != personID {
			continue
		}
		occurrences, err := decodeStoredCandidateEvidence(c.Evidence)
		if err != nil {
			return err
		}
		kept := slices.DeleteFunc(slices.Clone(occurrences), func(o PersonCandidateOccurrence) bool { return o.EvidenceKind == "face" && o.EvidenceID == faceID })
		if len(kept) == len(occurrences) {
			continue
		}
		if len(kept) == 0 {
			if _, err = tx.ExecContext(ctx, `UPDATE person_match_candidates SET state='superseded',decided_at=NULL,decided_person_id=NULL,revision=revision+1 WHERE candidate_id=?`, c.CandidateID); err != nil {
				return err
			}
			continue
		}
		raw, err := json.Marshal(kept)
		if err != nil {
			return err
		}
		next, err := normalizePersonCandidate(PersonMatchCandidate{ActorKey: "face:" + kept[0].EvidenceID, DisplayName: c.DisplayName, SuggestedPersonID: c.SuggestedPersonID, Reason: c.Reason, Evidence: raw})
		if err != nil {
			return err
		}
		existing, e := personCandidateByIdentityTx(ctx, tx, next)
		if e == nil && existing.CandidateID != c.CandidateID {
			if _, err = tx.ExecContext(ctx, `UPDATE person_match_candidates SET state='superseded',decided_at=NULL,decided_person_id=NULL,revision=revision+1 WHERE candidate_id=?`, c.CandidateID); err != nil {
				return err
			}
			if c.State == "rejected" && existing.State != "rejected" {
				if !slices.ContainsFunc(before.Candidates, func(old PersonMatchCandidate) bool { return old.CandidateID == existing.CandidateID }) {
					before.Candidates = append(before.Candidates, existing)
				}
				if _, err = tx.ExecContext(ctx, `UPDATE person_match_candidates SET state='rejected',decided_at=?,decided_person_id=NULL,revision=revision+1 WHERE candidate_id=?`, c.DecidedAt, existing.CandidateID); err != nil {
					return err
				}
			}
			continue
		}
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if _, err = tx.ExecContext(ctx, `UPDATE person_match_candidates SET actor_key=?,evidence_json=?,evidence_sha256=?,occurrence_count=?,revision=revision+1 WHERE candidate_id=?`, next.ActorKey, next.Evidence, next.EvidenceSHA256, next.OccurrenceCount, c.CandidateID); err != nil {
			return err
		}
	}
	return nil
}

func faceStateCandidateIDs(state PhotoFaceDecisionState) []string {
	ids := make([]string, 0, len(state.Candidates))
	for _, c := range state.Candidates {
		ids = append(ids, c.CandidateID)
	}
	return ids
}
