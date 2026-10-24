package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/canonical"
)

var ErrPersonMergeConflict = errors.New("person merge conflict")

// ErrPersonMergeTooLarge reports a merge whose survivor would exceed a person
// bound or whose moved records exceed the merge receipt limit.
var ErrPersonMergeTooLarge = errors.New("person merge too large")

type PersonMergeMoved struct {
	FaceCandidates         []PersonMergeFaceCandidate         `json:"face_candidates,omitzero"`
	DeduplicatedAssertions []PersonMergeDeduplicatedAssertion `json:"deduplicated_assertions"`
	AssertionIDs           []string                           `json:"assertion_ids"`
	SupersededCandidates   []string                           `json:"superseded_candidate_ids"`
	IdentityIDs            []string                           `json:"identity_ids"`
	DeduplicatedIdentities []PersonMergeDeduplicatedIdentity  `json:"deduplicated_identities"`
	ExternalUIDs           []PersonExternalUID                `json:"external_uids"`
	AssignmentIDs          []string                           `json:"assignment_ids"`
}

type PersonMergeDeduplicatedAssertion struct {
	Assertion           PersonDocumentAssertion `json:"assertion"`
	RetainedAssertionID string                  `json:"retained_assertion_id"`
}

type PersonMergeDeduplicatedIdentity struct {
	IdentityID         string `json:"identity_id"`
	Kind               string `json:"kind"`
	ValueNormalized    string `json:"value_normalized"`
	ValueDisplay       string `json:"value_display"`
	ScopeKind          string `json:"scope_kind"`
	ScopeValue         string `json:"scope_value"`
	Normalization      string `json:"normalization"`
	Origin             string `json:"origin"`
	EvidenceKind       string `json:"evidence_kind"`
	EvidenceID         string `json:"evidence_id"`
	Confidence         string `json:"confidence"`
	RecordedAt         string `json:"recorded_at"`
	RetainedIdentityID string `json:"retained_identity_id"`
}

type PersonExternalUID struct {
	System    string `json:"system"`
	ArchiveID string `json:"archive_id"`
	UID       string `json:"uid"`
}

type PersonMergeReceipt struct {
	MergeID, OperationID, RequestSHA256           string
	SurvivorPersonID, AbsorbedPersonID            string
	AbsorbedDisplayName                           string
	Moved                                         PersonMergeMoved
	SurvivorRevisionBefore, SurvivorRevisionAfter int64
	CreatedAt                                     string
}

type PersonSplitReceipt struct {
	OperationID, SourcePersonID, NewPersonID string
	// SourceRevisionAfter is derived from the fenced request so stored receipts keep their shape.
	SourceRevisionAfter int64    `json:"-"`
	MovedAssertionIDs   []string `json:"MovedAssertionIDs,omitzero"`
	MovedIdentityIDs    []string
	CreatedAt           string
}

type PersonSplitRequest struct {
	PersonID, OperationID, DisplayName string
	Revision                           int64
	IdentityIDs, AssignmentIDs         []string
	AssertionIDs                       []string `json:"AssertionIDs,omitzero"`
	External                           []PersonExternalUID
}

func validatePersonSplitRequest(request PersonSplitRequest) error {
	if validateUUIDv4(request.PersonID) != nil || validateUUIDv4(request.OperationID) != nil || request.Revision < 1 ||
		len(request.AssertionIDs) > 1000 || len(request.IdentityIDs)+len(request.AssignmentIDs)+len(request.External)+len(request.AssertionIDs) == 0 {
		return fmt.Errorf("%w: split requires fenced explicit membership", ErrInvalidPerson)
	}
	for _, ids := range [][]string{request.IdentityIDs, request.AssignmentIDs, request.AssertionIDs} {
		seen := map[string]bool{}
		for _, id := range ids {
			if validateUUIDv4(id) != nil || seen[id] {
				return fmt.Errorf("%w: invalid or repeated split identity", ErrInvalidPerson)
			}
			seen[id] = true
		}
	}
	seenExternal := map[string]bool{}
	for _, identity := range request.External {
		key := identity.System + "\x00" + identity.ArchiveID + "\x00" + identity.UID
		if !validExternalTuple(identity.System, identity.ArchiveID, identity.UID) || seenExternal[key] {
			return fmt.Errorf("%w: invalid or repeated split external identity", ErrInvalidPerson)
		}
		seenExternal[key] = true
	}
	return nil
}

