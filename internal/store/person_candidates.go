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
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/canonical"
)

const (
	personAssertionRoleDepicted    = "depicted"
	personAssertionActionAssert    = "assert"
	personAssertionActionWithdrawn = "withdrawn"
	personCandidateStateOpen       = "open"
	personCandidateStateRejected   = "rejected"
	personCandidateReasonFaceMatch = "face_match"
	maxOpenPersonCandidates        = 10000
	maxPersonCandidateEvidence     = 16384
	maxPersonAssertionNoteBytes    = 1000
)

var ErrPersonCandidateDecided = errors.New("person candidate is no longer open")

type PersonMatchCandidate struct {
	CandidateID, ActorKey, DisplayName, SuggestedPersonID, Reason string
	Evidence                                                      []byte
	EvidenceSHA256                                                string
	Revision                                                      int64
	OccurrenceCount                                               int64
	State, DecidedPersonID, CreatedAt, DecidedAt                  string
}

type CandidateDecision struct {
	CandidateID, Action, PersonID string
	ExpectedRevision              int64
}

type PersonDocumentAssertion struct {
	PhotoMark                                                               bool `json:"PhotoMark,omitzero"`
	AssertionID, ContentVersionID, PersonID, Role, Action, Note, RecordedAt string
	Revision                                                                int64
}

type PersonCandidateOccurrence struct {
	ContentVersionID string `json:"content_version_id"`
	Role             string `json:"role"`
	EvidenceKind     string `json:"evidence_kind"`
	EvidenceID       string `json:"evidence_id"`
}

func normalizedCandidateOccurrences(values []PersonCandidateOccurrence) []PersonCandidateOccurrence {
	values = slices.Clone(values)
	key := func(value PersonCandidateOccurrence) string {
		return strings.Join([]string{value.ContentVersionID, value.Role, value.EvidenceKind, value.EvidenceID}, "\x00")
	}
	slices.SortFunc(values, func(left, right PersonCandidateOccurrence) int {
		return strings.Compare(key(left), key(right))
	})
	return slices.Compact(values)
}

func normalizePersonCandidate(candidate PersonMatchCandidate) (PersonMatchCandidate, error) {
	invalid := func(detail string) (PersonMatchCandidate, error) {
		return PersonMatchCandidate{}, fmt.Errorf("%w: %s", ErrInvalidPerson, detail)
	}
	if candidate.CandidateID != "" && validateUUIDv4(candidate.CandidateID) != nil {
		return invalid("candidate id")
	}
	if !document.ValidPersonCandidateActorKey(candidate.ActorKey) || !document.ValidPersonIdentityText(candidate.ActorKey) ||
		!validPersonName(candidate.DisplayName) ||
		!slices.Contains([]string{"name_only", "identifier_conflict", "external_uid_conflict", "transfer_unresolved", personCandidateReasonFaceMatch}, candidate.Reason) ||
		(candidate.State != "" && candidate.State != personCandidateStateOpen) {
		return invalid("candidate fields")
	}
	if candidate.SuggestedPersonID != "" && validateUUIDv4(candidate.SuggestedPersonID) != nil {
		return invalid("suggested person id")
	}
	if len(candidate.Evidence) == 0 || len(candidate.Evidence) > maxPersonCandidateEvidence {
		return invalid("candidate evidence size")
	}
	var occurrences []PersonCandidateOccurrence
	if err := json.Unmarshal(candidate.Evidence, &occurrences, json.RejectUnknownMembers(true)); err != nil {
		return invalid("candidate evidence JSON")
	}
	occurrences = normalizedCandidateOccurrences(occurrences)
	if len(occurrences) == 0 {
		return invalid("candidate evidence is empty")
	}
	for _, occurrence := range occurrences {
		if (occurrence.Role == personAssertionRoleDepicted) != (candidate.Reason == personCandidateReasonFaceMatch) || candidate.Reason == personCandidateReasonFaceMatch && (occurrence.EvidenceKind != "face" || validateUUIDv4(occurrence.EvidenceID) != nil) || candidate.Reason != personCandidateReasonFaceMatch && occurrence.EvidenceKind == "face" {
			return invalid("depicted occurrence")
		}
		if validateUUIDv4(occurrence.ContentVersionID) != nil ||
			!document.ValidPersonRole(occurrence.Role) ||
			!slices.Contains(document.PersonEvidenceKinds(), document.PersonEvidenceKind(occurrence.EvidenceKind)) ||
			occurrence.EvidenceID == "" || len(occurrence.EvidenceID) > document.MaxPersonEvidenceIDBytes || !document.ValidPersonIdentityText(occurrence.EvidenceID) {
			return invalid("candidate occurrence")
		}
	}
	if candidate.Reason == personCandidateReasonFaceMatch && (!strings.HasPrefix(candidate.ActorKey, "face:") || !slices.ContainsFunc(occurrences, func(o PersonCandidateOccurrence) bool { return "face:"+o.EvidenceID == candidate.ActorKey })) {
		return invalid("face candidate actor")
	}
	raw, err := canonical.Marshal(occurrences)
	if err != nil {
		return PersonMatchCandidate{}, err
	}
	if len(raw) > maxPersonCandidateEvidence {
		return invalid("canonical candidate evidence size")
	}
	digest := sha256.Sum256(raw)
	wantDigest := hex.EncodeToString(digest[:])
	if candidate.EvidenceSHA256 != "" && candidate.EvidenceSHA256 != wantDigest {
		return invalid("candidate evidence digest")
	}
	candidate.Evidence = raw
	candidate.EvidenceSHA256 = wantDigest
	candidate.OccurrenceCount = int64(len(occurrences))
	candidate.Revision = 1
	candidate.State = personCandidateStateOpen
	candidate.DecidedPersonID = ""
	candidate.DecidedAt = ""
	return candidate, nil
}

