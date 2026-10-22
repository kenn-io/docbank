package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"math"
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
	AssertionID    string `json:"assertion_id,omitzero" format:"uuid"`
	PersonID       string `json:"person_id,omitzero" format:"uuid"`
	PersonName     string `json:"person_name,omitzero" maxLength:"200"`
	Action         string `json:"action" enum:"name,retract,reject,dismiss,key,withdraw_rejection"`
	Revision       int64  `json:"revision,omitzero" minimum:"0"`
	PersonRevision int64  `json:"person_revision,omitzero"`
}

type faceDecisionPerson struct {
	PersonID                string `json:"person_id"`
	KeyFaceID               string `json:"key_face_id,omitzero"`
	KeyFaceContentVersionID string `json:"key_face_content_version_id,omitzero"`
	Revision                int64  `json:"revision"`
}

type PhotoFaceDecisionState struct {
	VersionID  string                    `json:"content_version_id"`
	Faces      []PhotoFace               `json:"faces"`
	Assertions []PersonDocumentAssertion `json:"assertions"`
	Candidates []PersonMatchCandidate    `json:"candidates"`
	People     []faceDecisionPerson      `json:"people"`
}

type PhotoFaceDecisionReceipt struct {
	CreatedPerson faceDecisionPerson     `json:"created_person,omitzero"`
	RequestSHA256 string                 `json:"request_sha256,omitzero"`
	ReceiptID     string                 `json:"receipt_id"`
	Before        PhotoFaceDecisionState `json:"before"`
	After         PhotoFaceDecisionState `json:"after"`
	UndoOf        string                 `json:"undo_of,omitzero"`
}

type photoFaceReceiptAfter struct {
	CreatedPerson faceDecisionPerson     `json:"created_person,omitzero"`
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
	r.CreatedPerson = state.CreatedPerson
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
	people, err := stringListJSON(personIDs)
	if err != nil {
		return state, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT assertion_id,content_version_id,person_id,role,action,note,recorded_at,revision,photo_mark FROM person_document_assertions WHERE content_version_id=? AND role='depicted' AND person_id IN (SELECT value FROM json_each(?)) ORDER BY assertion_id`, versionID, people)
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
	faceRows, err := tx.QueryContext(ctx, `SELECT `+photoFaceColumns+` FROM photo_faces WHERE content_version_id=? AND face_id<>? AND assertion_id IN (SELECT assertion_id FROM person_document_assertions WHERE content_version_id=? AND role='depicted' AND person_id IN (SELECT value FROM json_each(?))) ORDER BY face_id`, versionID, faceID, versionID, people)
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
	candidateRows, err := tx.QueryContext(ctx, `SELECT c.candidate_id FROM person_candidate_faces e JOIN person_match_candidates c ON c.candidate_id=e.candidate_id WHERE ? AND e.face_id=? AND e.content_version_id=? AND c.state IN ('open','rejected') AND c.reason='face_match' ORDER BY c.candidate_id`, includeCandidates, faceID, versionID)
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
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(p.key_face_id,''),COALESCE(f.content_version_id,''),p.revision FROM persons p LEFT JOIN photo_faces f ON f.face_id=p.key_face_id WHERE p.person_id=?`, id).Scan(&p.KeyFaceID, &p.KeyFaceContentVersionID, &p.Revision); err != nil {
			return state, err
		}
		state.People = append(state.People, p)
	}
	return state, nil
}

func writeFaceDecisionReceiptTx(ctx context.Context, tx *sql.Tx, before, after PhotoFaceDecisionState, createdPerson faceDecisionPerson, undo, operationID, requestSHA string) (PhotoFaceDecisionReceipt, error) {
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
	result := PhotoFaceDecisionReceipt{RequestSHA256: requestSHA, ReceiptID: id, Before: before, After: after, UndoOf: undo, CreatedPerson: createdPerson}
	for _, state := range []PhotoFaceDecisionState{before, after} {
		for _, person := range state.People {
			if person.KeyFaceID != "" && person.KeyFaceContentVersionID == "" {
				return result, ErrInvalidPhotoFace
			}
		}
	}
	if err = validateFaceDecisionReceipt(result, id); err != nil {
		return result, err
	}
	if err = checkFaceDecisionReceiptSize(result); err != nil {
		return result, err
	}
	afterState := photoFaceReceiptAfter{RequestSHA256: requestSHA, ReceiptID: id, After: after, UndoOf: undo, CreatedPerson: createdPerson}
	var assetID string
	if err = tx.QueryRowContext(ctx, `SELECT f.asset_id FROM content_versions v JOIN photo_files f ON f.node_id=v.node_id WHERE v.version_id=?`, before.VersionID).Scan(&assetID); err != nil {
		return result, err
	}
	err = writePhotoReceiptTx(ctx, tx, photoReceipt{ReceiptID: id, Operation: "face_decision", AssetID: assetID, Before: before, After: afterState, BeforeRevision: 0, AfterRevision: 1})
	return result, err
}

