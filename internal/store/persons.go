package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
)

var (
	ErrPersonIdentityConflict = errors.New("person identity conflict")
	ErrPersonRetired          = errors.New("person is retired")
	ErrInvalidPerson          = errors.New("invalid person")
)

type Person struct {
	PersonID, DisplayName, DisplayNameFolded, Origin, State string
	Revision                                                int64
	CreatedAt, UpdatedAt                                    string
}

type PersonIdentity struct {
	IdentityID, PersonID, Kind, ValueNormalized, ValueDisplay, Normalization string
	Origin, EvidenceKind, EvidenceID, Confidence, RecordedAt                 string
	ScopeKind, ScopeValue                                                    string
}

func validPersonName(name string) bool {
	return utf8.ValidString(name) && strings.TrimSpace(name) != "" && len(name) <= document.MaxPersonDisplayNameBytes
}

func validPersonOrigin(origin string) bool {
	return slices.Contains([]string{"operator", "derived", "transfer"}, origin)
}

func validPersonConfidence(confidence string) bool {
	return slices.Contains([]string{"exact_identifier", "operator_asserted", "supplied_identity", "name_candidate"}, confidence)
}

func (s *Store) CreatePerson(ctx context.Context, displayName, origin string) (Person, error) {
	if !validPersonName(displayName) || !validPersonOrigin(origin) {
		return Person{}, ErrInvalidPerson
	}
	state := "provisional"
	if origin == "operator" {
		state = "curated"
	}
	id, err := newUUIDv4()
	if err != nil {
		return Person{}, err
	}
	now := nowRFC3339()
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO persons(person_id,display_name,display_name_folded,origin,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, id, displayName, document.FoldPersonName(displayName), origin, state, now, now); err != nil {
			return err
		}
		return advancePersonBindingEpochTx(ctx, tx)
	})
	if err != nil {
		return Person{}, err
	}
	person, _, err := s.PersonByID(ctx, id)
	return person, err
}