// OpenPersonCandidate returns the existing or newly retained candidate. The
// boolean is false only when a new evidence set cannot be retained because the
// bounded open queue is full.
func (s *Store) OpenPersonCandidate(
	ctx context.Context, tx *sql.Tx, candidate PersonMatchCandidate,
) (PersonMatchCandidate, bool, error) {
	return s.retainPersonCandidate(ctx, tx, candidate, false)
}
func (s *Store) retainPersonCandidate(ctx context.Context, tx *sql.Tx, candidate PersonMatchCandidate, terminal bool) (PersonMatchCandidate, bool, error) {
	if tx == nil {
		return PersonMatchCandidate{}, false, fmt.Errorf("%w: nil candidate transaction", ErrInvalidPerson)
	}
	candidate, err := normalizePersonCandidate(candidate)
	if err != nil {
		return PersonMatchCandidate{}, false, err
	}
	if candidate.SuggestedPersonID != "" {
		if terminal {
			var exists bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM persons WHERE person_id=?)`, candidate.SuggestedPersonID).Scan(&exists); err != nil {
				return PersonMatchCandidate{}, false, err
			}
			if !exists {
				return PersonMatchCandidate{}, false, ErrNotFound
			}
		} else {
			resolved, err := resolveActiveCandidatePersonTx(ctx, tx, candidate.SuggestedPersonID)
			if err != nil {
				return PersonMatchCandidate{}, false, err
			}
			candidate.SuggestedPersonID = resolved
		}
	}

	existing, err := personCandidateByIdentityTx(ctx, tx, candidate)
	if err == nil {
		return existing, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return PersonMatchCandidate{}, false, err
	}
	var openCount int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_match_candidates WHERE state='open'`).Scan(&openCount); err != nil {
		return PersonMatchCandidate{}, false, err
	}
	if !terminal && openCount >= maxOpenPersonCandidates {
		return PersonMatchCandidate{}, false, nil
	}
	if candidate.CandidateID == "" {
		candidate.CandidateID, err = newUUIDv4()
		if err != nil {
			return PersonMatchCandidate{}, false, err
		}
	}
	candidate.CreatedAt = nowRFC3339()
	_, err = tx.ExecContext(ctx, `INSERT INTO person_match_candidates(
		candidate_id,actor_key,display_name,suggested_person_id,reason,evidence_json,evidence_sha256,
		occurrence_count,revision,state,decided_person_id,created_at,decided_at
	) VALUES(?,?,?,?,?,?,?,?,1,'open',NULL,?,NULL)`,
		candidate.CandidateID, candidate.ActorKey, candidate.DisplayName,
		nullableString(candidate.SuggestedPersonID), candidate.Reason, candidate.Evidence,
		candidate.EvidenceSHA256, candidate.OccurrenceCount, candidate.CreatedAt)
	if s.driver.IsUniqueViolation(err) {
		existing, lookupErr := personCandidateByIdentityTx(ctx, tx, candidate)
		if lookupErr == nil {
			return existing, true, nil
		}
		return PersonMatchCandidate{}, false, errors.Join(err, lookupErr)
	}
	if err != nil {
		return PersonMatchCandidate{}, false, err
	}
	return candidate, true, nil
}