// checkFaceDecisionReceiptSize rejects a decision whose inverse would not
// fit the receipt limit, instead of accepting a decision that can never be
// undone. It reserves the largest digit width a revision counter can reach
// and a full receipt reference for undo_of, so the real undo (built the
// same way, with smaller actual values) is guaranteed to fit once this
// reserved estimate does.
func checkFaceDecisionReceiptSize(r PhotoFaceDecisionReceipt) error {
	const id = "00000000-0000-4000-8000-000000000000"
	before, after := reserveFaceDecisionRevisions(r.Before), reserveFaceDecisionRevisions(r.After)
	state := photoFaceReceiptAfter{RequestSHA256: r.RequestSHA256, ReceiptID: id, After: after, UndoOf: id, CreatedPerson: r.CreatedPerson}
	b, err := json.Marshal(before, json.Deterministic(true))
	if err != nil {
		return err
	}
	a, err := json.Marshal(state, json.Deterministic(true))
	if err != nil {
		return err
	}
	if len(b)+len(a) > maxBatchTagReceiptJSONBytes {
		return ErrInvalidPhotoFace
	}
	return nil
}

// reserveFaceDecisionRevisions clones state and maxes every revision field,
// reserving the largest digit width an undo's incremented revisions could add.
func reserveFaceDecisionRevisions(state PhotoFaceDecisionState) PhotoFaceDecisionState {
	state.Faces = slices.Clone(state.Faces)
	for i := range state.Faces {
		state.Faces[i].Revision = math.MaxInt64
	}
	state.Assertions = slices.Clone(state.Assertions)
	for i := range state.Assertions {
		state.Assertions[i].Revision = math.MaxInt64
	}
	state.Candidates = slices.Clone(state.Candidates)
	for i := range state.Candidates {
		state.Candidates[i].Revision = math.MaxInt64
	}
	state.People = slices.Clone(state.People)
	for i := range state.People {
		state.People[i].Revision = math.MaxInt64
	}
	return state
}

// faceCandidateActions are the decisions whose receipts capture the face's
// open and rejected suggestions.
var faceCandidateActions = []string{faceDecisionName, faceDecisionReject, "dismiss", "withdraw_rejection"}

func validateFaceDecisionRequest(request PhotoFaceDecisionRequest) error {
	if request.AssertionID != "" && (validateUUIDv4(request.AssertionID) != nil || request.Action != "retract" || request.FaceID != "") {
		return ErrInvalidPhotoFace
	}
	if request.PersonName != "" && (request.Action != faceDecisionName || request.PersonID != "" || !validPersonName(request.PersonName)) {
		return ErrInvalidPhotoFace
	}
	if request.OperationID != "" && validateUUIDv4(request.OperationID) != nil {
		return ErrInvalidPhotoFace
	}
	if (request.Revision < 1 && (request.Action != "key" || request.FaceID != "")) || (request.FaceID != "" && validateUUIDv4(request.FaceID) != nil) || (request.PersonID != "" && validateUUIDv4(request.PersonID) != nil) {
		return ErrInvalidPhotoFace
	}
	if !slices.Contains([]string{faceDecisionName, "retract", faceDecisionReject, "dismiss", "key", "withdraw_rejection"}, request.Action) {
		return ErrInvalidPhotoFace
	}
	if slices.Contains([]string{"retract", "dismiss"}, request.Action) && request.PersonID != "" || request.Action == "key" && request.PersonRevision < 1 {
		return ErrInvalidPhotoFace
	}
	if request.FaceID == "" && request.AssertionID == "" && request.Action != "key" {
		return ErrInvalidPhotoFace
	}
	if slices.Contains([]string{faceDecisionName, faceDecisionReject, "withdraw_rejection", "key"}, request.Action) && request.PersonID == "" && request.PersonName == "" {
		return ErrInvalidPhotoFace
	}
	return nil
}

func (s *Store) DecidePhotoFace(ctx context.Context, versionID string, request PhotoFaceDecisionRequest) (PhotoFaceDecisionReceipt, error) {
	if err := validateFaceDecisionRequest(request); err != nil {
		return PhotoFaceDecisionReceipt{}, err
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
		replay, found, err := replayFaceDecisionTx(ctx, tx, request.OperationID, requestSHA)
		if err != nil {
			return err
		}
		if found {
			result = replay
			return nil
		}
		result, err = s.decidePhotoFaceTx(ctx, tx, versionID, request, requestSHA)
		return err
	})
	return result, err
}