func requestDigest(value any) (string, error) {
	raw, err := canonical.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (s *Store) MergePersons(ctx context.Context, survivorID, absorbedID, operationID string, survivorRevision, absorbedRevision int64) (PersonMergeReceipt, error) {
	if survivorID == absorbedID || validateUUIDv4(survivorID) != nil || validateUUIDv4(absorbedID) != nil || validateUUIDv4(operationID) != nil || survivorRevision < 1 || absorbedRevision < 1 {
		return PersonMergeReceipt{}, fmt.Errorf("%w: invalid person merge identity or revision", ErrInvalidPerson)
	}
	requestHash, err := requestDigest([]any{survivorID, absorbedID, operationID, survivorRevision, absorbedRevision})
	if err != nil {
		return PersonMergeReceipt{}, err
	}
	var receipt PersonMergeReceipt
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		stored, err := personMergeReceiptTx(ctx, tx, operationID)
		replayed, found, err := replayReceipt(stored, err, stored.RequestSHA256 == requestHash, ErrPersonMergeConflict)
		if err != nil {
			return err
		}
		if found {
			if err := s.fenceBrowserMergeReplayTx(ctx, tx, replayed); err != nil {
				return err
			}
			receipt = replayed
			return nil
		}
		if err := s.fenceVisiblePeopleTx(ctx, tx, survivorID, absorbedID); err != nil {
			return err
		}
		if err := s.fenceBrowserMergeHiddenTx(ctx, tx, survivorID, absorbedID); err != nil {
			return err
		}
		if err := fencePersonTx(ctx, tx, survivorID, survivorRevision); err != nil {
			return err
		}
		if err := fencePersonTx(ctx, tx, absorbedID, absorbedRevision); err != nil {
			return err
		}
		var absorbedName string
		if err := tx.QueryRowContext(ctx, `SELECT display_name FROM persons WHERE person_id=?`, absorbedID).Scan(&absorbedName); err != nil {
			return err
		}
		if err := s.checkFaceMergeConflictsTx(ctx, tx, survivorID, absorbedID); err != nil {
			return err
		}
		if err := validatePersonMergeBoundsTx(ctx, tx, survivorID, absorbedID); err != nil {
			return err
		}
		moved, err := collectPersonMergeMovedTx(ctx, tx, survivorID, absorbedID)
		if err != nil {
			return err
		}
		moved.FaceCandidates, err = moveFaceCandidatesTx(ctx, tx, survivorID, absorbedID)
		if err != nil {
			return err
		}
		movedRaw, err := canonical.Marshal(moved)
		if err != nil {
			return err
		}
		if len(movedRaw) > document.MaxPersonMergeMovedBytes {
			return ErrPersonMergeTooLarge
		}
		if err := advancePersonBindingEpochTx(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM person_identities WHERE person_id=? AND EXISTS(SELECT 1 FROM person_identities keep WHERE keep.person_id=? AND keep.kind=person_identities.kind AND keep.value_normalized=person_identities.value_normalized AND (person_identities.kind='name_alias' OR (keep.scope_kind=person_identities.scope_kind AND keep.scope_value=person_identities.scope_value)))`, absorbedID, survivorID); err != nil {
			return err
		}
		now := nowRFC3339()
		if _, err := tx.ExecContext(ctx, `UPDATE person_identities SET person_id=? WHERE person_id=?`, survivorID, absorbedID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE person_external_identities SET person_id=?,updated_at=? WHERE person_id=?`, survivorID, now, absorbedID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE custodian_assignments SET person_id=?,revision=revision+1 WHERE person_id=?`, survivorID, absorbedID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, personMergeFaceRevisionSQL, survivorID, absorbedID, survivorID, absorbedID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE person_document_assertions SET action=COALESCE((SELECT other.action FROM person_document_assertions other WHERE other.person_id IN (?,?) AND other.person_id<>person_document_assertions.person_id AND other.content_version_id=person_document_assertions.content_version_id AND other.role=person_document_assertions.role AND other.action<>'withdrawn'),action),revision=revision+1 WHERE person_id IN (?,?) AND action='withdrawn'`, survivorID, absorbedID, survivorID, absorbedID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE persons SET key_face_id=COALESCE(key_face_id,(SELECT key_face_id FROM persons WHERE person_id=?)) WHERE person_id=?`, absorbedID, survivorID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE photo_faces SET assertion_id=(SELECT keep.assertion_id FROM person_document_assertions old JOIN person_document_assertions keep ON keep.person_id=? AND keep.content_version_id=old.content_version_id AND keep.role=old.role AND keep.action=old.action WHERE old.assertion_id=photo_faces.assertion_id) WHERE assertion_id IN (SELECT old.assertion_id FROM person_document_assertions old JOIN person_document_assertions keep ON keep.person_id=? AND keep.content_version_id=old.content_version_id AND keep.role=old.role AND keep.action=old.action WHERE old.person_id=?)`, survivorID, survivorID, absorbedID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE person_document_assertions SET photo_mark=1,revision=revision+1 WHERE person_id=? AND photo_mark=0 AND EXISTS(SELECT 1 FROM person_document_assertions other WHERE other.person_id=? AND other.content_version_id=person_document_assertions.content_version_id AND other.role=person_document_assertions.role AND other.photo_mark=1)`, survivorID, absorbedID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM person_document_assertions WHERE person_id=? AND EXISTS(
			SELECT 1 FROM person_document_assertions keep WHERE keep.person_id=?
			AND keep.content_version_id=person_document_assertions.content_version_id
			AND keep.role=person_document_assertions.role AND keep.action=person_document_assertions.action
		)`, absorbedID, survivorID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE person_document_assertions SET person_id=?,revision=revision+1 WHERE person_id=?`, survivorID, absorbedID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE person_match_candidates SET decided_person_id=?,revision=revision+1 WHERE decided_person_id=?`, survivorID, absorbedID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE person_match_candidates SET state='superseded',revision=revision+1 WHERE suggested_person_id=? AND state='open'`, absorbedID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE person_aliases SET surviving_person_id=? WHERE surviving_person_id=?`, survivorID, absorbedID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM persons WHERE person_id=?`, absorbedID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO person_aliases(retired_person_id,surviving_person_id,reason,retired_at) VALUES(?,?,?,?)`, absorbedID, survivorID, "merged", now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE persons SET revision=revision+1,updated_at=? WHERE person_id=?`, now, survivorID); err != nil {
			return err
		}
		mergeID, err := newUUIDv4()
		if err != nil {
			return err
		}
		receipt = PersonMergeReceipt{MergeID: mergeID, OperationID: operationID, RequestSHA256: requestHash,
			SurvivorPersonID: survivorID, AbsorbedPersonID: absorbedID, AbsorbedDisplayName: absorbedName,
			Moved: moved, SurvivorRevisionBefore: survivorRevision, SurvivorRevisionAfter: survivorRevision + 1, CreatedAt: now}
		_, err = tx.ExecContext(ctx, `INSERT INTO person_merges(merge_id,operation_id,request_sha256,survivor_person_id,absorbed_person_id,absorbed_display_name,moved_json,survivor_revision_before,survivor_revision_after,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, mergeID, operationID, requestHash, survivorID, absorbedID, absorbedName, movedRaw, survivorRevision, survivorRevision+1, now)
		return err
	})
	return receipt, err
}