func (s *Store) PersonCandidates(ctx context.Context, state string, limit, offset int) ([]PersonMatchCandidate, int64, error) {
	if state == "" {
		state = personCandidateStateOpen
	}
	if limit == 0 {
		limit = 100
	}
	if !slices.Contains([]string{personCandidateStateOpen, "linked", personCandidateStateRejected, "superseded"}, state) || limit < 1 || limit > 250 || offset < 0 {
		return nil, 0, ErrInvalidPerson
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, fmt.Errorf("starting person candidate page snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	visible, err := s.personCandidateVisibility(ctx, tx)
	if err != nil {
		return nil, 0, err
	}
	var total int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_match_candidates WHERE state=? AND `+visible, state).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT candidate_id,actor_key,display_name,
		COALESCE(suggested_person_id,''),reason,evidence_json,evidence_sha256,revision,occurrence_count,
		state,COALESCE(decided_person_id,''),created_at,COALESCE(decided_at,'')
		FROM person_match_candidates WHERE state=? AND `+visible+` ORDER BY created_at,candidate_id LIMIT ? OFFSET ?`, state, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	candidates := make([]PersonMatchCandidate, 0)
	for rows.Next() {
		candidate, err := scanPersonCandidate(rows)
		if err != nil {
			return nil, 0, err
		}
		candidates = append(candidates, candidate)
	}
	return candidates, total, rows.Err()
}

func (s *Store) OpenPersonCandidateCount(ctx context.Context) (int64, bool, error) {
	var count int64
	var full bool
	err := s.photoReadTx(ctx, func(tx *sql.Tx) error {
		visible, e := s.personCandidateVisibility(ctx, tx)
		if e != nil {
			return e
		}
		var global int64
		e = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN `+visible+` THEN 1 ELSE 0 END),0) FROM person_match_candidates WHERE state='open'`).Scan(&global, &count)
		full = global >= maxOpenPersonCandidates
		return e
	})
	return count, full, err
}

func openPersonCandidateCount(ctx context.Context, q metadataQuerier) (int64, bool, error) {
	var count int64
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_match_candidates WHERE state='open'`).Scan(&count); err != nil {
		return 0, false, err
	}
	return count, count >= maxOpenPersonCandidates, nil
}

func (s *Store) DecidePersonCandidate(ctx context.Context, decision CandidateDecision) (PersonMatchCandidate, error) {
	if validateUUIDv4(decision.CandidateID) != nil || decision.ExpectedRevision < 1 ||
		!slices.Contains([]string{"link", "reject", "new_person"}, decision.Action) ||
		(decision.Action == "link") != (decision.PersonID != "") ||
		(decision.PersonID != "" && validateUUIDv4(decision.PersonID) != nil) {
		return PersonMatchCandidate{}, ErrInvalidPerson
	}
	var decided PersonMatchCandidate
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		candidate, err := personCandidateByIDTx(ctx, tx, decision.CandidateID)
		if err != nil {
			return err
		}
		if candidate.State != personCandidateStateOpen {
			return ErrPersonCandidateDecided
		}
		if candidate.Revision != decision.ExpectedRevision {
			return ErrStaleRevision
		}
		var occurrences []PersonCandidateOccurrence
		if candidate.Reason == personCandidateReasonFaceMatch || decision.Action != "reject" {
			occurrences, err = decodeStoredCandidateEvidence(candidate.Evidence)
			if err != nil {
				return err
			}
		}
		if candidate.Reason == personCandidateReasonFaceMatch {
			activeOccurrences := occurrences[:0]
			for _, occurrence := range occurrences {
				if err := requireCandidateVersionTx(ctx, tx, occurrence.ContentVersionID); errors.Is(err, ErrNotFound) {
					continue
				} else if err != nil {
					return err
				}
				activeOccurrences = append(activeOccurrences, occurrence)
			}
			occurrences = activeOccurrences
			if len(occurrences) == 0 && decision.Action != "reject" {
				return ErrNotFound
			}
			for _, o := range occurrences {
				if e := s.facePhotoQuery(ctx, tx, o.ContentVersionID); e != nil {
					return e
				}
				if e := requireFaceOccurrenceTx(ctx, tx, o); e != nil {
					return e
				}
				if decision.Action == "reject" && candidate.SuggestedPersonID != "" {
					var assigned bool
					if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM photo_faces f JOIN person_document_assertions a ON a.assertion_id=f.assertion_id WHERE f.face_id=? AND f.content_version_id=? AND a.person_id=COALESCE((SELECT surviving_person_id FROM person_aliases WHERE retired_person_id=?),?))`, o.EvidenceID, o.ContentVersionID, candidate.SuggestedPersonID, candidate.SuggestedPersonID).Scan(&assigned); err != nil {
						return err
					}
					if assigned {
						return ErrPersonIdentityConflict
					}
				}
				if _, e := tx.ExecContext(ctx, `UPDATE photo_faces SET revision=revision+1 WHERE face_id=? AND content_version_id=?`, o.EvidenceID, o.ContentVersionID); e != nil {
					return e
				}
			}
		}
		personID := ""
		switch decision.Action {
		case "link":
			personID, err = resolveActiveCandidatePersonTx(ctx, tx, decision.PersonID)
		case "new_person":
			personID, err = createCandidatePersonTx(ctx, tx, candidate.DisplayName)
		}
		if err != nil {
			return err
		}
		if personID != "" {
			err = materializeCandidateAssertionsTx(ctx, tx, personID, occurrences)
			if err != nil {
				return err
			}
		}
		state := personCandidateStateRejected
		if personID != "" {
			state = "linked"
		}
		now := nowRFC3339()
		result, err := tx.ExecContext(ctx, `UPDATE person_match_candidates SET state=?,decided_person_id=?,decided_at=?,revision=revision+1 WHERE candidate_id=? AND revision=? AND state='open'`,
			state, nullableString(personID), now, candidate.CandidateID, candidate.Revision)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return ErrStaleRevision
		}
		if candidate.Reason == personCandidateReasonFaceMatch && personID != "" {
			for _, occurrence := range occurrences {
				before, err := s.faceDecisionStateTx(ctx, tx, occurrence.ContentVersionID, occurrence.EvidenceID, []string{personID}, true)
				if err != nil {
					return err
				}
				if err := s.clearNamedFaceCandidatesTx(ctx, tx, &before, occurrence.EvidenceID, personID, faceDecisionName); err != nil {
					return err
				}
			}
		}
		if candidate.Reason == personCandidateReasonFaceMatch && state == personCandidateStateRejected {
			if err := requirePhotoFaceRejectionBound(ctx, tx, "", "", candidate.CandidateID); err != nil {
				return err
			}
		}
		if err := advancePersonBindingEpochTx(ctx, tx); err != nil {
			return err
		}
		decided, err = personCandidateByIDTx(ctx, tx, candidate.CandidateID)
		return err
	})
	return decided, err
}

func (s *Store) AssertDocumentPerson(ctx context.Context, assertion PersonDocumentAssertion) (PersonDocumentAssertion, error) {
	assertion.PhotoMark = assertion.Role == personAssertionRoleDepicted && assertion.Action == personAssertionActionAssert
	if err := validatePersonAssertion(assertion); err != nil {
		return PersonDocumentAssertion{}, err
	}
	var saved PersonDocumentAssertion
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		if assertion.Role == personAssertionRoleDepicted {
			if err := s.facePhotoQuery(ctx, tx, assertion.ContentVersionID); err != nil {
				return err
			}
		}
		personID, err := resolveActiveCandidatePersonTx(ctx, tx, assertion.PersonID)
		if err != nil {
			return err
		}
		assertion.PersonID = personID
		if err := requireCandidateVersionTx(ctx, tx, assertion.ContentVersionID); err != nil {
			return err
		}
		existing, err := personAssertionByIdentityTx(ctx, tx, assertion.ContentVersionID, assertion.PersonID, assertion.Role)
		switch {
		case err == nil:
			if existing.Action == personAssertionActionWithdrawn && assertion.Action != personAssertionActionWithdrawn {
				if err := requirePhotoAssertionCapacity(ctx, tx, assertion.ContentVersionID, assertion.Role, existing.AssertionID); err != nil {
					return err
				}
			}
			if assertion.AssertionID != "" && assertion.AssertionID != existing.AssertionID {
				return ErrInvalidPerson
			}
			if assertion.Revision != existing.Revision {
				return ErrStaleRevision
			}
			result, err := tx.ExecContext(ctx, `UPDATE person_document_assertions SET action=?,note=?,photo_mark=?,revision=revision+1 WHERE assertion_id=? AND revision=?`,
				assertion.Action, assertion.Note, assertion.Role == personAssertionRoleDepicted && assertion.Action == personAssertionActionAssert, existing.AssertionID, existing.Revision)
			if err != nil {
				return err
			}
			changed, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if changed != 1 {
				return ErrStaleRevision
			}
			assertion.AssertionID = existing.AssertionID
		case errors.Is(err, sql.ErrNoRows):
			if err := requirePhotoAssertionCapacity(ctx, tx, assertion.ContentVersionID, assertion.Role, ""); err != nil {
				return err
			}
			if assertion.Revision != 1 {
				return ErrStaleRevision
			}
			if assertion.AssertionID == "" {
				assertion.AssertionID, err = newUUIDv4()
				if err != nil {
					return err
				}
			}
			assertion.RecordedAt = nowRFC3339()
			_, err = tx.ExecContext(ctx, `INSERT INTO person_document_assertions(assertion_id,content_version_id,person_id,role,action,note,recorded_at,revision) VALUES(?,?,?,?,?,?,?,1)`,
				assertion.AssertionID, assertion.ContentVersionID, assertion.PersonID, assertion.Role,
				assertion.Action, assertion.Note, assertion.RecordedAt)
			if err != nil {
				return err
			}
		default:
			return err
		}
		if assertion.Role == personAssertionRoleDepicted {
			if _, err = tx.ExecContext(ctx, `UPDATE person_document_assertions SET photo_mark=? WHERE assertion_id=?`, assertion.Action == personAssertionActionAssert, assertion.AssertionID); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE photo_faces SET revision=revision+1 WHERE assertion_id=?`, assertion.AssertionID); err != nil {
				return err
			}
			if assertion.Action != personAssertionActionAssert {
				if _, err = tx.ExecContext(ctx, `UPDATE persons SET key_face_id=NULL,revision=revision+1,updated_at=? WHERE key_face_id IN (SELECT face_id FROM photo_faces WHERE assertion_id=?)`, nowRFC3339(), assertion.AssertionID); err != nil {
					return err
				}
			}
		}
		if err := advancePersonBindingEpochTx(ctx, tx); err != nil {
			return err
		}
		saved, err = personAssertionByIDTx(ctx, tx, assertion.AssertionID)
		return err
	})
	return saved, err
}