func (s *Store) decidePhotoFaceTx(ctx context.Context, tx *sql.Tx, versionID string, request PhotoFaceDecisionRequest, requestSHA string) (PhotoFaceDecisionReceipt, error) {
	face, err := decisionFaceTx(ctx, tx, versionID, request)
	if err != nil {
		return PhotoFaceDecisionReceipt{}, err
	}
	var createdPerson faceDecisionPerson
	if request.PersonName != "" {
		person, err := createPersonTx(ctx, tx, request.PersonName, "operator")
		if err != nil {
			return PhotoFaceDecisionReceipt{}, err
		}
		request.PersonID = person.PersonID
		createdPerson = faceDecisionPerson{PersonID: person.PersonID, Revision: person.Revision}
	}
	request.PersonID, err = decisionPersonTx(ctx, tx, request)
	if err != nil {
		return PhotoFaceDecisionReceipt{}, err
	}
	personIDs := []string{}
	if request.PersonID != "" {
		personIDs = append(personIDs, request.PersonID)
	}
	var assigned PersonDocumentAssertion
	assertionID := cmp.Or(request.AssertionID, face.AssertionID)
	if assertionID != "" {
		assigned, err = personAssertionByIDTx(ctx, tx, assertionID)
		if errors.Is(err, sql.ErrNoRows) {
			return PhotoFaceDecisionReceipt{}, ErrNotFound
		}
		if err != nil {
			return PhotoFaceDecisionReceipt{}, err
		}
		personIDs = append(personIDs, assigned.PersonID)
	}
	if request.AssertionID != "" {
		if assigned.Revision != request.Revision {
			return PhotoFaceDecisionReceipt{}, ErrStaleRevision
		}
		if assigned.ContentVersionID != versionID || assigned.Role != "depicted" || !assigned.PhotoMark || assigned.Action != "assert" {
			return PhotoFaceDecisionReceipt{}, ErrInvalidPhotoFace
		}
	}
	stateVersion := versionID
	if request.Action == "key" && request.FaceID == "" {
		stateVersion, err = keyFaceVersionTx(ctx, tx, request.PersonID, versionID)
		if err != nil {
			return PhotoFaceDecisionReceipt{}, err
		}
	}
	withCandidates := slices.Contains(faceCandidateActions, request.Action)
	before, err := s.faceDecisionStateTx(ctx, tx, stateVersion, face.FaceID, personIDs, withCandidates)
	if err != nil {
		return PhotoFaceDecisionReceipt{}, err
	}
	if request.AssertionID != "" {
		_, err = tx.ExecContext(ctx, `UPDATE person_document_assertions SET photo_mark=0,revision=revision+1,action=CASE WHEN EXISTS(SELECT 1 FROM photo_faces f WHERE f.assertion_id=person_document_assertions.assertion_id) THEN action ELSE 'withdrawn' END WHERE assertion_id=?`, request.AssertionID)
	} else if request.Action == "key" {
		err = setKeyFaceTx(ctx, tx, request, assigned)
	} else {
		err = s.applyFaceDecisionTx(ctx, tx, &before, request, face, assigned.PersonID)
	}
	if err != nil {
		return PhotoFaceDecisionReceipt{}, err
	}
	if request.Action == faceDecisionReject {
		if err = requirePhotoFaceRejectionBound(ctx, tx, face.FaceID, versionID, ""); err != nil {
			return PhotoFaceDecisionReceipt{}, err
		}
	}
	if face.FaceID != "" {
		if _, err = tx.ExecContext(ctx, `UPDATE photo_faces SET revision=revision+1 WHERE face_id=? AND content_version_id=?`, face.FaceID, versionID); err != nil {
			return PhotoFaceDecisionReceipt{}, err
		}
	}
	err = advancePersonBindingEpochForVersionTx(ctx, tx, stateVersion)
	if err != nil {
		return PhotoFaceDecisionReceipt{}, err
	}
	after, err := s.faceDecisionStateTx(ctx, tx, stateVersion, face.FaceID, personIDs, withCandidates, faceStateCandidateIDs(before)...)
	if err != nil {
		return PhotoFaceDecisionReceipt{}, err
	}
	return writeFaceDecisionReceiptTx(ctx, tx, before, after, createdPerson, "", request.OperationID, requestSHA)
}

// decisionFaceTx loads the requested face and checks its revision. A key
// decision without a face clears the key and returns the zero face.
func decisionFaceTx(ctx context.Context, tx *sql.Tx, versionID string, request PhotoFaceDecisionRequest) (PhotoFace, error) {
	if request.FaceID == "" {
		return PhotoFace{}, nil
	}
	face, err := scanPhotoFace(tx.QueryRowContext(ctx, `SELECT `+photoFaceColumns+` FROM photo_faces WHERE face_id=? AND content_version_id=?`, request.FaceID, versionID))
	if errors.Is(err, sql.ErrNoRows) {
		return face, ErrNotFound
	}
	if err != nil {
		return face, err
	}
	if face.Revision != request.Revision {
		return face, ErrStaleRevision
	}
	return face, nil
}

// decisionPersonTx resolves the requested person through merges, except
// for withdrawing a rejection, which names the exact rejected person.
func decisionPersonTx(ctx context.Context, tx *sql.Tx, request PhotoFaceDecisionRequest) (string, error) {
	if request.PersonID == "" {
		return "", nil
	}
	if request.Action != "withdraw_rejection" {
		return resolveActiveCandidatePersonTx(ctx, tx, request.PersonID)
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM persons WHERE person_id=?)`, request.PersonID).Scan(&exists); err != nil {
		return "", err
	}
	if !exists {
		return "", ErrNotFound
	}
	return request.PersonID, nil
}

func setKeyFaceTx(ctx context.Context, tx *sql.Tx, request PhotoFaceDecisionRequest, assigned PersonDocumentAssertion) error {
	if err := fencePersonTx(ctx, tx, request.PersonID, request.PersonRevision); err != nil {
		return err
	}
	if request.FaceID != "" && (assigned.PersonID != request.PersonID || assigned.Action != personAssertionActionAssert) {
		return ErrPersonIdentityConflict
	}
	_, err := tx.ExecContext(ctx, `UPDATE persons SET key_face_id=?,revision=revision+1,updated_at=? WHERE person_id=?`, nullableString(request.FaceID), nowRFC3339(), request.PersonID)
	return err
}

func (s *Store) applyFaceDecisionTx(ctx context.Context, tx *sql.Tx, before *PhotoFaceDecisionState, request PhotoFaceDecisionRequest, face PhotoFace, oldPerson string) error {
	retract := request.Action == "retract" || request.Action == "dismiss" || request.Action == faceDecisionName && oldPerson != "" && oldPerson != request.PersonID || (request.Action == faceDecisionReject && oldPerson == request.PersonID)
	if retract && face.AssertionID != "" {
		if err := retractFaceTx(ctx, tx, face); err != nil {
			return err
		}
	}
	switch request.Action {
	case faceDecisionName:
		a, _, err := ensurePersonAssertionTx(ctx, tx, face.ContentVersionID, request.PersonID, personAssertionRoleDepicted)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE photo_faces SET named_at=CASE WHEN assertion_id=? THEN named_at ELSE ? END,assertion_id=? WHERE face_id=? AND content_version_id=?`, a.AssertionID, nowRFC3339(), a.AssertionID, face.FaceID, face.ContentVersionID); err != nil {
			return err
		}
		return s.clearNamedFaceCandidatesTx(ctx, tx, before, face.FaceID, request.PersonID, request.Action)
	case "withdraw_rejection":
		return s.clearNamedFaceCandidatesTx(ctx, tx, before, face.FaceID, request.PersonID, request.Action)
	case faceDecisionReject, "dismiss":
		return s.rejectFaceTx(ctx, tx, before, request, face)
	}
	return nil
}

