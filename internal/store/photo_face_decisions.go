package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"reflect"
	"slices"
)

const (
	faceDecisionName   = "name"
	faceDecisionReject = "reject"
)

type PhotoFaceDecisionRequest struct {
	OperationID    string `json:"operation_id,omitzero" format:"uuid"`
	FaceID         string `json:"face_id,omitzero" format:"uuid"`
	PersonID       string `json:"person_id,omitzero" format:"uuid"`
	Action         string `json:"action" enum:"name,retract,reject,dismiss,key,withdraw_rejection"`
	Revision       int64  `json:"revision,omitzero" minimum:"0"`
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
	RequestSHA256 string                 `json:"request_sha256,omitzero"`
	ReceiptID     string                 `json:"receipt_id"`
	Before        PhotoFaceDecisionState `json:"before"`
	After         PhotoFaceDecisionState `json:"after"`
	UndoOf        string                 `json:"undo_of,omitzero"`
}

type photoFaceReceiptAfter struct {
	RequestSHA256 string                 `json:"request_sha256,omitzero"`
	ReceiptID     string                 `json:"receipt_id"`
	After         PhotoFaceDecisionState `json:"after"`
	UndoOf        string                 `json:"undo_of,omitzero"`
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
	r.RequestSHA256 = state.RequestSHA256
	r.ReceiptID = state.ReceiptID
	r.After = state.After
	r.UndoOf = state.UndoOf
	return r, validateFaceDecisionReceipt(r, id)
}

