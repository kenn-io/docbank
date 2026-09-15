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

type PersonMergeMoved struct {
	IdentityIDs            []string                          `json:"identity_ids"`
	DeduplicatedIdentities []PersonMergeDeduplicatedIdentity `json:"deduplicated_identities"`
	ExternalUIDs           []PersonMergeExternalUID          `json:"external_uids"`
	AssignmentIDs          []string                          `json:"assignment_ids"`
	AssertionIDs           []string                          `json:"assertion_ids"`
	SupersededCandidate    []string                          `json:"superseded_candidate_ids"`
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

type PersonMergeExternalUID struct {
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
	MovedIdentityIDs                         []string
	CreatedAt                                string
}

type PersonSplitRequest struct {
	PersonID, OperationID, DisplayName       string
	Revision                                 int64
	IdentityIDs, AssignmentIDs, AssertionIDs []string
	External                                 []PersonExternalIdentity
}

func validatePersonSplitRequest(request PersonSplitRequest) error {
	if validateUUIDv4(request.PersonID) != nil || validateUUIDv4(request.OperationID) != nil || request.Revision < 1 ||
		len(request.IdentityIDs)+len(request.AssignmentIDs)+len(request.AssertionIDs)+len(request.External) == 0 {
		return errors.New("split requires fenced explicit membership")
	}
	for _, ids := range [][]string{request.IdentityIDs, request.AssignmentIDs, request.AssertionIDs} {
		seen := map[string]bool{}
		for _, id := range ids {
			if validateUUIDv4(id) != nil || seen[id] {
				return errors.New("invalid or repeated split identity")
			}
			seen[id] = true
		}
	}
	seenExternal := map[string]bool{}
	for _, identity := range request.External {
		key := identity.System + "\x00" + identity.ArchiveID + "\x00" + identity.UID
		if !validExternalTuple(identity.System, identity.ArchiveID, identity.UID) || seenExternal[key] {
			return errors.New("invalid or repeated split external identity")
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
		return PersonMergeReceipt{}, ErrPersonMergeConflict
	}
	requestHash, err := requestDigest([]any{survivorID, absorbedID, operationID, survivorRevision, absorbedRevision})
	if err != nil {
		return PersonMergeReceipt{}, err
	}
	var receipt PersonMergeReceipt
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		replayed, found, err := personMergeReceiptTx(ctx, tx, operationID, requestHash)
		if err != nil {
			return err
		}
		if found {
			receipt = replayed
			return nil
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
		moved, err := collectPersonMergeMovedTx(ctx, tx, survivorID, absorbedID)
		if err != nil {
			return err
		}
		if err := validateMergeCollisionsTx(ctx, tx, survivorID, absorbedID); err != nil {
			return err
		}
		retirements, err := mergeExternalRetirementsTx(ctx, tx, survivorID, absorbedID)
		if err != nil {
			return err
		}
		if err := validatePersonMergeBoundsTx(ctx, tx, survivorID, absorbedID); err != nil {
			return err
		}
		movedRaw, err := canonical.Marshal(moved)
		if err != nil {
			return err
		}
		if len(movedRaw) > document.MaxPersonMergeMovedBytes {
			return ErrPersonMergeConflict
		}
		for _, retirement := range retirements {
			if _, err := tx.ExecContext(ctx, `UPDATE person_external_identities SET uid_state='retired',updated_at=? WHERE person_id=? AND system=? AND archive_id=? AND uid=?`, nowRFC3339(), retirement.personID, retirement.system, retirement.archiveID, retirement.uid); err != nil {
				return err
			}
		}
		if err := markDocumentPeopleDirtyForPerson(ctx, tx, absorbedID, "person_merged"); err != nil {
			return err
		}
		if err := markDocumentPeopleDirtyForPerson(ctx, tx, survivorID, "person_merged"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM person_identities WHERE person_id=? AND EXISTS(SELECT 1 FROM person_identities keep WHERE keep.person_id=? AND keep.kind=person_identities.kind AND keep.value_normalized=person_identities.value_normalized AND (person_identities.kind='name_alias' OR (keep.scope_kind=person_identities.scope_kind AND keep.scope_value=person_identities.scope_value)))`, absorbedID, survivorID); err != nil {
			return err
		}
		for _, table := range []string{"person_identities", "person_external_identities", "custodian_assignments", "person_document_assertions"} {
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET person_id=? WHERE person_id=?`, table), survivorID, absorbedID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE person_aliases SET surviving_person_id=? WHERE surviving_person_id=?`, survivorID, absorbedID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE person_match_candidates SET state='superseded' WHERE suggested_person_id=? AND state='open'`, absorbedID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM persons WHERE person_id=?`, absorbedID); err != nil {
			return err
		}
		now := nowRFC3339()
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

func personMergeReceiptTx(ctx context.Context, tx *sql.Tx, operationID, requestHash string) (PersonMergeReceipt, bool, error) {
	var receipt PersonMergeReceipt
	var movedRaw []byte
	err := tx.QueryRowContext(ctx, `SELECT merge_id,operation_id,request_sha256,survivor_person_id,absorbed_person_id,absorbed_display_name,moved_json,survivor_revision_before,survivor_revision_after,created_at FROM person_merges WHERE operation_id=?`, operationID).Scan(&receipt.MergeID, &receipt.OperationID, &receipt.RequestSHA256, &receipt.SurvivorPersonID, &receipt.AbsorbedPersonID, &receipt.AbsorbedDisplayName, &movedRaw, &receipt.SurvivorRevisionBefore, &receipt.SurvivorRevisionAfter, &receipt.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PersonMergeReceipt{}, false, nil
	}
	if err != nil {
		return PersonMergeReceipt{}, false, err
	}
	if receipt.RequestSHA256 != requestHash {
		return PersonMergeReceipt{}, false, ErrPersonMergeConflict
	}
	if err := json.Unmarshal(movedRaw, &receipt.Moved, json.RejectUnknownMembers(true)); err != nil {
		return PersonMergeReceipt{}, false, err
	}
	return receipt, true, nil
}

func collectPersonMergeMovedTx(ctx context.Context, tx *sql.Tx, survivorID, absorbedID string) (PersonMergeMoved, error) {
	moved := PersonMergeMoved{
		IdentityIDs: []string{}, DeduplicatedIdentities: []PersonMergeDeduplicatedIdentity{},
		ExternalUIDs: []PersonMergeExternalUID{}, AssignmentIDs: []string{}, AssertionIDs: []string{}, SupersededCandidate: []string{},
	}
	queries := []struct {
		query string
		dst   *[]string
	}{
		{`SELECT identity_id FROM person_identities WHERE person_id=? ORDER BY identity_id`, &moved.IdentityIDs},
		{`SELECT assignment_id FROM custodian_assignments WHERE person_id=? ORDER BY assignment_id`, &moved.AssignmentIDs},
		{`SELECT assertion_id FROM person_document_assertions WHERE person_id=? ORDER BY assertion_id`, &moved.AssertionIDs},
		{`SELECT candidate_id FROM person_match_candidates WHERE suggested_person_id=? AND state='open' ORDER BY candidate_id`, &moved.SupersededCandidate},
	}
	for _, item := range queries {
		values, err := collectPersonMergeColumnTx(ctx, tx, item.query, absorbedID)
		if err != nil {
			return PersonMergeMoved{}, err
		}
		*item.dst = append(*item.dst, values...)
	}
	var err error
	moved.ExternalUIDs, err = collectPersonMergeExternalUIDsTx(ctx, tx, absorbedID)
	if err != nil {
		return PersonMergeMoved{}, err
	}
	moved.DeduplicatedIdentities, err = collectPersonMergeDeduplicatedIdentitiesTx(ctx, tx, survivorID, absorbedID)
	if err != nil {
		return PersonMergeMoved{}, err
	}
	return moved, nil
}

func collectPersonMergeExternalUIDsTx(ctx context.Context, tx *sql.Tx, personID string) (_ []PersonMergeExternalUID, retErr error) {
	rows, err := tx.QueryContext(ctx, `SELECT system,archive_id,uid FROM person_external_identities WHERE person_id=? ORDER BY system,archive_id,uid`, personID)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	values := []PersonMergeExternalUID{}
	for rows.Next() {
		var value PersonMergeExternalUID
		if err := rows.Scan(&value.System, &value.ArchiveID, &value.UID); err != nil {
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
		return ErrPersonMergeConflict
	}
	return nil
}

func validateMergeCollisionsTx(ctx context.Context, tx *sql.Tx, survivorID, absorbedID string) error {
	var collisions int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_document_assertions a JOIN person_document_assertions s ON s.person_id=? AND a.person_id=? AND s.content_version_id=a.content_version_id AND s.role=a.role`, survivorID, absorbedID).Scan(&collisions); err != nil {
		return err
	}
	if collisions > 0 {
		return ErrPersonMergeConflict
	}
	return nil
}

type mergeExternalRetirement struct {
	personID, system, archiveID, uid string
}

func mergeExternalRetirementsTx(ctx context.Context, tx *sql.Tx, survivorID, absorbedID string) (_ []mergeExternalRetirement, retErr error) {
	rows, err := tx.QueryContext(ctx, `SELECT s.system,s.archive_id,s.uid,a.uid FROM person_external_identities s JOIN person_external_identities a ON a.system=s.system AND a.archive_id=s.archive_id WHERE s.person_id=? AND a.person_id=? AND s.uid_state='current' AND a.uid_state='current'`, survivorID, absorbedID)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	retirements := []mergeExternalRetirement{}
	for rows.Next() {
		var system, archiveID, survivorUID, absorbedUID string
		if err := rows.Scan(&system, &archiveID, &survivorUID, &absorbedUID); err != nil {
			return nil, err
		}
		var target string
		err := tx.QueryRowContext(ctx, `SELECT surviving_uid FROM person_external_uid_aliases WHERE system=? AND archive_id=? AND retired_uid=?`, system, archiveID, absorbedUID).Scan(&target)
		if err == nil && target == survivorUID {
			retirements = append(retirements, mergeExternalRetirement{absorbedID, system, archiveID, absorbedUID})
			continue
		}
		err = tx.QueryRowContext(ctx, `SELECT surviving_uid FROM person_external_uid_aliases WHERE system=? AND archive_id=? AND retired_uid=?`, system, archiveID, survivorUID).Scan(&target)
		if err == nil && target == absorbedUID {
			retirements = append(retirements, mergeExternalRetirement{survivorID, system, archiveID, survivorUID})
			continue
		}
		return nil, ErrPersonMergeConflict
	}
	return retirements, rows.Err()
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
		if err == nil {
			if storedHash != requestHash {
				return ErrPersonMergeConflict
			}
			return json.Unmarshal(raw, &receipt, json.RejectUnknownMembers(true))
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
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
				if _, err := tx.ExecContext(ctx, query, newID, request.PersonID, id); err != nil {
					return err
				}
			}
		}
		for _, external := range request.External {
			if _, err := tx.ExecContext(ctx, `UPDATE person_external_identities SET person_id=?,updated_at=? WHERE person_id=? AND system=? AND archive_id=? AND uid=?`, newID, now, request.PersonID, external.System, external.ArchiveID, external.UID); err != nil {
				return err
			}
		}
		if err := markDocumentPeopleDirtyForPerson(ctx, tx, request.PersonID, "person_split"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE persons SET revision=revision+1,updated_at=? WHERE person_id=?`, now, request.PersonID); err != nil {
			return err
		}
		if err := markDocumentPeopleDirtyForPerson(ctx, tx, newID, "person_split"); err != nil {
			return err
		}
		receipt = PersonSplitReceipt{OperationID: request.OperationID, SourcePersonID: request.PersonID, NewPersonID: newID, MovedIdentityIDs: slices.Clone(request.IdentityIDs), CreatedAt: now}
		raw, err = canonical.Marshal(receipt)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO person_splits(operation_id,request_sha256,receipt_json,created_at) VALUES(?,?,?,?)`, request.OperationID, requestHash, raw, now)
		return err
	})
	return receipt, err
}