func retractFaceTx(ctx context.Context, tx *sql.Tx, face PhotoFace) error {
	if _, err := tx.ExecContext(ctx, `UPDATE photo_faces SET assertion_id=NULL,named_at=NULL WHERE face_id=? AND content_version_id=?`, face.FaceID, face.ContentVersionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE persons SET key_face_id=NULL,revision=revision+1,updated_at=? WHERE key_face_id=?`, nowRFC3339(), face.FaceID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE person_document_assertions SET action='withdrawn',revision=revision+1 WHERE assertion_id=? AND photo_mark=0 AND action='assert' AND NOT EXISTS(SELECT 1 FROM photo_faces f WHERE f.assertion_id=person_document_assertions.assertion_id)`, face.AssertionID)
	return err
}

// rejectFaceTx records a rejection of the requested person for the face, or
// a dismissal of the face when no person applies.
func (s *Store) rejectFaceTx(ctx context.Context, tx *sql.Tx, before *PhotoFaceDecisionState, request PhotoFaceDecisionRequest, face PhotoFace) error {
	if err := s.clearNamedFaceCandidatesTx(ctx, tx, before, face.FaceID, request.PersonID, request.Action); err != nil {
		return err
	}
	raw, err := json.Marshal([]PersonCandidateOccurrence{{ContentVersionID: face.ContentVersionID, Role: personAssertionRoleDepicted, EvidenceKind: "face", EvidenceID: face.FaceID}})
	if err != nil {
		return err
	}
	person := request.PersonID
	if request.Action == "dismiss" {
		person = ""
	}
	c, kept, err := s.retainPersonCandidate(ctx, tx, PersonMatchCandidate{ActorKey: "face:" + face.FaceID, DisplayName: "Face", SuggestedPersonID: person, Reason: personCandidateReasonFaceMatch, Evidence: raw}, true)
	if err != nil {
		return err
	}
	if !kept {
		return ErrInvalidPhotoFace
	}
	if c.State != personCandidateStateOpen && !slices.ContainsFunc(before.Candidates, func(old PersonMatchCandidate) bool { return old.CandidateID == c.CandidateID }) {
		before.Candidates = append(before.Candidates, c)
	}
	_, err = tx.ExecContext(ctx, `UPDATE person_match_candidates SET state='rejected',decided_person_id=NULL,decided_at=?,revision=revision+1 WHERE candidate_id=?`, nowRFC3339(), c.CandidateID)
	return err
}

// keyFaceVersionTx returns the content version that holds the person's key
// face, or fallback when the person has none. Clearing a key files its
// receipt there, so undo depends on the key face's own photo rather than
// on whichever photo the request named.
func keyFaceVersionTx(ctx context.Context, tx *sql.Tx, personID, fallback string) (string, error) {
	var versionID string
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(f.content_version_id,'') FROM persons p LEFT JOIN photo_faces f ON f.face_id=p.key_face_id WHERE p.person_id=?`, personID).Scan(&versionID)
	if err != nil || versionID == "" {
		return fallback, err
	}
	return versionID, nil
}

func requirePhotoFaceRejectionBound(ctx context.Context, tx metadataQuerier, faceID, versionID, candidateID string) error {
	var exceeded bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT m.face_id FROM person_candidate_faces m
		JOIN photo_faces f ON f.face_id=m.face_id AND f.content_version_id=m.content_version_id
		JOIN person_match_candidates c ON c.candidate_id=m.candidate_id
		WHERE ((?='' AND ?='' AND ?='') OR (m.face_id=? AND (?='' OR m.content_version_id=?)) OR EXISTS(
			SELECT 1 FROM person_candidate_faces target WHERE target.candidate_id=? AND target.face_id=m.face_id AND target.content_version_id=m.content_version_id
		)) AND c.state='rejected'
		GROUP BY m.face_id,m.content_version_id HAVING COUNT(DISTINCT c.suggested_person_id)>1000
	)`, faceID, versionID, candidateID, faceID, versionID, versionID, candidateID).Scan(&exceeded)
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
	requestSHA, err := requestDigest([]string{"undo", id, operationID})
	if err != nil {
		return PhotoFaceDecisionReceipt{}, err
	}
	var result PhotoFaceDecisionReceipt
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		r, err := s.visibleFaceDecisionTx(ctx, tx, id)
		if err != nil {
			return err
		}
		replay, found, err := replayFaceDecisionTx(ctx, tx, operationID, requestSHA)
		if err != nil {
			return err
		}
		if found {
			result = replay
			return nil
		}
		result, err = undoFaceDecisionTx(ctx, tx, r, operationID, requestSHA)
		return err
	})
	return result, err
}