func (s *Store) DocumentPersonAssertions(ctx context.Context, contentVersionID string) ([]PersonDocumentAssertion, error) {
	if validateUUIDv4(contentVersionID) != nil {
		return nil, ErrInvalidPerson
	}
	rows, err := s.db.QueryContext(ctx, `SELECT assertion_id,content_version_id,person_id,role,action,note,recorded_at,revision,photo_mark FROM person_document_assertions WHERE content_version_id=? ORDER BY role,person_id,assertion_id`, contentVersionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	assertions := make([]PersonDocumentAssertion, 0)
	for rows.Next() {
		assertion, err := scanMarkedPersonAssertion(rows)
		if err != nil {
			return nil, err
		}
		assertions = append(assertions, assertion)
	}
	return assertions, rows.Err()
}

func validatePersonAssertion(assertion PersonDocumentAssertion) error {
	if assertion.PhotoMark && (assertion.Role != personAssertionRoleDepicted || assertion.Action != personAssertionActionAssert) {
		return ErrInvalidPerson
	}
	if assertion.AssertionID != "" && validateUUIDv4(assertion.AssertionID) != nil {
		return ErrInvalidPerson
	}
	if validateUUIDv4(assertion.ContentVersionID) != nil || validateUUIDv4(assertion.PersonID) != nil || assertion.Revision < 1 ||
		!document.ValidPersonRole(assertion.Role) ||
		(!slices.Contains([]string{personAssertionActionAssert, "suppress"}, assertion.Action) && (assertion.Role != personAssertionRoleDepicted || assertion.Action != personAssertionActionWithdrawn)) ||
		len(assertion.Note) > maxPersonAssertionNoteBytes || !utf8.ValidString(assertion.Note) {
		return ErrInvalidPerson
	}
	return nil
}

func resolveActiveCandidatePersonTx(ctx context.Context, tx *sql.Tx, personID string) (string, error) {
	resolved, err := resolvePersonIDTx(ctx, tx, personID)
	if err != nil {
		return "", err
	}
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM persons WHERE person_id=?`, resolved).Scan(&state); err != nil {
		return "", err
	}
	if state == personStateRetired {
		return "", ErrPersonRetired
	}
	return resolved, nil
}

func createCandidatePersonTx(ctx context.Context, tx *sql.Tx, displayName string) (string, error) {
	person, err := createPersonTx(ctx, tx, displayName, "operator")
	return person.PersonID, err
}

func materializeCandidateAssertionsTx(
	ctx context.Context, tx *sql.Tx, personID string, occurrences []PersonCandidateOccurrence,
) error {
	for _, occurrence := range occurrences {
		if err := requireCandidateVersionTx(ctx, tx, occurrence.ContentVersionID); errors.Is(err, ErrNotFound) {
			continue
		} else if err != nil {
			return err
		}
		assertion, _, err := ensurePersonAssertionTx(ctx, tx, occurrence.ContentVersionID, personID, occurrence.Role)
		if err != nil {
			return err
		}
		if err := attachCandidateFaceTx(ctx, tx, occurrence, assertion.AssertionID); err != nil {
			return err
		}
	}
	return nil
}

func requireCandidateVersionTx(ctx context.Context, tx *sql.Tx, versionID string) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM content_versions WHERE version_id=?)`, versionID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	return nil
}

func decodeStoredCandidateEvidence(raw []byte) ([]PersonCandidateOccurrence, error) {
	var occurrences []PersonCandidateOccurrence
	if err := json.Unmarshal(raw, &occurrences, json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("decoding candidate evidence: %w", err)
	}
	if len(occurrences) == 0 {
		return nil, errors.New("decoding candidate evidence: empty occurrence set")
	}
	return occurrences, nil
}

func personCandidateByIdentityTx(ctx context.Context, tx *sql.Tx, candidate PersonMatchCandidate) (PersonMatchCandidate, error) {
	query := `SELECT candidate_id,actor_key,display_name,COALESCE(suggested_person_id,''),reason,
		evidence_json,evidence_sha256,revision,occurrence_count,state,COALESCE(decided_person_id,''),
		created_at,COALESCE(decided_at,'') FROM person_match_candidates
		WHERE actor_key=? AND suggested_person_id IS NULL AND reason=? AND evidence_sha256=?`
	args := []any{candidate.ActorKey, candidate.Reason, candidate.EvidenceSHA256}
	if candidate.SuggestedPersonID != "" {
		query = `SELECT candidate_id,actor_key,display_name,COALESCE(suggested_person_id,''),reason,
			evidence_json,evidence_sha256,revision,occurrence_count,state,COALESCE(decided_person_id,''),
			created_at,COALESCE(decided_at,'') FROM person_match_candidates
			WHERE actor_key=? AND suggested_person_id=? AND reason=? AND evidence_sha256=?`
		args = []any{candidate.ActorKey, candidate.SuggestedPersonID, candidate.Reason, candidate.EvidenceSHA256}
	}
	return scanPersonCandidate(tx.QueryRowContext(ctx, query, args...))
}

func personCandidateByIDTx(ctx context.Context, tx *sql.Tx, candidateID string) (PersonMatchCandidate, error) {
	candidate, err := scanPersonCandidate(tx.QueryRowContext(ctx, `SELECT candidate_id,actor_key,display_name,
		COALESCE(suggested_person_id,''),reason,evidence_json,evidence_sha256,revision,occurrence_count,
		state,COALESCE(decided_person_id,''),created_at,COALESCE(decided_at,'')
		FROM person_match_candidates WHERE candidate_id=?`, candidateID))
	if errors.Is(err, sql.ErrNoRows) {
		return PersonMatchCandidate{}, ErrNotFound
	}
	return candidate, err
}

func scanPersonCandidate(row scanner) (PersonMatchCandidate, error) {
	var candidate PersonMatchCandidate
	err := row.Scan(&candidate.CandidateID, &candidate.ActorKey, &candidate.DisplayName, &candidate.SuggestedPersonID,
		&candidate.Reason, &candidate.Evidence, &candidate.EvidenceSHA256, &candidate.Revision,
		&candidate.OccurrenceCount, &candidate.State, &candidate.DecidedPersonID, &candidate.CreatedAt, &candidate.DecidedAt)
	return candidate, err
}

func personAssertionByIdentityTx(ctx context.Context, tx *sql.Tx, versionID, personID, role string) (PersonDocumentAssertion, error) {
	return scanMarkedPersonAssertion(tx.QueryRowContext(ctx, `SELECT assertion_id,content_version_id,person_id,role,action,note,recorded_at,revision,photo_mark FROM person_document_assertions WHERE content_version_id=? AND person_id=? AND role=?`, versionID, personID, role))
}

func personAssertionByIDTx(ctx context.Context, tx *sql.Tx, assertionID string) (PersonDocumentAssertion, error) {
	return scanMarkedPersonAssertion(tx.QueryRowContext(ctx, `SELECT assertion_id,content_version_id,person_id,role,action,note,recorded_at,revision,photo_mark FROM person_document_assertions WHERE assertion_id=?`, assertionID))
}

// Projection rebuilds also read released schemas without photo ownership metadata.
func scanPersonAssertion(row scanner) (PersonDocumentAssertion, error) {
	var assertion PersonDocumentAssertion
	err := row.Scan(&assertion.AssertionID, &assertion.ContentVersionID, &assertion.PersonID, &assertion.Role, &assertion.Action, &assertion.Note, &assertion.RecordedAt, &assertion.Revision)
	return assertion, err
}

func scanMarkedPersonAssertion(row scanner) (PersonDocumentAssertion, error) {
	var assertion PersonDocumentAssertion
	err := row.Scan(&assertion.AssertionID, &assertion.ContentVersionID, &assertion.PersonID, &assertion.Role,
		&assertion.Action, &assertion.Note, &assertion.RecordedAt, &assertion.Revision, &assertion.PhotoMark)
	return assertion, err
}

func ensurePersonAssertionTx(ctx context.Context, tx *sql.Tx, versionID, personID, role string) (PersonDocumentAssertion, bool, error) {
	existing, err := personAssertionByIdentityTx(ctx, tx, versionID, personID, role)
	if err == nil {
		if existing.Action == personAssertionActionWithdrawn {
			if err := requirePhotoAssertionCapacity(ctx, tx, versionID, role, existing.AssertionID); err != nil {
				return PersonDocumentAssertion{}, false, err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE person_document_assertions SET action='assert',revision=revision+1 WHERE assertion_id=?`, existing.AssertionID); err != nil {
				return PersonDocumentAssertion{}, false, err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE photo_faces SET revision=revision+1 WHERE assertion_id=?`, existing.AssertionID); err != nil {
				return PersonDocumentAssertion{}, false, err
			}
			existing.Action = personAssertionActionAssert
			existing.Revision++
			return existing, true, nil
		} else if existing.Action != personAssertionActionAssert {
			return PersonDocumentAssertion{}, false, ErrPersonIdentityConflict
		}
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return PersonDocumentAssertion{}, false, err
	}
	if err := requirePhotoAssertionCapacity(ctx, tx, versionID, role, ""); err != nil {
		return PersonDocumentAssertion{}, false, err
	}
	id, err := newUUIDv4()
	if err != nil {
		return PersonDocumentAssertion{}, false, err
	}
	assertion := PersonDocumentAssertion{AssertionID: id, ContentVersionID: versionID, PersonID: personID, Role: role, Action: personAssertionActionAssert, Revision: 1, RecordedAt: nowRFC3339()}
	_, err = tx.ExecContext(ctx, `INSERT INTO person_document_assertions(assertion_id,content_version_id,person_id,role,action,note,recorded_at,revision) VALUES(?,?,?,?,'assert','',?,1)`, id, versionID, personID, role, assertion.RecordedAt)
	return assertion, true, err
}

func (s *Store) personCandidateVisibility(ctx context.Context, q metadataQuerier) (string, error) {
	if _, err := s.hiddenSession(ctx, q); err == nil {
		return "1=1", nil
	} else if !errors.Is(err, ErrHiddenLocked) {
		return "", err
	}
	return personCandidateVisibleSQL, nil
}

func requireFaceOccurrenceTx(ctx context.Context, tx *sql.Tx, occurrence PersonCandidateOccurrence) error {
	if occurrence.Role != personAssertionRoleDepicted || validateUUIDv4(occurrence.EvidenceID) != nil {
		return ErrInvalidPerson
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM photo_faces WHERE face_id=? AND content_version_id=?)`, occurrence.EvidenceID, occurrence.ContentVersionID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrInvalidPhotoFace
	}
	return nil
}

func attachCandidateFaceTx(ctx context.Context, tx *sql.Tx, occurrence PersonCandidateOccurrence, assertionID string) error {
	if occurrence.EvidenceKind != "face" {
		return nil
	}
	var existing string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(assertion_id,'') FROM photo_faces WHERE face_id=? AND content_version_id=?`, occurrence.EvidenceID, occurrence.ContentVersionID).Scan(&existing); err != nil {
		return err
	}
	if existing != "" && existing != assertionID {
		return ErrPersonIdentityConflict
	}
	if existing == assertionID {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE photo_faces SET assertion_id=?,revision=revision+1 WHERE face_id=? AND content_version_id=?`, assertionID, occurrence.EvidenceID, occurrence.ContentVersionID)
	return err
}

var personCandidateVisibleSQL = `NOT EXISTS(SELECT 1 FROM person_candidate_faces e WHERE e.candidate_id=person_match_candidates.candidate_id AND NOT (` + personVersionVisibleSQL("e.content_version_id") + `))`