func (s *Store) UpdatePerson(ctx context.Context, id string, revision int64, name string) (Person, error) {
	if !validPersonName(name) {
		return Person{}, ErrInvalidPerson
	}
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		if err := fencePersonTx(ctx, tx, id, revision); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE persons SET display_name=?,display_name_folded=?,revision=revision+1,state='curated',updated_at=? WHERE person_id=? AND revision=? AND state<>'retired'`, name, document.FoldPersonName(name), nowRFC3339(), id, revision)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrStaleRevision
		}
		return advancePersonBindingEpochTx(ctx, tx)
	})
	if err != nil {
		return Person{}, err
	}
	person, _, err := s.PersonByID(ctx, id)
	return person, err
}

// RetirePerson keeps document assertions as historical operator decisions.
// They remain readable, but the retired person cannot receive new assertions.
func (s *Store) RetirePerson(ctx context.Context, id string, revision int64) (Person, error) {
	var retired Person
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		if err := fencePersonTx(ctx, tx, id, revision); err != nil {
			return err
		}
		now := nowRFC3339()
		result, err := tx.ExecContext(ctx, `UPDATE persons SET state='retired',revision=revision+1,updated_at=? WHERE person_id=? AND revision=? AND state<>'retired'`, now, id, revision)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrStaleRevision
		}
		if _, err := tx.ExecContext(ctx, `UPDATE person_aliases SET surviving_person_id=NULL,reason='deleted',retired_at=? WHERE surviving_person_id=?`, now, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO person_aliases(retired_person_id,surviving_person_id,reason,retired_at) VALUES(?,NULL,'deleted',?)`, id, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE custodian_assignments SET person_id=NULL,revision=revision+1 WHERE person_id=? AND retired_at IS NULL`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE person_match_candidates SET state='superseded',revision=revision+1 WHERE suggested_person_id=? AND state='open'`, id); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT person_id,display_name,display_name_folded,origin,state,revision,created_at,updated_at FROM persons WHERE person_id=?`, id).Scan(
			&retired.PersonID, &retired.DisplayName, &retired.DisplayNameFolded, &retired.Origin, &retired.State,
			&retired.Revision, &retired.CreatedAt, &retired.UpdatedAt); err != nil {
			return err
		}
		return advancePersonBindingEpochTx(ctx, tx)
	})
	if err != nil {
		return Person{}, err
	}
	return retired, nil
}

func resolvePersonIDTx(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	var survivor sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT surviving_person_id FROM person_aliases WHERE retired_person_id=?`, id).Scan(&survivor)
	if err == nil {
		if !survivor.Valid || survivor.String == "" {
			return "", ErrNotFound
		}
		id = survivor.String
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM persons WHERE person_id=?)`, id).Scan(&exists); err != nil {
		return "", err
	}
	if !exists {
		return "", ErrNotFound
	}
	return id, nil
}

func (s *Store) PersonByID(ctx context.Context, id string) (Person, string, error) {
	var person Person
	err := s.db.QueryRowContext(ctx, `SELECT person_id,display_name,display_name_folded,origin,state,revision,created_at,updated_at
		FROM persons WHERE person_id=COALESCE((SELECT surviving_person_id FROM person_aliases WHERE retired_person_id=?),?)
		AND state<>'retired'`, id, id).Scan(&person.PersonID, &person.DisplayName, &person.DisplayNameFolded, &person.Origin,
		&person.State, &person.Revision, &person.CreatedAt, &person.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Person{}, "", ErrNotFound
	}
	if err != nil {
		return Person{}, "", err
	}
	reachedThrough := ""
	if person.PersonID != id {
		reachedThrough = id
	}
	return person, reachedThrough, nil
}

// PeopleByDisplayName returns active people in stable folded-name order.
func (s *Store) PeopleByDisplayName(ctx context.Context, query, afterName, afterID string, limit int) ([]Person, error) {
	if !utf8.ValidString(query) || len(query) > document.MaxPersonDisplayNameBytes || limit < 1 || limit > 251 ||
		(afterName == "") != (afterID == "") {
		return nil, ErrInvalidPerson
	}
	prefix := document.FoldPersonName(query)
	rows, err := s.db.QueryContext(ctx, `SELECT person_id,display_name,display_name_folded,origin,state,revision,created_at,updated_at
		FROM persons WHERE state<>'retired' AND display_name_folded LIKE ? ESCAPE '\'
		AND (?='' OR display_name_folded>? OR (display_name_folded=? AND person_id>?))
		ORDER BY display_name_folded,person_id LIMIT ?`, escapePersonLike(prefix)+"%", afterName, afterName, afterName, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	people := []Person{}
	for rows.Next() {
		var person Person
		if err := rows.Scan(&person.PersonID, &person.DisplayName, &person.DisplayNameFolded, &person.Origin,
			&person.State, &person.Revision, &person.CreatedAt, &person.UpdatedAt); err != nil {
			return nil, err
		}
		people = append(people, person)
	}
	return people, rows.Err()
}

func escapePersonLike(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	return strings.ReplaceAll(value, `_`, `\_`)
}

func (s *Store) AddPersonIdentity(ctx context.Context, personID string, revision int64, identity PersonIdentity) (PersonIdentity, error) {
	if len(identity.EvidenceKind) > document.MaxPersonEvidenceKindBytes || len(identity.EvidenceID) > document.MaxPersonEvidenceIDBytes ||
		!slices.Contains(document.PersonEvidenceKinds(), document.PersonEvidenceKind(identity.EvidenceKind)) || identity.EvidenceID == "" ||
		!validPersonOrigin(identity.Origin) || !validPersonConfidence(identity.Confidence) {
		return PersonIdentity{}, ErrInvalidPerson
	}
	normalized, err := document.NormalizeScopedPersonIdentity(document.PersonIdentityKind(identity.Kind), identity.ValueDisplay, identity.ScopeKind, identity.ScopeValue)
	if err != nil {
		return PersonIdentity{}, ErrInvalidPerson
	}
	id, err := newUUIDv4()
	if err != nil {
		return PersonIdentity{}, err
	}
	identity.IdentityID, identity.PersonID = id, personID
	identity.ValueNormalized, identity.Normalization = normalized.ValueNormalized, normalized.Normalization
	identity.RecordedAt = nowRFC3339()
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		if err := fencePersonTx(ctx, tx, personID, revision); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_identities WHERE person_id=?`, personID).Scan(&count); err != nil {
			return err
		}
		if count >= document.MaxPersonIdentitiesPerPerson {
			return ErrPersonIdentityConflict
		}
		result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO person_identities(identity_id,person_id,kind,value_normalized,value_display,scope_kind,scope_value,normalization,origin,evidence_kind,evidence_id,confidence,recorded_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, identity.IdentityID, personID, identity.Kind, identity.ValueNormalized, identity.ValueDisplay, identity.ScopeKind, identity.ScopeValue, identity.Normalization, identity.Origin, identity.EvidenceKind, identity.EvidenceID, identity.Confidence, identity.RecordedAt)
		if err != nil {
			return err
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if inserted != 1 {
			return ErrPersonIdentityConflict
		}
		if _, err := tx.ExecContext(ctx, `UPDATE persons SET revision=revision+1,updated_at=? WHERE person_id=?`, nowRFC3339(), personID); err != nil {
			return err
		}
		return advancePersonBindingEpochTx(ctx, tx)
	})
	if err != nil {
		return PersonIdentity{}, err
	}
	return identity, nil
}