// visibleFaceDecisionTx loads a face decision receipt whose photo the
// caller can still see.
func (s *Store) visibleFaceDecisionTx(ctx context.Context, tx *sql.Tx, id string) (PhotoFaceDecisionReceipt, error) {
	var raw, beforeRaw []byte
	err := tx.QueryRowContext(ctx, `SELECT before_json,after_json FROM photo_change_receipts WHERE receipt_id=? AND operation='face_decision'`, id).Scan(&beforeRaw, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return PhotoFaceDecisionReceipt{}, ErrNotFound
	}
	if err != nil {
		return PhotoFaceDecisionReceipt{}, err
	}
	r, err := decodeFaceDecisionReceipt(beforeRaw, raw, id)
	if err != nil {
		return r, err
	}
	return r, s.facePhotoQuery(ctx, tx, r.After.VersionID)
}

func undoFaceDecisionTx(ctx context.Context, tx *sql.Tx, r PhotoFaceDecisionReceipt, operationID, requestSHA string) (PhotoFaceDecisionReceipt, error) {
	history := faceWithdrawalHistory{}
	current, err := fenceFaceDecisionUndoTx(ctx, tx, r, history)
	if err != nil {
		return PhotoFaceDecisionReceipt{}, err
	}
	if err = requireUndoKeyFacesTx(ctx, tx, r); err != nil {
		return PhotoFaceDecisionReceipt{}, err
	}
	detached, attached, err := undoFaceLinksTx(ctx, tx, r, &current, history)
	if err != nil {
		return PhotoFaceDecisionReceipt{}, err
	}
	if err = restoreFaceDecisionRowsTx(ctx, tx, r); err != nil {
		return PhotoFaceDecisionReceipt{}, err
	}
	if err = relinkUndoAssertionsTx(ctx, tx, r, detached, attached); err != nil {
		return PhotoFaceDecisionReceipt{}, err
	}
	if r.CreatedPerson.PersonID != "" {
		var unused bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM persons p
			WHERE p.person_id=? AND p.state<>'retired' AND p.revision=?
			AND NOT EXISTS(SELECT 1 FROM person_document_assertions a WHERE a.person_id=p.person_id
				AND NOT (a.role='depicted' AND a.action='withdrawn' AND a.content_version_id=?))
			AND NOT EXISTS(SELECT 1 FROM custodian_assignments c WHERE c.person_id=p.person_id AND c.retired_at IS NULL))`,
			r.CreatedPerson.PersonID, r.CreatedPerson.Revision, r.After.VersionID).Scan(&unused); err != nil {
			return PhotoFaceDecisionReceipt{}, err
		}
		if unused {
			if _, err = retirePersonTx(ctx, tx, r.CreatedPerson.PersonID, r.CreatedPerson.Revision); err != nil {
				return PhotoFaceDecisionReceipt{}, err
			}
		}
	}
	if err = requireFaceDecisionUndoLimitsTx(ctx, tx, r); err != nil {
		return PhotoFaceDecisionReceipt{}, err
	}
	if err = advancePersonBindingEpochForVersionTx(ctx, tx, r.After.VersionID); err != nil {
		return PhotoFaceDecisionReceipt{}, err
	}
	for _, p := range r.After.People {
		if err = validateMergedPhotoFaces(ctx, tx, p.PersonID); err != nil {
			return PhotoFaceDecisionReceipt{}, err
		}
	}
	after, err := faceDecisionRowsTx(ctx, tx, current)
	if err != nil {
		return PhotoFaceDecisionReceipt{}, err
	}
	return writeFaceDecisionReceiptTx(ctx, tx, current, after, faceDecisionPerson{}, r.ReceiptID, operationID, requestSHA)
}

// fenceFaceDecisionUndoTx returns the current rows the receipt touched,
// or ErrStaleRevision when any of them changed after the decision.
func fenceFaceDecisionUndoTx(ctx context.Context, tx *sql.Tx, r PhotoFaceDecisionReceipt, history faceWithdrawalHistory) (PhotoFaceDecisionState, error) {
	current, err := faceDecisionRowsTx(ctx, tx, r.After)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound) {
		return current, ErrStaleRevision
	}
	if err != nil {
		return current, err
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
			automatic, err := history.automatic(ctx, tx, observed)
			if err != nil {
				return current, err
			}
			if !automatic {
				return current, ErrStaleRevision
			}
		} else if observed.Action != personAssertionActionAssert {
			return current, ErrStaleRevision
		}
		observed.Action, observed.Revision = a.Action, a.Revision
		fenced.Assertions[i] = observed
	}
	currentHash, err := requestDigest(fenced)
	if err != nil {
		return current, err
	}
	afterHash, err := requestDigest(r.After)
	if err != nil {
		return current, err
	}
	if currentHash != afterHash {
		return current, ErrStaleRevision
	}
	return current, nil
}

// requireUndoKeyFacesTx checks that every key face the undo restores still
// exists on the photo version it was chosen from.
func requireUndoKeyFacesTx(ctx context.Context, tx *sql.Tx, r PhotoFaceDecisionReceipt) error {
	for i, p := range r.Before.People {
		after := r.After.People[i]
		if p.KeyFaceID == "" || (p.KeyFaceID == after.KeyFaceID && p.KeyFaceContentVersionID == after.KeyFaceContentVersionID) {
			continue
		}
		if p.KeyFaceContentVersionID == "" {
			return ErrStaleRevision
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM photo_faces WHERE face_id=? AND content_version_id=?)`, p.KeyFaceID, p.KeyFaceContentVersionID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrStaleRevision
		}
	}
	return nil
}