func (s *Store) fenceBrowserMergeHiddenTx(ctx context.Context, tx *sql.Tx, survivorID, absorbedID string) error {
	if visible, _ := ctx.Value(visiblePersonContextKey{}).(bool); !visible {
		return nil
	}
	if _, err := s.hiddenSession(ctx, tx); !errors.Is(err, ErrHiddenLocked) {
		return err
	}
	var hidden bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM (
		SELECT content_version_id FROM person_document_assertions WHERE person_id IN (?,?)
		UNION
		SELECT e.content_version_id FROM person_candidate_faces e JOIN person_match_candidates c ON c.candidate_id=e.candidate_id WHERE c.suggested_person_id IN (?,?)
	) affected WHERE NOT `+personVersionVisibleSQL("affected.content_version_id")+`)`, survivorID, absorbedID, survivorID, absorbedID).Scan(&hidden); err != nil {
		return err
	}
	if hidden {
		return fmt.Errorf("%w: unlock Hidden and retry the merge", ErrHiddenLocked)
	}
	return nil
}

// A replay may outlive its browser's Hidden unlock and the absorbed person.
func (s *Store) fenceBrowserMergeReplayTx(ctx context.Context, tx *sql.Tx, receipt PersonMergeReceipt) error {
	if visible, _ := ctx.Value(visiblePersonContextKey{}).(bool); !visible {
		return nil
	}
	survivorID, err := resolvePersonIDTx(ctx, tx, receipt.SurvivorPersonID)
	if err != nil {
		return err
	}
	if err := s.fenceVisiblePeopleTx(ctx, tx, survivorID); err != nil {
		return err
	}
	if _, err := s.hiddenSession(ctx, tx); !errors.Is(err, ErrHiddenLocked) {
		return err
	}
	versions := make([]string, 0, len(receipt.Moved.DeduplicatedAssertions))
	for _, removed := range receipt.Moved.DeduplicatedAssertions {
		versions = append(versions, removed.Assertion.ContentVersionID)
	}
	ids, err := json.Marshal(receipt.Moved.AssertionIDs)
	if err != nil {
		return err
	}
	versionIDs, err := json.Marshal(versions)
	if err != nil {
		return err
	}
	candidates := make([]string, 0, 2*len(receipt.Moved.FaceCandidates)+len(receipt.Moved.SupersededCandidates))
	for _, moved := range receipt.Moved.FaceCandidates {
		candidates = append(candidates, moved.CandidateID, moved.RetainedCandidateID)
	}
	candidates = append(candidates, receipt.Moved.SupersededCandidates...)
	candidateIDs, err := json.Marshal(candidates)
	if err != nil {
		return err
	}
	var hidden bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM (
		SELECT content_version_id FROM person_document_assertions WHERE assertion_id IN (SELECT value FROM json_each(?))
		UNION SELECT value FROM json_each(?)
		UNION SELECT e.content_version_id FROM person_candidate_faces e JOIN person_match_candidates c ON c.candidate_id=e.candidate_id WHERE c.candidate_id IN (SELECT value FROM json_each(?))
	) moved WHERE NOT `+personVersionVisibleSQL("moved.content_version_id")+`)`, string(ids), string(versionIDs), string(candidateIDs)).Scan(&hidden); err != nil {
		return err
	}
	if hidden {
		return fmt.Errorf("%w: unlock Hidden and retry the merge", ErrHiddenLocked)
	}
	return nil
}