func (s *Store) RemovePersonIdentity(ctx context.Context, personID, identityID string, revision int64) error {
	return s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		if err := fencePersonTx(ctx, tx, personID, revision); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM person_identities WHERE person_id=? AND identity_id=?`, personID, identityID)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, `UPDATE persons SET revision=revision+1,updated_at=? WHERE person_id=?`, nowRFC3339(), personID); err != nil {
			return err
		}
		return advancePersonBindingEpochTx(ctx, tx)
	})
}

func (s *Store) PersonIdentities(ctx context.Context, personID string) ([]PersonIdentity, error) {
	person, _, err := s.PersonByID(ctx, personID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT identity_id,person_id,kind,value_normalized,value_display,normalization,origin,evidence_kind,evidence_id,confidence,recorded_at,scope_kind,scope_value FROM person_identities WHERE person_id=? ORDER BY kind,value_normalized,identity_id`, person.PersonID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	identities := []PersonIdentity{}
	for rows.Next() {
		var identity PersonIdentity
		if err := rows.Scan(&identity.IdentityID, &identity.PersonID, &identity.Kind, &identity.ValueNormalized, &identity.ValueDisplay, &identity.Normalization, &identity.Origin, &identity.EvidenceKind, &identity.EvidenceID, &identity.Confidence, &identity.RecordedAt, &identity.ScopeKind, &identity.ScopeValue); err != nil {
			return nil, err
		}
		identities = append(identities, identity)
	}
	return identities, rows.Err()
}

func fencePersonTx(ctx context.Context, tx *sql.Tx, personID string, revision int64) error {
	var state string
	var current int64
	err := tx.QueryRowContext(ctx, `SELECT state,revision FROM persons WHERE person_id=?`, personID).Scan(&state, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if state == "retired" {
		return ErrPersonRetired
	}
	if current != revision {
		return ErrStaleRevision
	}
	return nil
}

// advancePersonBindingEpochTx invalidates person bindings after an authority change.
func advancePersonBindingEpochTx(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE document_people_state SET binding_epoch=binding_epoch+1,updated_at=? WHERE singleton=1`, nowRFC3339())
	return err
}