// undoFaceLinksTx lists the assertions the undo detaches from and
// reattaches to faces, and adds them to current so the undo receipt
// captures dependencies that older receipts omitted.
func undoFaceLinksTx(ctx context.Context, tx *sql.Tx, r PhotoFaceDecisionReceipt, current *PhotoFaceDecisionState, history faceWithdrawalHistory) ([]string, []string, error) {
	var detached, attached []string
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
			a, err := personAssertionByIDTx(ctx, tx, assertionID)
			if err != nil {
				return nil, nil, err
			}
			if assertionID == old {
				if err = requireReattachableAssertionTx(ctx, tx, r, a, history); err != nil {
					return nil, nil, err
				}
			}
			if !slices.ContainsFunc(current.Assertions, func(v PersonDocumentAssertion) bool { return v.AssertionID == assertionID }) {
				current.Assertions = append(current.Assertions, a)
			}
		}
	}
	slices.SortFunc(current.Assertions, func(a, b PersonDocumentAssertion) int { return cmp.Compare(a.AssertionID, b.AssertionID) })
	return detached, attached, nil
}

// requireReattachableAssertionTx refuses to reattach a face to an assertion
// whose person merged away, that an operator withdrew or suppressed, or
// that changed in a way the receipt does not explain.
func requireReattachableAssertionTx(ctx context.Context, tx *sql.Tx, r PhotoFaceDecisionReceipt, a PersonDocumentAssertion, history faceWithdrawalHistory) error {
	resolved, err := resolveActiveCandidatePersonTx(ctx, tx, a.PersonID)
	if err != nil {
		return err
	}
	wasSuppressed := slices.ContainsFunc(r.Before.Assertions, func(old PersonDocumentAssertion) bool {
		return old.AssertionID == a.AssertionID && old.Action == "suppress"
	})
	if a.Action == personAssertionActionWithdrawn && !slices.ContainsFunc(r.After.Assertions, func(after PersonDocumentAssertion) bool { return after.AssertionID == a.AssertionID }) {
		automatic, err := history.automatic(ctx, tx, a)
		if err != nil {
			return err
		}
		if !automatic {
			return ErrStaleRevision
		}
	}
	if resolved != a.PersonID || a.Action == "suppress" && !wasSuppressed {
		return ErrStaleRevision
	}
	return nil
}

// restoreFaceDecisionRowsTx writes back every row whose revision the
// decision advanced.
func restoreFaceDecisionRowsTx(ctx context.Context, tx *sql.Tx, r PhotoFaceDecisionReceipt) error {
	for i, f := range r.Before.Faces {
		if f.Revision == r.After.Faces[i].Revision {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE photo_faces SET named_at=CASE WHEN assertion_id IS ? THEN named_at ELSE NULL END,assertion_id=?,revision=revision+1 WHERE face_id=? AND content_version_id=?`, nullableString(f.AssertionID), nullableString(f.AssertionID), f.FaceID, r.After.VersionID); err != nil {
			return err
		}
	}
	if err := restoreFaceDecisionAssertionsTx(ctx, tx, r); err != nil {
		return err
	}
	for i, c := range r.Before.Candidates {
		if c.Revision == r.After.Candidates[i].Revision {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE person_match_candidates SET state=?,decided_person_id=?,decided_at=?,revision=revision+1 WHERE candidate_id=?`, c.State, nullableString(c.DecidedPersonID), nullableString(c.DecidedAt), c.CandidateID); err != nil {
			return err
		}
	}
	for i, p := range r.Before.People {
		if p.Revision == r.After.People[i].Revision {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE persons SET key_face_id=?,revision=revision+1,updated_at=? WHERE person_id=?`, nullableString(p.KeyFaceID), nowRFC3339(), p.PersonID); err != nil {
			return err
		}
	}
	return nil
}

func restoreFaceDecisionAssertionsTx(ctx context.Context, tx *sql.Tx, r PhotoFaceDecisionReceipt) error {
	var faceIDs []string
	for _, face := range r.After.Faces {
		faceIDs = append(faceIDs, face.FaceID)
	}
	faces, err := stringListJSON(faceIDs)
	if err != nil {
		return err
	}
	for i, a := range r.Before.Assertions {
		if a.Revision == r.After.Assertions[i].Revision {
			continue
		}
		if a.PhotoMark && !r.After.Assertions[i].PhotoMark {
			resolved, err := resolveActiveCandidatePersonTx(ctx, tx, a.PersonID)
			if err != nil {
				return err
			}
			if resolved != a.PersonID {
				return ErrStaleRevision
			}
		}
		if a.Action == personAssertionActionWithdrawn && a.Revision > 0 {
			var shared bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM photo_faces WHERE assertion_id=? AND face_id NOT IN (SELECT value FROM json_each(?)))`, a.AssertionID, faces).Scan(&shared); err != nil {
				return err
			}
			if shared {
				return ErrStaleRevision
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE person_document_assertions SET action=CASE WHEN ?='assert' AND ?=0 AND NOT EXISTS(SELECT 1 FROM photo_faces f WHERE f.assertion_id=person_document_assertions.assertion_id) THEN 'withdrawn' WHEN ?=0 AND ?='withdrawn' AND EXISTS(SELECT 1 FROM photo_faces f WHERE f.assertion_id=person_document_assertions.assertion_id) THEN 'assert' ELSE ? END,photo_mark=?,revision=revision+1 WHERE assertion_id=?`, a.Action, a.PhotoMark, a.Revision, a.Action, a.Action, a.PhotoMark, a.AssertionID); err != nil {
			return err
		}
	}
	return nil
}

func relinkUndoAssertionsTx(ctx context.Context, tx *sql.Tx, r PhotoFaceDecisionReceipt, detached, attached []string) error {
	for _, assertionID := range detached {
		if _, err := tx.ExecContext(ctx, `UPDATE person_document_assertions SET action='withdrawn',revision=revision+1 WHERE assertion_id=? AND photo_mark=0 AND action='assert' AND NOT EXISTS(SELECT 1 FROM photo_faces WHERE assertion_id=?)`, assertionID, assertionID); err != nil {
			return err
		}
	}
	for _, assertionID := range attached {
		if slices.ContainsFunc(r.Before.Assertions, func(a PersonDocumentAssertion) bool {
			return a.AssertionID == assertionID && a.Revision > 0 && a.Action == personAssertionActionWithdrawn
		}) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE person_document_assertions SET action='assert',revision=revision+1 WHERE assertion_id=? AND action='withdrawn'`, assertionID); err != nil {
			return err
		}
	}
	return nil
}