func personMergeReceiptTx(ctx context.Context, tx *sql.Tx, operationID string) (PersonMergeReceipt, error) {
	var receipt PersonMergeReceipt
	var movedRaw []byte
	err := tx.QueryRowContext(ctx, `SELECT merge_id,operation_id,request_sha256,survivor_person_id,absorbed_person_id,absorbed_display_name,moved_json,survivor_revision_before,survivor_revision_after,created_at FROM person_merges WHERE operation_id=?`, operationID).Scan(&receipt.MergeID, &receipt.OperationID, &receipt.RequestSHA256, &receipt.SurvivorPersonID, &receipt.AbsorbedPersonID, &receipt.AbsorbedDisplayName, &movedRaw, &receipt.SurvivorRevisionBefore, &receipt.SurvivorRevisionAfter, &receipt.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PersonMergeReceipt{}, ErrNotFound
	}
	if err != nil {
		return PersonMergeReceipt{}, err
	}
	if err := json.Unmarshal(movedRaw, &receipt.Moved, json.RejectUnknownMembers(true)); err != nil {
		return PersonMergeReceipt{}, err
	}
	return receipt, nil
}

func collectPersonMergeMovedTx(ctx context.Context, tx *sql.Tx, survivorID, absorbedID string) (PersonMergeMoved, error) {
	moved := PersonMergeMoved{
		IdentityIDs: []string{}, DeduplicatedIdentities: []PersonMergeDeduplicatedIdentity{},
		ExternalUIDs: []PersonExternalUID{}, AssignmentIDs: []string{},
		AssertionIDs: []string{}, SupersededCandidates: []string{},
	}
	queries := []struct {
		query string
		dst   *[]string
	}{
		{`SELECT identity_id FROM person_identities WHERE person_id=? ORDER BY identity_id`, &moved.IdentityIDs},
		{`SELECT assignment_id FROM custodian_assignments WHERE person_id=? ORDER BY assignment_id`, &moved.AssignmentIDs},
		{`SELECT assertion_id FROM person_document_assertions WHERE person_id=? ORDER BY assertion_id`, &moved.AssertionIDs},
		{`SELECT candidate_id FROM person_match_candidates WHERE suggested_person_id=? AND state='open' ORDER BY candidate_id`, &moved.SupersededCandidates},
	}
	for _, item := range queries {
		values, err := collectPersonMergeColumnTx(ctx, tx, item.query, absorbedID)
		if err != nil {
			return PersonMergeMoved{}, err
		}
		*item.dst = append(*item.dst, values...)
	}
	var err error
	moved.ExternalUIDs, err = collectPersonExternalUIDsTx(ctx, tx, absorbedID)
	if err != nil {
		return PersonMergeMoved{}, err
	}
	moved.DeduplicatedAssertions, err = collectPersonMergeDeduplicatedAssertionsTx(ctx, tx, survivorID, absorbedID)
	if err != nil {
		return PersonMergeMoved{}, err
	}
	moved.DeduplicatedIdentities, err = collectPersonMergeDeduplicatedIdentitiesTx(ctx, tx, survivorID, absorbedID)
	if err != nil {
		return PersonMergeMoved{}, err
	}
	return moved, nil
}