func (s *Store) faceDecisionStateTx(ctx context.Context, tx *sql.Tx, versionID string, faceID string, personIDs []string, includeCandidates bool, candidateIDs ...string) (PhotoFaceDecisionState, error) {
	state := PhotoFaceDecisionState{VersionID: versionID}
	var err error
	if faceID != "" {
		f, e := scanPhotoFace(tx.QueryRowContext(ctx, `SELECT `+photoFaceColumns+` FROM photo_faces WHERE face_id=? AND content_version_id=?`, faceID, versionID))
		err = e
		if e == nil {
			state.Faces = []PhotoFace{f}
		}
	}
	if err != nil {
		return state, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT assertion_id,content_version_id,person_id,role,action,note,recorded_at,revision,photo_mark FROM person_document_assertions WHERE content_version_id=? AND role='depicted' AND person_id IN (SELECT value FROM json_each(?)) ORDER BY assertion_id`, versionID, personIDsJSON(personIDs))
	if err != nil {
		return state, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var a PersonDocumentAssertion
		if err = rows.Scan(&a.AssertionID, &a.ContentVersionID, &a.PersonID, &a.Role, &a.Action, &a.Note, &a.RecordedAt, &a.Revision, &a.PhotoMark); err != nil {
			return state, err
		}
		state.Assertions = append(state.Assertions, a)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return state, err
	}
	faceRows, err := tx.QueryContext(ctx, `SELECT `+photoFaceColumns+` FROM photo_faces WHERE content_version_id=? AND face_id<>? AND assertion_id IN (SELECT assertion_id FROM person_document_assertions WHERE content_version_id=? AND role='depicted' AND person_id IN (SELECT value FROM json_each(?))) ORDER BY face_id`, versionID, faceID, versionID, personIDsJSON(personIDs))
	if err != nil {
		return state, err
	}
	defer func() { _ = faceRows.Close() }()
	for faceRows.Next() {
		face, err := scanPhotoFace(faceRows)
		if err != nil {
			return state, err
		}
		state.Faces = append(state.Faces, face)
	}
	if err := errors.Join(faceRows.Err(), faceRows.Close()); err != nil {
		return state, err
	}
	slices.SortFunc(state.Faces, func(a, b PhotoFace) int { return cmp.Compare(a.FaceID, b.FaceID) })
	candidateRows, err := tx.QueryContext(ctx, `SELECT c.candidate_id FROM person_candidate_faces e JOIN person_match_candidates c ON c.candidate_id=e.candidate_id WHERE ? AND e.face_id=? AND c.state IN ('open','rejected') AND c.reason='face_match' ORDER BY c.candidate_id`, includeCandidates, faceID)
	if err != nil {
		return state, err
	}
	defer func() { _ = candidateRows.Close() }()
	var ids []string
	for candidateRows.Next() {
		var id string
		if err = candidateRows.Scan(&id); err != nil {
			return state, err
		}
		ids = append(ids, id)
	}
	err = errors.Join(candidateRows.Err(), candidateRows.Close())
	if err != nil {
		return state, err
	}
	ids = append(ids, candidateIDs...)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(state.Assertions) > 1000 {
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

func writeFaceDecisionReceiptTx(ctx context.Context, tx *sql.Tx, before, after PhotoFaceDecisionState, undo, operationID, requestSHA string) (PhotoFaceDecisionReceipt, error) {
	id := operationID
	var err error
	if id == "" {
		id, err = newUUIDv4()
		if err != nil {
			return PhotoFaceDecisionReceipt{}, err
		}
	}
	// Keep newly created records as inactive rows so undo never reuses their revisions.
	for _, a := range after.Assertions {
		if !slices.ContainsFunc(before.Assertions, func(b PersonDocumentAssertion) bool { return a.AssertionID == b.AssertionID }) {
			a.Action = personAssertionActionWithdrawn
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
	before.Faces, after.Faces = changedFaceRows(before.Faces, after.Faces)
	var dependencies []PersonDocumentAssertion
	for i, a := range after.Assertions {
		if reflect.DeepEqual(before.Assertions[i], a) && slices.ContainsFunc(before.Faces, func(f PhotoFace) bool {
			return slices.ContainsFunc(after.Faces, func(next PhotoFace) bool {
				return next.FaceID == f.FaceID && next.AssertionID != f.AssertionID && (f.AssertionID == a.AssertionID || next.AssertionID == a.AssertionID)
			})
		}) {
			dependencies = append(dependencies, a)
		}
	}
	before.Assertions, after.Assertions = changedFaceRows(before.Assertions, after.Assertions)
	before.Assertions = append(before.Assertions, dependencies...)
	after.Assertions = append(after.Assertions, dependencies...)
	slices.SortFunc(before.Assertions, func(a, b PersonDocumentAssertion) int { return cmp.Compare(a.AssertionID, b.AssertionID) })
	slices.SortFunc(after.Assertions, func(a, b PersonDocumentAssertion) int { return cmp.Compare(a.AssertionID, b.AssertionID) })
	before.Candidates, after.Candidates = changedFaceRows(before.Candidates, after.Candidates)
	before.People, after.People = changedFaceRows(before.People, after.People)
	result := PhotoFaceDecisionReceipt{RequestSHA256: requestSHA, ReceiptID: id, Before: before, After: after, UndoOf: undo}
	if err = validateFaceDecisionReceipt(result, id); err != nil {
		return result, err
	}
	afterState := photoFaceReceiptAfter{RequestSHA256: requestSHA, ReceiptID: id, After: after, UndoOf: undo}
	var assetID string
	if err = tx.QueryRowContext(ctx, `SELECT f.asset_id FROM content_versions v JOIN photo_files f ON f.node_id=v.node_id WHERE v.version_id=?`, before.VersionID).Scan(&assetID); err != nil {
		return result, err
	}
	err = writePhotoReceiptTx(ctx, tx, photoReceipt{ReceiptID: id, Operation: "face_decision", AssetID: assetID, Before: before, After: afterState, BeforeRevision: 0, AfterRevision: 1})
	return result, err
}

func (s *Store) DecidePhotoFace(ctx context.Context, versionID string, request PhotoFaceDecisionRequest) (PhotoFaceDecisionReceipt, error) {
	if request.OperationID != "" && validateUUIDv4(request.OperationID) != nil {
		return PhotoFaceDecisionReceipt{}, ErrInvalidPhotoFace
	}
	if (request.Revision < 1 && (request.Action != "key" || request.FaceID != "")) || (request.FaceID != "" && validateUUIDv4(request.FaceID) != nil) || (request.PersonID != "" && validateUUIDv4(request.PersonID) != nil) {
		return PhotoFaceDecisionReceipt{}, ErrInvalidPhotoFace
	}
	if !slices.Contains([]string{faceDecisionName, "retract", faceDecisionReject, "dismiss", "key", "withdraw_rejection"}, request.Action) {
		return PhotoFaceDecisionReceipt{}, ErrInvalidPhotoFace
	}
	if slices.Contains([]string{"retract", "dismiss"}, request.Action) && request.PersonID != "" || request.Action == "key" && request.PersonRevision < 1 {
		return PhotoFaceDecisionReceipt{}, ErrInvalidPhotoFace
	}
	requestSHA, err := requestDigest([]any{versionID, request})
	if err != nil {
		return PhotoFaceDecisionReceipt{}, err
	}
	var result PhotoFaceDecisionReceipt
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		if err := s.facePhotoQuery(ctx, tx, versionID); err != nil {
			return err
		}
		replay, found, e := replayFaceDecisionTx(ctx, tx, request.OperationID, requestSHA)
		if e != nil {
			return e
		}
		if found {
			result = replay
			return nil
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
		if request.FaceID == "" && request.Action != "key" {
			return ErrInvalidPhotoFace
		}
		if slices.Contains([]string{faceDecisionName, faceDecisionReject, "withdraw_rejection", "key"}, request.Action) && request.PersonID == "" {
			return ErrInvalidPhotoFace
		}
		personIDs := []string{}
		if request.PersonID != "" {
			if request.Action == "withdraw_rejection" {
				var exists bool
				err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM persons WHERE person_id=?)`, request.PersonID).Scan(&exists)
				if err == nil && !exists {
					return ErrNotFound
				}
			} else {
				request.PersonID, err = resolveActiveCandidatePersonTx(ctx, tx, request.PersonID)
			}
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
		before, err := s.faceDecisionStateTx(ctx, tx, versionID, face.FaceID, personIDs, slices.Contains([]string{faceDecisionName, faceDecisionReject, "dismiss", "withdraw_rejection"}, request.Action))
		if err != nil {
			return err
		}
		if request.Action == "key" {
			if err = fencePersonTx(ctx, tx, request.PersonID, request.PersonRevision); err != nil {
				return err
			}
			if request.FaceID != "" && (oldPerson != request.PersonID || oldAction != personAssertionActionAssert) {
				return ErrPersonIdentityConflict
			}
			if _, err = tx.ExecContext(ctx, `UPDATE persons SET key_face_id=?,revision=revision+1,updated_at=? WHERE person_id=?`, nullableString(request.FaceID), nowRFC3339(), request.PersonID); err != nil {
				return err
			}
		} else {
			retract := request.Action == "retract" || request.Action == "dismiss" || request.Action == faceDecisionName && oldPerson != "" && oldPerson != request.PersonID || (request.Action == faceDecisionReject && oldPerson == request.PersonID)
			if retract && face.AssertionID != "" {
				if _, err = tx.ExecContext(ctx, `UPDATE photo_faces SET assertion_id=NULL WHERE face_id=?`, face.FaceID); err != nil {
					return err
				}
				if _, err = tx.ExecContext(ctx, `UPDATE persons SET key_face_id=NULL,revision=revision+1,updated_at=? WHERE key_face_id=?`, nowRFC3339(), face.FaceID); err != nil {
					return err
				}
				if _, err = tx.ExecContext(ctx, `UPDATE person_document_assertions SET action='withdrawn',revision=revision+1 WHERE assertion_id=? AND photo_mark=0 AND action='assert' AND NOT EXISTS(SELECT 1 FROM photo_faces f WHERE f.assertion_id=person_document_assertions.assertion_id)`, face.AssertionID); err != nil {
					return err
				}
			}
			if request.Action == faceDecisionName {
				a, _, e := ensurePersonAssertionTx(ctx, tx, versionID, request.PersonID, personAssertionRoleDepicted)
				if e != nil {
					return e
				}
				if _, err = tx.ExecContext(ctx, `UPDATE photo_faces SET assertion_id=? WHERE face_id=?`, a.AssertionID, face.FaceID); err != nil {
					return err
				}
				if err = s.clearNamedFaceCandidatesTx(ctx, tx, &before, face.FaceID, request.PersonID, request.Action); err != nil {
					return err
				}
			}
			if request.Action == "withdraw_rejection" {
				if err = s.clearNamedFaceCandidatesTx(ctx, tx, &before, face.FaceID, request.PersonID, request.Action); err != nil {
					return err
				}
			}
			if request.Action == faceDecisionReject || request.Action == "dismiss" {
				if err = s.clearNamedFaceCandidatesTx(ctx, tx, &before, face.FaceID, request.PersonID, request.Action); err != nil {
					return err
				}
				raw, e := json.Marshal([]PersonCandidateOccurrence{{ContentVersionID: versionID, Role: personAssertionRoleDepicted, EvidenceKind: "face", EvidenceID: face.FaceID}})
				if e != nil {
					return e
				}
				person := request.PersonID
				if request.Action == "dismiss" {
					person = ""
				}
				c, kept, e := s.retainPersonCandidate(ctx, tx, PersonMatchCandidate{ActorKey: "face:" + face.FaceID, DisplayName: "Face", SuggestedPersonID: person, Reason: personCandidateReasonFaceMatch, Evidence: raw}, true)
				if e != nil {
					return e
				}
				if !kept {
					return ErrInvalidPhotoFace
				}
				if c.State != personCandidateStateOpen && !slices.ContainsFunc(before.Candidates, func(old PersonMatchCandidate) bool { return old.CandidateID == c.CandidateID }) {
					before.Candidates = append(before.Candidates, c)
				}
				if _, err = tx.ExecContext(ctx, `UPDATE person_match_candidates SET state='rejected',decided_person_id=NULL,decided_at=?,revision=revision+1 WHERE candidate_id=?`, nowRFC3339(), c.CandidateID); err != nil {
					return err
				}
			}
		}
		if request.Action == faceDecisionReject {
			if err = requirePhotoFaceRejectionBound(ctx, tx, face.FaceID, ""); err != nil {
				return err
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
		after, err := s.faceDecisionStateTx(ctx, tx, versionID, face.FaceID, personIDs, slices.Contains([]string{faceDecisionName, faceDecisionReject, "dismiss", "withdraw_rejection"}, request.Action), faceStateCandidateIDs(before)...)
		if err != nil {
			return err
		}
		result, err = writeFaceDecisionReceiptTx(ctx, tx, before, after, "", request.OperationID, requestSHA)
		return err
	})
	return result, err
}