func requireFaceDecisionUndoLimitsTx(ctx context.Context, tx *sql.Tx, r PhotoFaceDecisionReceipt) error {
	count, err := photoAssertionCount(ctx, tx, r.After.VersionID, "")
	if err != nil {
		return err
	}
	if count > 1000 {
		return ErrPhotoFaceLimit
	}
	if len(r.Before.Candidates) == 0 {
		return nil
	}
	var openCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_match_candidates WHERE state='open'`).Scan(&openCount); err != nil {
		return err
	}
	if openCount > maxOpenPersonCandidates {
		return ErrInvalidPhotoFace
	}
	return nil
}

func validateFaceDecisionReceipt(r PhotoFaceDecisionReceipt, id string) error {
	if r.CreatedPerson != (faceDecisionPerson{}) && (validateUUIDv4(r.CreatedPerson.PersonID) != nil || r.CreatedPerson.Revision < 1 || r.UndoOf != "") {
		return ErrInvalidPhotoFace
	}
	if r.CreatedPerson.PersonID != "" {
		if !slices.ContainsFunc(r.After.Assertions, func(a PersonDocumentAssertion) bool {
			return a.PersonID == r.CreatedPerson.PersonID && a.ContentVersionID == r.After.VersionID && a.Role == personAssertionRoleDepicted && a.Action == personAssertionActionAssert
		}) || slices.ContainsFunc(r.After.People, func(p faceDecisionPerson) bool {
			return p.PersonID == r.CreatedPerson.PersonID
		}) {
			return ErrInvalidPhotoFace
		}
	}
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
		if b.PhotoMark != a.PhotoMark {
			// Clear and its inverses change one existing assertion only.
			if len(r.After.Assertions) != 1 || len(r.After.Faces) != 0 || len(r.After.Candidates) != 0 || len(r.After.People) != 0 || b.Revision < 1 || a.Revision != b.Revision+1 {
				return ErrInvalidPhotoFace
			}
			if b.PhotoMark {
				if b.Action != personAssertionActionAssert || a.Action != personAssertionActionAssert && a.Action != personAssertionActionWithdrawn {
					return ErrInvalidPhotoFace
				}
			} else if r.UndoOf == "" || a.Action != personAssertionActionAssert || b.Action != personAssertionActionAssert && b.Action != personAssertionActionWithdrawn {
				return ErrInvalidPhotoFace
			}
		}
		if b.Revision < 0 || a.Revision < b.Revision {
			return ErrInvalidPhotoFace
		}
		if b.Revision == 0 {
			if b.Action != personAssertionActionWithdrawn {
				return ErrInvalidPhotoFace
			}
			b.Revision = 1
		}
		if validatePersonAssertion(a) != nil || validatePersonAssertion(b) != nil || a.Role != personAssertionRoleDepicted || b.Role != a.Role || b.AssertionID != a.AssertionID || a.ContentVersionID != r.After.VersionID || b.ContentVersionID != a.ContentVersionID || b.PersonID != a.PersonID || b.Note != a.Note || b.RecordedAt != a.RecordedAt || i > 0 && r.After.Assertions[i-1].AssertionID >= a.AssertionID {
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
		if validateUUIDv4(a.PersonID) != nil || a.PersonID != b.PersonID || a.Revision < b.Revision || b.Revision < 1 ||
			a.KeyFaceID != "" && validateUUIDv4(a.KeyFaceID) != nil || b.KeyFaceID != "" && validateUUIDv4(b.KeyFaceID) != nil ||
			a.KeyFaceContentVersionID != "" && validateUUIDv4(a.KeyFaceContentVersionID) != nil ||
			b.KeyFaceContentVersionID != "" && validateUUIDv4(b.KeyFaceContentVersionID) != nil ||
			a.KeyFaceID == "" && a.KeyFaceContentVersionID != "" || b.KeyFaceID == "" && b.KeyFaceContentVersionID != "" ||
			i > 0 && r.After.People[i-1].PersonID >= a.PersonID {
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
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT f.face_id,COALESCE(c.suggested_person_id,'') FROM person_match_candidates c JOIN person_candidate_faces e ON e.candidate_id=c.candidate_id JOIN photo_faces f ON f.face_id=e.face_id AND f.content_version_id=e.content_version_id WHERE `+rejectedFaceMatchPredicate+` AND f.content_version_id=? ORDER BY 1,2`, faces[0].ContentVersionID)
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
		// Dismissing or naming the face settles every open suggestion for it.
		settlesOpen := (action == "dismiss" || action == faceDecisionName) && c.State == personCandidateStateOpen
		if c.State != personCandidateStateOpen && c.State != personCandidateStateRejected || !settlesOpen && c.SuggestedPersonID != "" && c.SuggestedPersonID != personID {
			continue
		}
		occurrences, err := decodeStoredCandidateEvidence(c.Evidence)
		if err != nil {
			return err
		}
		kept := slices.DeleteFunc(slices.Clone(occurrences), func(o PersonCandidateOccurrence) bool {
			return o.EvidenceKind == "face" && o.EvidenceID == faceID && o.ContentVersionID == before.VersionID
		})
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