func collectPersonExternalUIDsTx(ctx context.Context, tx *sql.Tx, personID string) (_ []PersonExternalUID, retErr error) {
	rows, err := tx.QueryContext(ctx, `SELECT system,archive_id,uid FROM person_external_identities WHERE person_id=? ORDER BY system,archive_id,uid`, personID)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	values := []PersonExternalUID{}
	for rows.Next() {
		var value PersonExternalUID
		if err := rows.Scan(&value.System, &value.ArchiveID, &value.UID); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func collectPersonMergeDeduplicatedAssertionsTx(ctx context.Context, tx *sql.Tx, survivorID, absorbedID string) (_ []PersonMergeDeduplicatedAssertion, retErr error) {
	rows, err := tx.QueryContext(ctx, `SELECT a.assertion_id,a.content_version_id,a.person_id,a.role,a.action,a.note,a.recorded_at,a.revision,a.photo_mark,k.assertion_id
		FROM person_document_assertions a JOIN person_document_assertions k
		ON k.person_id=? AND k.content_version_id=a.content_version_id AND k.role=a.role AND (k.action=a.action OR k.action='withdrawn' OR a.action='withdrawn')
		WHERE a.person_id=? ORDER BY a.assertion_id`, survivorID, absorbedID)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	values := []PersonMergeDeduplicatedAssertion{}
	for rows.Next() {
		var value PersonMergeDeduplicatedAssertion
		a := &value.Assertion
		if err := rows.Scan(&a.AssertionID, &a.ContentVersionID, &a.PersonID, &a.Role, &a.Action, &a.Note, &a.RecordedAt, &a.Revision, &a.PhotoMark, &value.RetainedAssertionID); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func collectPersonMergeDeduplicatedIdentitiesTx(ctx context.Context, tx *sql.Tx, survivorID, absorbedID string) (_ []PersonMergeDeduplicatedIdentity, retErr error) {
	rows, err := tx.QueryContext(ctx, `SELECT a.identity_id,a.kind,a.value_normalized,a.value_display,
		a.scope_kind,a.scope_value,a.normalization,a.origin,a.evidence_kind,a.evidence_id,a.confidence,a.recorded_at,k.identity_id
		FROM person_identities a JOIN person_identities k
		  ON k.person_id=? AND k.kind=a.kind AND k.value_normalized=a.value_normalized
		 AND (a.kind='name_alias' OR (k.scope_kind=a.scope_kind AND k.scope_value=a.scope_value))
		WHERE a.person_id=? ORDER BY a.identity_id,k.identity_id`, survivorID, absorbedID)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	values := []PersonMergeDeduplicatedIdentity{}
	for rows.Next() {
		var value PersonMergeDeduplicatedIdentity
		if err := rows.Scan(&value.IdentityID, &value.Kind, &value.ValueNormalized, &value.ValueDisplay,
			&value.ScopeKind, &value.ScopeValue, &value.Normalization, &value.Origin, &value.EvidenceKind,
			&value.EvidenceID, &value.Confidence, &value.RecordedAt, &value.RetainedIdentityID); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func collectPersonMergeColumnTx(ctx context.Context, tx *sql.Tx, query, personID string) (_ []string, retErr error) {
	rows, err := tx.QueryContext(ctx, query, personID)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	values := []string{}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func validatePersonMergeBoundsTx(ctx context.Context, tx *sql.Tx, survivorID, absorbedID string) error {
	var identities, external int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM (
		SELECT kind,CASE WHEN kind='name_alias' THEN '' ELSE scope_kind END AS normalized_scope_kind,
		CASE WHEN kind='name_alias' THEN '' ELSE scope_value END AS normalized_scope_value,value_normalized
		FROM person_identities WHERE person_id IN (?,?)
		GROUP BY kind,normalized_scope_kind,normalized_scope_value,value_normalized)`, survivorID, absorbedID).Scan(&identities); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_external_identities WHERE person_id IN (?,?)`, survivorID, absorbedID).Scan(&external); err != nil {
		return err
	}
	if identities > document.MaxPersonIdentitiesPerPerson || external > document.MaxPersonExternalIdentities {
		return ErrPersonMergeTooLarge
	}
	// Each moved ID appears in the receipt as a quoted JSON string, so the sum
	// of their lengths plus quotes is a lower bound on the receipt size. Reject
	// before collecting IDs that cannot fit.
	var movedBytes int
	if err := tx.QueryRowContext(ctx, `SELECT
		COALESCE((SELECT SUM(length(identity_id)+2) FROM person_identities WHERE person_id=?),0)
		+ COALESCE((SELECT SUM(length(assignment_id)+2) FROM custodian_assignments WHERE person_id=?),0)
		+ COALESCE((SELECT SUM(length(assertion_id)+2) FROM person_document_assertions WHERE person_id=?),0)
		+ COALESCE((SELECT SUM(length(candidate_id)+2) FROM person_match_candidates
			WHERE suggested_person_id=? AND (state='open' OR reason='face_match')),0)`,
		absorbedID, absorbedID, absorbedID, absorbedID).Scan(&movedBytes); err != nil {
		return err
	}
	if movedBytes > document.MaxPersonMergeMovedBytes {
		return ErrPersonMergeTooLarge
	}
	var conflict bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM person_document_assertions a
		JOIN person_document_assertions s ON s.person_id=? AND a.person_id=?
		AND s.content_version_id=a.content_version_id AND s.role=a.role AND s.action<>a.action AND s.action<>'withdrawn' AND a.action<>'withdrawn')`, survivorID, absorbedID).Scan(&conflict); err != nil {
		return err
	}
	if conflict {
		return ErrPersonMergeConflict
	}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM person_external_identities
		WHERE person_id IN (?,?) AND uid_state='current' GROUP BY system,archive_id HAVING COUNT(*)>1)`, survivorID, absorbedID).Scan(&conflict); err != nil {
		return err
	}
	if conflict {
		return ErrPersonMergeConflict
	}
	return nil
}

func (s *Store) SplitPerson(ctx context.Context, request PersonSplitRequest) (PersonSplitReceipt, error) {
	if err := validatePersonSplitRequest(request); err != nil || !validPersonName(request.DisplayName) {
		if err == nil {
			err = ErrInvalidPerson
		}
		return PersonSplitReceipt{}, err
	}
	requestHash, err := requestDigest(request)
	if err != nil {
		return PersonSplitReceipt{}, err
	}
	var receipt PersonSplitReceipt
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		var storedHash string
		var raw []byte
		err := tx.QueryRowContext(ctx, `SELECT request_sha256,receipt_json FROM person_splits WHERE operation_id=?`, request.OperationID).Scan(&storedHash, &raw)
		raw, found, err := replayReceipt(raw, err, storedHash == requestHash, ErrPersonMergeConflict)
		if err != nil {
			return err
		}
		if found {
			if err := json.Unmarshal(raw, &receipt, json.RejectUnknownMembers(true)); err != nil {
				return err
			}
			receipt.SourceRevisionAfter = request.Revision + 1
			return nil
		}
		if err := fencePersonTx(ctx, tx, request.PersonID, request.Revision); err != nil {
			return err
		}
		for _, selection := range []struct {
			table, column string
			ids           []string
		}{
			{"person_identities", "identity_id", request.IdentityIDs},
			{"custodian_assignments", "assignment_id", request.AssignmentIDs},
			{"person_document_assertions", "assertion_id", request.AssertionIDs},
		} {
			for _, id := range selection.ids {
				if selection.table == "person_document_assertions" {
					a, err := personAssertionByIDTx(ctx, tx, id)
					if errors.Is(err, sql.ErrNoRows) {
						return ErrPersonMergeConflict
					}
					if err != nil {
						return err
					}
					if a.Role == "depicted" {
						if err = s.facePhotoQuery(ctx, tx, a.ContentVersionID); err != nil {
							return err
						}
					}
				}
				var exists bool
				query := fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %s WHERE person_id=? AND %s=?)`, selection.table, selection.column)
				if err := tx.QueryRowContext(ctx, query, request.PersonID, id).Scan(&exists); err != nil || !exists {
					if err != nil {
						return err
					}
					return ErrPersonMergeConflict
				}
			}
		}
		for _, external := range request.External {
			var exists bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM person_external_identities WHERE person_id=? AND system=? AND archive_id=? AND uid=?)`, request.PersonID, external.System, external.ArchiveID, external.UID).Scan(&exists); err != nil || !exists {
				if err != nil {
					return err
				}
				return ErrPersonMergeConflict
			}
		}
		newID, err := newUUIDv4()
		if err != nil {
			return err
		}
		now := nowRFC3339()
		if _, err := tx.ExecContext(ctx, `INSERT INTO persons(person_id,display_name,display_name_folded,origin,state,created_at,updated_at) VALUES(?,?,?,'operator','curated',?,?)`, newID, request.DisplayName, document.FoldPersonName(request.DisplayName), now, now); err != nil {
			return err
		}
		for _, selection := range []struct {
			table, column string
			ids           []string
		}{
			{"person_identities", "identity_id", request.IdentityIDs},
			{"custodian_assignments", "assignment_id", request.AssignmentIDs},
			{"person_document_assertions", "assertion_id", request.AssertionIDs},
		} {
			for _, id := range selection.ids {
				query := fmt.Sprintf(`UPDATE %s SET person_id=? WHERE person_id=? AND %s=?`, selection.table, selection.column)
				if selection.table == "person_document_assertions" {
					query = `UPDATE person_document_assertions SET person_id=?,revision=revision+1 WHERE person_id=? AND assertion_id=?`
					if _, err := tx.ExecContext(ctx, `UPDATE photo_faces SET revision=revision+1 WHERE assertion_id=?`, id); err != nil {
						return err
					}
				}
				if selection.table == "custodian_assignments" {
					query = `UPDATE custodian_assignments SET person_id=?,revision=revision+1 WHERE person_id=? AND assignment_id=?`
				}
				if _, err := tx.ExecContext(ctx, query, newID, request.PersonID, id); err != nil {
					return err
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE persons SET key_face_id=(SELECT p.key_face_id FROM persons p JOIN photo_faces f ON f.face_id=p.key_face_id JOIN person_document_assertions a ON a.assertion_id=f.assertion_id WHERE p.person_id=? AND a.person_id=?) WHERE person_id=?`, request.PersonID, newID, newID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE persons SET key_face_id=NULL WHERE person_id=? AND key_face_id=(SELECT key_face_id FROM persons WHERE person_id=?)`, request.PersonID, newID); err != nil {
			return err
		}
		for _, external := range request.External {
			if _, err := tx.ExecContext(ctx, `UPDATE person_external_identities SET person_id=?,updated_at=? WHERE person_id=? AND system=? AND archive_id=? AND uid=?`, newID, now, request.PersonID, external.System, external.ArchiveID, external.UID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE persons SET revision=revision+1,updated_at=? WHERE person_id=?`, now, request.PersonID); err != nil {
			return err
		}
		if err := advancePersonBindingEpochTx(ctx, tx); err != nil {
			return err
		}
		receipt = PersonSplitReceipt{OperationID: request.OperationID, SourcePersonID: request.PersonID, NewPersonID: newID, SourceRevisionAfter: request.Revision + 1, MovedIdentityIDs: slices.Clone(request.IdentityIDs), MovedAssertionIDs: slices.Clone(request.AssertionIDs), CreatedAt: now}
		raw, err = canonical.Marshal(receipt)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO person_splits(operation_id,request_sha256,receipt_json,created_at) VALUES(?,?,?,?)`, request.OperationID, requestHash, raw, now)
		return err
	})
	return receipt, err
}

func validateMergedPhotoFaces(ctx context.Context, tx *sql.Tx, id string) error {
	return validatePersonKeyFaces(ctx, tx, id, ErrPersonIdentityConflict)
}

// UNION deduplicates face-version pairs referenced by assignments and candidates.
const personMergeFaceRevisionSQL = `UPDATE photo_faces SET revision=revision+1 WHERE EXISTS (
 SELECT 1 FROM (
  SELECT face_id,content_version_id FROM photo_faces WHERE assertion_id IN (SELECT assertion_id FROM person_document_assertions WHERE person_id IN (?,?))
  UNION
  SELECT e.face_id,e.content_version_id FROM person_candidate_faces e JOIN person_match_candidates c ON c.candidate_id=e.candidate_id WHERE c.suggested_person_id IN (?,?)
 ) affected WHERE affected.face_id=photo_faces.face_id AND affected.content_version_id=photo_faces.content_version_id
)`