func requirePhotoFaceRejectionBound(ctx context.Context, tx metadataQuerier, faceID, candidateID string) error {
	var exceeded bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT m.face_id FROM person_candidate_faces m JOIN photo_faces f ON f.face_id=m.face_id AND f.content_version_id=m.content_version_id JOIN person_match_candidates c ON c.candidate_id=m.candidate_id WHERE ((?='' AND ?='') OR m.face_id=? OR m.face_id IN (SELECT face_id FROM person_candidate_faces WHERE candidate_id=?)) AND c.state='rejected' GROUP BY m.face_id HAVING COUNT(DISTINCT c.suggested_person_id)>1000)`, faceID, candidateID, faceID, candidateID).Scan(&exceeded)
	if err != nil {
		return err
	}
	if exceeded {
		return ErrPhotoFaceRejectionLimit
	}
	return nil
}

func (s *Store) UndoPhotoFaceDecision(ctx context.Context, id string) (PhotoFaceDecisionReceipt, error) {
	return s.UndoPhotoFaceDecisionWithOperationID(ctx, id, "")
}

func (s *Store) UndoPhotoFaceDecisionWithOperationID(ctx context.Context, id, operationID string) (PhotoFaceDecisionReceipt, error) {
	if operationID != "" && validateUUIDv4(operationID) != nil {
		return PhotoFaceDecisionReceipt{}, ErrInvalidPhotoFace
	}
	requestSHA, e := requestDigest([]string{"undo", id, operationID})
	if e != nil {
		return PhotoFaceDecisionReceipt{}, e
	}
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
		replay, found, e := replayFaceDecisionTx(ctx, tx, operationID, requestSHA)
		if e != nil {
			return e
		}
		if found {
			result = replay
			return nil
		}
		var ids []string
		for _, p := range r.After.People {
			ids = append(ids, p.PersonID)
		}
		current, err := faceDecisionRowsTx(ctx, tx, r.After)
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound) {
			return ErrStaleRevision
		}
		if err != nil {
			return err
		}
		// Unchanged active dependencies fence identity while allowing automatic withdrawal.
		fenced := current
		fenced.Assertions = slices.Clone(current.Assertions)
		for i, a := range r.After.Assertions {
			if a.Action != personAssertionActionAssert || !reflect.DeepEqual(r.Before.Assertions[i], a) {
				continue
			}
			observed := current.Assertions[i]
			if observed.Action == personAssertionActionWithdrawn {
				automatic, err := automaticFaceWithdrawalTx(ctx, tx, observed)
				if err != nil {
					return err
				}
				if !automatic {
					return ErrStaleRevision
				}
			} else if observed.Action != personAssertionActionAssert {
				return ErrStaleRevision
			}
			observed.Action, observed.Revision = a.Action, a.Revision
			fenced.Assertions[i] = observed
		}
		currentHash, err := requestDigest(fenced)
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
		// Capture dependencies omitted by older receipts so reconciliation stays undoable.
		var detached []string
		var attached []string
		for i, f := range r.After.Faces {
			old := r.Before.Faces[i].AssertionID
			if f.AssertionID == old {
				continue
			}
			if f.AssertionID != "" {
				detached = append(detached, f.AssertionID)
			}
			if old != "" {
				attached = append(attached, old)
			}
			for _, assertionID := range []string{f.AssertionID, old} {
				if assertionID == "" {
					continue
				}
				a, e := personAssertionByIDTx(ctx, tx, assertionID)
				if e != nil {
					return e
				}
				if assertionID == old {
					resolved, e := resolveActiveCandidatePersonTx(ctx, tx, a.PersonID)
					if e != nil {
						return e
					}
					wasSuppressed := slices.ContainsFunc(r.Before.Assertions, func(old PersonDocumentAssertion) bool {
						return old.AssertionID == a.AssertionID && old.Action == "suppress"
					})
					if a.Action == personAssertionActionWithdrawn && !slices.ContainsFunc(r.After.Assertions, func(after PersonDocumentAssertion) bool { return after.AssertionID == a.AssertionID }) {
						automatic, e := automaticFaceWithdrawalTx(ctx, tx, a)
						if e != nil {
							return e
						}
						if !automatic {
							return ErrStaleRevision
						}
					}
					if resolved != a.PersonID || a.Action == "suppress" && !wasSuppressed {
						return ErrStaleRevision
					}
				}
				if !slices.ContainsFunc(current.Assertions, func(v PersonDocumentAssertion) bool { return v.AssertionID == assertionID }) {
					current.Assertions = append(current.Assertions, a)
				}
			}
		}
		slices.SortFunc(current.Assertions, func(a, b PersonDocumentAssertion) int { return cmp.Compare(a.AssertionID, b.AssertionID) })
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
			if a.Action == personAssertionActionWithdrawn && a.Revision > 0 {
				var faceIDs []string
				for _, face := range r.After.Faces {
					faceIDs = append(faceIDs, face.FaceID)
				}
				var shared bool
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM photo_faces WHERE assertion_id=? AND face_id NOT IN (SELECT value FROM json_each(?)))`, a.AssertionID, personIDsJSON(faceIDs)).Scan(&shared); err != nil {
					return err
				}
				if shared {
					return ErrStaleRevision
				}
			}
			if _, err = tx.ExecContext(ctx, `UPDATE person_document_assertions SET action=CASE WHEN ?=0 AND ?='withdrawn' AND EXISTS(SELECT 1 FROM photo_faces f WHERE f.assertion_id=person_document_assertions.assertion_id) THEN 'assert' ELSE ? END,photo_mark=?,revision=revision+1 WHERE assertion_id=?`, a.Revision, a.Action, a.Action, a.PhotoMark, a.AssertionID); err != nil {
				return err
			}
		}
		for i, c := range r.Before.Candidates {
			if c.Revision == r.After.Candidates[i].Revision {
				continue
			}
			if _, err = tx.ExecContext(ctx, `UPDATE person_match_candidates SET state=?,decided_person_id=?,decided_at=?,revision=revision+1 WHERE candidate_id=?`, c.State, nullableString(c.DecidedPersonID), nullableString(c.DecidedAt), c.CandidateID); err != nil {
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
		for _, assertionID := range detached {
			if _, err = tx.ExecContext(ctx, `UPDATE person_document_assertions SET action='withdrawn',revision=revision+1 WHERE assertion_id=? AND photo_mark=0 AND action='assert' AND NOT EXISTS(SELECT 1 FROM photo_faces WHERE assertion_id=?)`, assertionID, assertionID); err != nil {
				return err
			}
		}
		for _, assertionID := range attached {
			if slices.ContainsFunc(r.Before.Assertions, func(a PersonDocumentAssertion) bool {
				return a.AssertionID == assertionID && a.Revision > 0 && a.Action == personAssertionActionWithdrawn
			}) {
				continue
			}
			if _, err = tx.ExecContext(ctx, `UPDATE person_document_assertions SET action='assert',revision=revision+1 WHERE assertion_id=? AND action='withdrawn'`, assertionID); err != nil {
				return err
			}
		}
		count, err := photoAssertionCount(ctx, tx, r.After.VersionID, "")
		if err != nil {
			return err
		}
		if count > 1000 {
			return ErrPhotoFaceLimit
		}
		if len(r.Before.Candidates) > 0 {
			var openCount int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_match_candidates WHERE state='open'`).Scan(&openCount); err != nil {
				return err
			}
			if openCount > maxOpenPersonCandidates {
				return ErrInvalidPhotoFace
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
		after, err := faceDecisionRowsTx(ctx, tx, current)
		if err != nil {
			return err
		}
		result, err = writeFaceDecisionReceiptTx(ctx, tx, current, after, id, operationID, requestSHA)
		return err
	})
	return result, err
}

func validateFaceDecisionReceipt(r PhotoFaceDecisionReceipt, id string) error {
	if r.RequestSHA256 != "" && validateCatalogSHA256(r.RequestSHA256, "face decision request") != nil {
		return ErrInvalidPhotoFace
	}
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
			if b.Action != personAssertionActionWithdrawn {
				return ErrInvalidPhotoFace
			}
			b.Revision = 1
		}
		if validatePersonAssertion(a) != nil || validatePersonAssertion(b) != nil || a.Role != personAssertionRoleDepicted || b.Role != a.Role || b.AssertionID != a.AssertionID || a.ContentVersionID != r.After.VersionID || b.ContentVersionID != a.ContentVersionID || b.PersonID != a.PersonID || b.Note != a.Note || b.PhotoMark != a.PhotoMark || b.RecordedAt != a.RecordedAt || i > 0 && r.After.Assertions[i-1].AssertionID >= a.AssertionID {
			return ErrInvalidPhotoFace
		}
	}
	for i, a := range r.After.Candidates {
		b := r.Before.Candidates[i]
		if b.CandidateID != a.CandidateID || a.Revision < b.Revision || b.Revision < 0 || b.SuggestedPersonID != a.SuggestedPersonID || b.CreatedAt != a.CreatedAt || b.ActorKey != a.ActorKey || b.EvidenceSHA256 != a.EvidenceSHA256 || !reflect.DeepEqual(b.Evidence, a.Evidence) || i > 0 && r.After.Candidates[i-1].CandidateID >= a.CandidateID {
			return ErrInvalidPhotoFace
		}
		if b.Revision == 0 {
			if b.State != "superseded" {
				return ErrInvalidPhotoFace
			}
			b.Revision = 1
		}
		for _, c := range []PersonMatchCandidate{a, b} {
			if c.Reason != personCandidateReasonFaceMatch {
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
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT f.face_id,COALESCE(c.suggested_person_id,'') FROM person_match_candidates c JOIN person_candidate_faces e ON e.candidate_id=c.candidate_id JOIN photo_faces f ON f.face_id=e.face_id WHERE c.reason='face_match' AND c.state='rejected' AND f.content_version_id=? ORDER BY 1,2`, faces[0].ContentVersionID)
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

func (s *Store) personFaceDetailTx(ctx context.Context, tx *sql.Tx, id string, page PersonAssertionPage) ([]PersonDocumentAssertion, string, bool, error) {
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
		visible = personVersionVisibleSQL("pa.content_version_id")
	} else if err != nil {
		return nil, "", false, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT pa.assertion_id,pa.content_version_id,pa.person_id,pa.role,pa.action,pa.note,pa.recorded_at,pa.revision,pa.photo_mark,COALESCE(member.role,''),n.id,n.name,COALESCE(cv.mime_type,'') FROM person_document_assertions pa JOIN content_versions cv ON cv.version_id=pa.content_version_id JOIN nodes n ON n.id=cv.node_id LEFT JOIN photo_files member ON member.node_id=n.id LEFT JOIN photo_assets asset ON asset.asset_id=member.asset_id WHERE pa.person_id=? AND pa.assertion_id>? AND n.trashed_at IS NULL AND (pa.role<>'depicted' OR (asset.kind='photo' AND asset.excluded_at IS NULL AND n.kind='file' AND member.role IN ('image','raw'))) AND `+visible+` ORDER BY pa.assertion_id`, id, page.After)
	if err != nil {
		return nil, "", false, err
	}
	defer func() { _ = rows.Close() }()
	var result []PersonDocumentAssertion
	truncated := false
	for rows.Next() {
		var a PersonDocumentAssertion
		var role string
		var node Node
		if e := rows.Scan(&a.AssertionID, &a.ContentVersionID, &a.PersonID, &a.Role, &a.Action, &a.Note, &a.RecordedAt, &a.Revision, &a.PhotoMark, &role, &node.ID, &node.Name, &node.MimeType); e != nil {
			return nil, "", false, e
		}
		if a.Role == personAssertionRoleDepicted && !validFacePhotoNode(role, node) {
			continue
		}

		if len(result) == page.Limit {
			truncated = true
			break
		}
		result = append(result, a)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return nil, "", false, err
	}

	return result, key, truncated, nil
}

func (s *Store) clearNamedFaceCandidatesTx(ctx context.Context, tx *sql.Tx, before *PhotoFaceDecisionState, faceID, personID, action string) error {
	for _, c := range slices.Clone(before.Candidates) {
		if action == faceDecisionReject && c.State == personCandidateStateRejected && c.SuggestedPersonID == "" {
			continue
		}
		if action == "withdraw_rejection" && (c.State != personCandidateStateRejected || c.SuggestedPersonID != personID) {
			continue
		}
		if c.State != personCandidateStateOpen && c.State != personCandidateStateRejected || (action != "dismiss" || c.State != personCandidateStateOpen) && c.SuggestedPersonID != "" && c.SuggestedPersonID != personID {
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
		if _, err = tx.ExecContext(ctx, `UPDATE person_match_candidates SET state='superseded',decided_at=NULL,decided_person_id=NULL,revision=revision+1 WHERE candidate_id=?`, c.CandidateID); err != nil {
			return err
		}
		existing, e := personCandidateByIdentityTx(ctx, tx, next)
		if e == nil {
			if !slices.ContainsFunc(before.Candidates, func(old PersonMatchCandidate) bool { return old.CandidateID == existing.CandidateID }) {
				before.Candidates = append(before.Candidates, existing)
			}
		} else if errors.Is(e, sql.ErrNoRows) {
			var kept bool
			existing, kept, e = s.retainPersonCandidate(ctx, tx, next, c.State == personCandidateStateRejected)
			if e != nil {
				return e
			}
			if !kept {
				return ErrInvalidPhotoFace
			}
			placeholder := existing
			placeholder.State = "superseded"
			placeholder.Revision = 0
			before.Candidates = append(before.Candidates, placeholder)
		} else {
			return e
		}
		if c.State == personCandidateStateOpen && existing.State == "superseded" {
			_, full, err := openPersonCandidateCount(ctx, tx)
			if err != nil {
				return err
			}
			if full {
				return ErrInvalidPhotoFace
			}
			if _, err := tx.ExecContext(ctx, `UPDATE person_match_candidates SET state='open',decided_at=NULL,decided_person_id=NULL,revision=revision+1 WHERE candidate_id=?`, existing.CandidateID); err != nil {
				return err
			}
		}
		if c.State == personCandidateStateRejected {
			if _, err = tx.ExecContext(ctx, `UPDATE person_match_candidates SET state='rejected',decided_at=?,decided_person_id=NULL,revision=revision+1 WHERE candidate_id=?`, c.DecidedAt, existing.CandidateID); err != nil {
				return err
			}
		}
	}
	return nil
}

func automaticFaceWithdrawalTx(ctx context.Context, tx *sql.Tx, assertion PersonDocumentAssertion) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT receipt_id,before_json,after_json FROM photo_change_receipts INDEXED BY photo_change_receipts_face_version WHERE operation='face_decision' AND json_extract(before_json,'$.content_version_id')=?`, assertion.ContentVersionID)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	automatic := false
	for rows.Next() {
		var id string
		var before, after []byte
		if err := rows.Scan(&id, &before, &after); err != nil {
			return false, err
		}
		receipt, err := decodeFaceDecisionReceipt(before, after, id)
		if err != nil {
			return false, err
		}
		if receipt.Before.VersionID != assertion.ContentVersionID {
			continue
		}
		detached := slices.ContainsFunc(receipt.After.Assertions, func(a PersonDocumentAssertion) bool {
			return a.AssertionID == assertion.AssertionID && a.Revision == assertion.Revision && a.Action == personAssertionActionWithdrawn
		})
		active := slices.ContainsFunc(receipt.Before.Assertions, func(a PersonDocumentAssertion) bool {
			return a.AssertionID == assertion.AssertionID && a.Action == personAssertionActionAssert
		})
		referenced := slices.ContainsFunc(receipt.After.Faces, func(f PhotoFace) bool { return f.AssertionID == assertion.AssertionID })
		automatic = automatic || detached && active && !referenced
	}
	return automatic, errors.Join(rows.Err(), rows.Close())
}

func faceStateCandidateIDs(state PhotoFaceDecisionState) []string {
	ids := make([]string, 0, len(state.Candidates))
	for _, c := range state.Candidates {
		ids = append(ids, c.CandidateID)
	}
	return ids
}

func personIDsJSON(ids []string) string { raw, _ := json.Marshal(ids); return string(raw) }
func changedFaceRows[T any](before, after []T) ([]T, []T) {
	var b, a []T
	for i := range after {
		if !reflect.DeepEqual(before[i], after[i]) {
			b = append(b, before[i])
			a = append(a, after[i])
		}
	}
	return b, a
}
func faceDecisionRowsTx(ctx context.Context, tx *sql.Tx, template PhotoFaceDecisionState) (PhotoFaceDecisionState, error) {
	current := PhotoFaceDecisionState{VersionID: template.VersionID}
	for _, v := range template.Faces {
		f, e := scanPhotoFace(tx.QueryRowContext(ctx, `SELECT `+photoFaceColumns+` FROM photo_faces WHERE face_id=?`, v.FaceID))
		if e != nil {
			return current, e
		}
		current.Faces = append(current.Faces, f)
	}
	for _, v := range template.Assertions {
		a, e := personAssertionByIDTx(ctx, tx, v.AssertionID)
		if e != nil {
			return current, e
		}
		current.Assertions = append(current.Assertions, a)
	}
	for _, v := range template.Candidates {
		c, e := personCandidateByIDTx(ctx, tx, v.CandidateID)
		if e != nil {
			return current, e
		}
		current.Candidates = append(current.Candidates, c)
	}
	for _, v := range template.People {
		p := faceDecisionPerson{PersonID: v.PersonID}
		e := tx.QueryRowContext(ctx, `SELECT COALESCE(key_face_id,''),revision FROM persons WHERE person_id=?`, p.PersonID).Scan(&p.KeyFaceID, &p.Revision)
		if e != nil {
			return current, e
		}
		current.People = append(current.People, p)
	}
	return current, nil
}

func replayFaceDecisionTx(ctx context.Context, tx *sql.Tx, id, requestSHA string) (PhotoFaceDecisionReceipt, bool, error) {
	if id == "" {
		return PhotoFaceDecisionReceipt{}, false, nil
	}
	var before, after []byte
	var operation string
	err := tx.QueryRowContext(ctx, `SELECT operation,before_json,after_json FROM photo_change_receipts WHERE receipt_id=?`, id).Scan(&operation, &before, &after)
	if errors.Is(err, sql.ErrNoRows) {
		return PhotoFaceDecisionReceipt{}, false, nil
	}
	if err != nil {
		return PhotoFaceDecisionReceipt{}, false, err
	}
	if operation != "face_decision" {
		return PhotoFaceDecisionReceipt{}, false, ErrExists
	}
	receipt, err := decodeFaceDecisionReceipt(before, after, id)
	if err != nil {
		return receipt, false, err
	}
	if receipt.RequestSHA256 != requestSHA {
		return receipt, false, ErrExists
	}
	return receipt, true, nil
}