// faceWithdrawalHistory decodes each content version's face decision
// receipts at most once per undo. Undo writes its own receipt only after
// every fence check, so the cached receipts stay current for the whole check.
type faceWithdrawalHistory map[string][]PhotoFaceDecisionReceipt

// automatic reports whether an earlier face decision withdrew the assertion
// as a side effect of detaching its last face, rather than an operator
// withdrawing it directly. A whole-photo clear detaches no face, so it never
// counts.
func (h faceWithdrawalHistory) automatic(ctx context.Context, tx *sql.Tx, assertion PersonDocumentAssertion) (bool, error) {
	receipts, ok := h[assertion.ContentVersionID]
	if !ok {
		var err error
		receipts, err = faceDecisionReceiptsTx(ctx, tx, assertion.ContentVersionID)
		if err != nil {
			return false, err
		}
		h[assertion.ContentVersionID] = receipts
	}
	for _, receipt := range receipts {
		detached := slices.ContainsFunc(receipt.After.Assertions, func(a PersonDocumentAssertion) bool {
			return a.AssertionID == assertion.AssertionID && a.Revision == assertion.Revision && a.Action == personAssertionActionWithdrawn
		})
		active := slices.ContainsFunc(receipt.Before.Assertions, func(a PersonDocumentAssertion) bool {
			return a.AssertionID == assertion.AssertionID && a.Action == personAssertionActionAssert
		})
		linkedBefore := slices.ContainsFunc(receipt.Before.Faces, func(f PhotoFace) bool { return f.AssertionID == assertion.AssertionID })
		referenced := slices.ContainsFunc(receipt.After.Faces, func(f PhotoFace) bool { return f.AssertionID == assertion.AssertionID })
		if detached && active && linkedBefore && !referenced {
			return true, nil
		}
	}
	return false, nil
}

func faceDecisionReceiptsTx(ctx context.Context, tx *sql.Tx, versionID string) ([]PhotoFaceDecisionReceipt, error) {
	rows, err := tx.QueryContext(ctx, `SELECT receipt_id,before_json,after_json FROM photo_change_receipts INDEXED BY photo_change_receipts_face_version WHERE operation='face_decision' AND json_extract(before_json,'$.content_version_id')=?`, versionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var receipts []PhotoFaceDecisionReceipt
	for rows.Next() {
		var id string
		var before, after []byte
		if err := rows.Scan(&id, &before, &after); err != nil {
			return nil, err
		}
		receipt, err := decodeFaceDecisionReceipt(before, after, id)
		if err != nil {
			return nil, err
		}
		if receipt.Before.VersionID == versionID {
			receipts = append(receipts, receipt)
		}
	}
	return receipts, errors.Join(rows.Err(), rows.Close())
}

func faceStateCandidateIDs(state PhotoFaceDecisionState) []string {
	ids := make([]string, 0, len(state.Candidates))
	for _, c := range state.Candidates {
		ids = append(ids, c.CandidateID)
	}
	return ids
}

func stringListJSON(ids []string) (string, error) {
	raw, err := json.Marshal(ids)
	return string(raw), err
}

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
		f, e := scanPhotoFace(tx.QueryRowContext(ctx, `SELECT `+photoFaceColumns+` FROM photo_faces WHERE face_id=? AND content_version_id=?`, v.FaceID, template.VersionID))
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
		e := tx.QueryRowContext(ctx, `SELECT COALESCE(p.key_face_id,''),COALESCE(f.content_version_id,''),p.revision FROM persons p LEFT JOIN photo_faces f ON f.face_id=p.key_face_id WHERE p.person_id=?`, p.PersonID).Scan(&p.KeyFaceID, &p.KeyFaceContentVersionID, &p.Revision)
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
