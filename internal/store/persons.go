package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

const personStateRetired = "retired"

type Person struct {
	PersonID, DisplayName, DisplayNameFolded, Origin, State string
	Revision                                                int64
	CreatedAt, UpdatedAt                                    string
}

// PersonDetail contains one snapshot of a person and the selectors needed to
// choose explicit members for a split.
type PersonDetail struct {
	Person

	KeyFaceID            string
	Assertions           []PersonDocumentAssertion
	AssertionsNextCursor string
	AssertionsTruncated  bool
	ReachedThrough       string
	Identities           []PersonIdentity
	External             []PersonExternalIdentity
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

func createPersonTx(ctx context.Context, tx *sql.Tx, displayName, origin string) (Person, error) {
	id, err := newUUIDv4()
	if err != nil {
		return Person{}, err
	}
	return createPersonWithIDTx(ctx, tx, id, displayName, origin)
}

func createPersonWithIDTx(ctx context.Context, tx *sql.Tx, id, displayName, origin string) (Person, error) {
	if !validPersonName(displayName) || !validPersonOrigin(origin) {
		return Person{}, ErrInvalidPerson
	}
	state := "provisional"
	if origin == "operator" {
		state = "curated"
	}
	now := nowRFC3339()
	if _, err := tx.ExecContext(ctx, `INSERT INTO persons(person_id,display_name,display_name_folded,origin,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, id, displayName, document.FoldPersonName(displayName), origin, state, now, now); err != nil {
		return Person{}, err
	}
	var created Person
	err := tx.QueryRowContext(ctx, `SELECT person_id,display_name,display_name_folded,origin,state,revision,created_at,updated_at FROM persons WHERE person_id=?`, id).Scan(
		&created.PersonID, &created.DisplayName, &created.DisplayNameFolded, &created.Origin, &created.State,
		&created.Revision, &created.CreatedAt, &created.UpdatedAt)
	return created, err
}

func (s *Store) CreatePerson(ctx context.Context, displayName, origin string) (Person, error) {
	var created Person
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		var err error
		created, err = createPersonTx(ctx, tx, displayName, origin)
		if err != nil {
			return err
		}
		return advancePersonBindingEpochTx(ctx, tx)
	})
	if err != nil {
		return Person{}, err
	}
	return created, nil
}

func (s *Store) UpdatePerson(ctx context.Context, id string, revision int64, name string) (Person, error) {
	if !validPersonName(name) {
		return Person{}, ErrInvalidPerson
	}
	var updated Person
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		if err := s.fenceVisiblePeopleTx(ctx, tx, id); err != nil {
			return err
		}
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
		if err := tx.QueryRowContext(ctx, `SELECT person_id,display_name,display_name_folded,origin,state,revision,created_at,updated_at FROM persons WHERE person_id=?`, id).Scan(
			&updated.PersonID, &updated.DisplayName, &updated.DisplayNameFolded, &updated.Origin, &updated.State,
			&updated.Revision, &updated.CreatedAt, &updated.UpdatedAt); err != nil {
			return err
		}
		return advancePersonBindingEpochTx(ctx, tx)
	})
	if err != nil {
		return Person{}, err
	}
	return updated, nil
}

// RetirePerson keeps document assertions as historical operator decisions.
// They remain readable, but the retired person cannot receive new assertions.
func (s *Store) RetirePerson(ctx context.Context, id string, revision int64) (Person, error) {
	var retired Person
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		var err error
		retired, err = retirePersonTx(ctx, tx, id, revision)
		return err
	})
	if err != nil {
		return Person{}, err
	}
	return retired, nil
}

func retirePersonTx(ctx context.Context, tx *sql.Tx, id string, revision int64) (Person, error) {
	var retired Person
	if err := fencePersonTx(ctx, tx, id, revision); err != nil {
		return Person{}, err
	}
	now := nowRFC3339()
	result, err := tx.ExecContext(ctx, `UPDATE persons SET state='retired',revision=revision+1,updated_at=? WHERE person_id=? AND revision=? AND state<>'retired'`, now, id, revision)
	if err != nil {
		return Person{}, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return Person{}, err
	}
	if count != 1 {
		return Person{}, ErrStaleRevision
	}
	if _, err := tx.ExecContext(ctx, `UPDATE person_aliases SET surviving_person_id=NULL,reason='deleted',retired_at=? WHERE surviving_person_id=?`, now, id); err != nil {
		return Person{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO person_aliases(retired_person_id,surviving_person_id,reason,retired_at) VALUES(?,NULL,'deleted',?)`, id, now); err != nil {
		return Person{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE custodian_assignments SET person_id=NULL,revision=revision+1 WHERE person_id=? AND retired_at IS NULL`, id); err != nil {
		return Person{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE person_match_candidates SET state='superseded',revision=revision+1 WHERE suggested_person_id=? AND state='open'`, id); err != nil {
		return Person{}, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT person_id,display_name,display_name_folded,origin,state,revision,created_at,updated_at FROM persons WHERE person_id=?`, id).Scan(
		&retired.PersonID, &retired.DisplayName, &retired.DisplayNameFolded, &retired.Origin, &retired.State,
		&retired.Revision, &retired.CreatedAt, &retired.UpdatedAt); err != nil {
		return Person{}, err
	}
	if err := advancePersonBindingEpochTx(ctx, tx); err != nil {
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

func personByIDTx(ctx context.Context, q metadataQuerier, id string) (Person, string, error) {
	var person Person
	err := q.QueryRowContext(ctx, `SELECT person_id,display_name,display_name_folded,origin,state,revision,created_at,updated_at
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

func (s *Store) PersonByID(ctx context.Context, id string) (Person, string, error) {
	return personByIDTx(ctx, s.db, id)
}

// personVisibleSQL is the browser's person visibility rule. It binds
// visibleOnly, then hideLocked twice. Browser rename and merge use it inside
// their write transactions, so a person that Hidden locking or sensitivity
// conceals from the list cannot be edited by ID.
func personVisibleSQL(person string) string {
	return `(?=0 OR (NOT EXISTS(SELECT 1 FROM person_document_assertions a WHERE a.person_id=` + person + `)
		AND NOT EXISTS(SELECT 1 FROM document_people p WHERE p.person_id=` + person + `)
		AND NOT EXISTS(SELECT 1 FROM person_identities i WHERE i.person_id=` + person + `
			AND i.origin='derived' AND i.confidence='exact_identifier')) OR EXISTS(
		SELECT 1 FROM person_document_assertions a WHERE a.person_id=` + person + `
		AND a.role<>'blind_copy' AND (?=0 OR ` + personVersionVisibleSQL("a.content_version_id") + `)) OR EXISTS(
		SELECT 1 FROM document_people p WHERE p.person_id=` + person + `
		AND p.sensitive=0 AND (?=0 OR ` + personVersionVisibleSQL("p.content_version_id") + `)))`
}

// personRecentSQL keeps namesakes together and ranks their readable, current
// depicted evidence. It starts from each namesake's indexed assertions, so a
// typed prefix reads only the matched people's evidence rather than every
// current photo. The inner LIMIT -1 keeps SQLite from flattening the sorted
// assertions into the outer query, so the photo checks stop at the newest
// readable one. It binds whether locked Hidden photos must be concealed.
func personRecentSQL() string {
	joins, where := personPhotoSource(true)
	return `COALESCE((SELECT recent.recorded_at FROM (
 SELECT assertion.recorded_at,assertion.content_version_id FROM persons namesake
 CROSS JOIN person_document_assertions assertion ON assertion.person_id=namesake.person_id AND assertion.action='assert'
 WHERE namesake.display_name_folded=persons.display_name_folded AND namesake.state<>'retired' AND assertion.role='depicted'
 ORDER BY recorded_at DESC LIMIT -1) recent
 WHERE EXISTS(SELECT 1 FROM content_versions cv` + joins + where + `
 AND cv.version_id=recent.content_version_id AND (?=0 OR ` + photoVisibilityPredicate(false) + `))
 LIMIT 1),'')`
}

// Materialize the bounded matches before reading their visible identity labels.
var peopleByDisplayNameSQL = `WITH matches AS MATERIALIZED (
 SELECT persons.*%s FROM persons WHERE %s state<>'retired'
	AND ` + personVisibleSQL("persons.person_id") + `
	AND (%s)
	ORDER BY %s LIMIT ?)
SELECT person_id,display_name,display_name_folded,origin,state,revision,created_at,updated_at,%s,
	COALESCE((SELECT CASE WHEN i.kind='handle' THEN i.value_display || ' (' || i.scope_kind || ': ' || i.scope_value || ')' ELSE i.value_display END
		FROM person_identities i WHERE i.person_id=persons.person_id
		AND (i.kind IN ('email','phone') OR (i.kind='handle' AND i.scope_kind<>'' AND i.scope_value<>''))
		AND (?=0 OR i.origin='operator' OR EXISTS(SELECT 1 FROM document_people dp
			WHERE dp.person_id=i.person_id AND dp.actor_key=i.kind || ':' || i.value_normalized
			AND dp.sensitive=0 AND (?=0 OR ` + personVersionVisibleSQL("dp.content_version_id") + `)))
		ORDER BY CASE i.kind WHEN 'email' THEN 0 WHEN 'phone' THEN 1 ELSE 2 END,i.value_normalized,i.identity_id LIMIT 1),'')
 FROM matches persons ORDER BY %s`

type PersonSearchResult struct {
	Person

	IdentityLabel string
	// RecentRank is the name group's newest visible depicted evidence time
	// when ranked recent-first, and empty otherwise. Cursors carry it.
	RecentRank   string
	PhotoSummary *PersonPhotoSummary
}

type PersonPhotoSummary struct {
	HiddenCount  *int64             `json:"hidden_count,omitzero"`
	FaceHidden   bool               `json:"face_hidden,omitzero"`
	LibraryCount int64              `json:"library_count"`
	Face         *PersonFacePreview `json:"face,omitzero"`
}

type PeopleSearchOptions struct {
	// RecentFirst orders name groups by their newest visible depicted
	// evidence, then by folded name and ID. AfterRecent continues that order.
	RecentFirst  bool
	AfterRecent  string
	PhotoSummary bool
	GridRecipe   string
}

// peopleCandidatesSQL narrows the matches to a typed name prefix and, for the
// Photos page, to people with a visible depicted photo: Library photos, and
// Hidden photos too while unlocked.
func peopleCandidatesSQL(prefix string, photosOnly, unlocked bool) (string, []any) {
	clause, args := "", []any{}
	if prefix != "" {
		clause, args = `instr(display_name_folded, ?) > 0 AND docbank_person_name_starts_with_word_v1(display_name_folded, ?) AND`, []any{prefix, prefix}
	}
	if photosOnly {
		joins, where := personPhotoSource(unlocked)
		clause += ` EXISTS(SELECT 1 FROM person_document_assertions assertion
 CROSS JOIN content_versions cv ON cv.version_id=assertion.content_version_id` + joins + where + `
 AND assertion.person_id=persons.person_id AND assertion.action='assert' AND assertion.role='depicted') AND`
	}
	return clause, args
}

// PeopleByDisplayName returns active people in stable folded-name order, or
// recent-first when requested.
func (s *Store) PeopleByDisplayName(ctx context.Context, query, afterName, afterID string, limit int, visibleOnly bool, options PeopleSearchOptions) ([]PersonSearchResult, error) {
	if !utf8.ValidString(query) || len(query) > document.MaxPersonDisplayNameBytes || limit < 1 || limit > 251 ||
		(afterName == "") != (afterID == "") || (!options.RecentFirst && options.AfterRecent != "") {
		return nil, ErrInvalidPerson
	}
	people := []PersonSearchResult{}
	err := s.photoReadTx(ctx, func(tx *sql.Tx) error {
		hideLocked := false
		if visibleOnly || options.PhotoSummary {
			_, err := s.hiddenSession(ctx, tx)
			if err != nil && !errors.Is(err, ErrHiddenLocked) {
				return err
			}
			hideLocked = errors.Is(err, ErrHiddenLocked)
		}
		rankColumn, rank := "", `''`
		order := "display_name_folded,person_id"
		cursor := `?='' OR display_name_folded>? OR (display_name_folded=? AND person_id>?)`
		cursorArgs := []any{afterName, afterName, afterName, afterID}
		args := []any{}
		if options.RecentFirst {
			rankColumn, rank = ","+personRecentSQL()+" recent_rank", "recent_rank"
			order = "recent_rank DESC," + order
			cursor = `?='' OR recent_rank<? OR (recent_rank=? AND (display_name_folded>? OR (display_name_folded=? AND person_id>?)))`
			cursorArgs = []any{afterName, options.AfterRecent, options.AfterRecent, afterName, afterName, afterID}
			args = append(args, hideLocked)
		}
		candidates, filter := peopleCandidatesSQL(document.FoldPersonName(query), options.PhotoSummary, !hideLocked)
		args = append(args, filter...)
		args = append(args, visibleOnly, hideLocked, hideLocked)
		args = append(args, cursorArgs...)
		args = append(args, limit, visibleOnly, hideLocked)
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(peopleByDisplayNameSQL, rankColumn, candidates, cursor, order, rank, order), args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var person PersonSearchResult
			if err := rows.Scan(&person.PersonID, &person.DisplayName, &person.DisplayNameFolded, &person.Origin,
				&person.State, &person.Revision, &person.CreatedAt, &person.UpdatedAt, &person.RecentRank, &person.IdentityLabel); err != nil {
				return err
			}
			people = append(people, person)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		if options.PhotoSummary {
			return s.personPhotoSummaries(ctx, tx, people, options.GridRecipe, !hideLocked)
		}
		return nil
	})
	return people, err
}

// visiblePersonContextKey marks browser writes, which may only change people
// the browser can list.
type visiblePersonContextKey struct{}

func WithVisiblePeopleOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, visiblePersonContextKey{}, true)
}

// fenceVisiblePeopleTx reports a person concealed from a browser write as not
// found. API-key writes keep their full scope.
func (s *Store) fenceVisiblePeopleTx(ctx context.Context, tx *sql.Tx, ids ...string) error {
	if visible, _ := ctx.Value(visiblePersonContextKey{}).(bool); !visible {
		return nil
	}
	_, err := s.hiddenSession(ctx, tx)
	if err != nil && !errors.Is(err, ErrHiddenLocked) {
		return err
	}
	hideLocked := errors.Is(err, ErrHiddenLocked)
	for _, id := range ids {
		var found bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM persons WHERE person_id=? AND `+personVisibleSQL("persons.person_id")+`)`,
			id, true, hideLocked, hideLocked).Scan(&found); err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
	}
	return nil
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

func personIdentitiesTx(ctx context.Context, q metadataQuerier, personID string) ([]PersonIdentity, error) {
	rows, err := q.QueryContext(ctx, `SELECT identity_id,person_id,kind,value_normalized,value_display,normalization,origin,evidence_kind,evidence_id,confidence,recorded_at,scope_kind,scope_value FROM person_identities WHERE person_id=? ORDER BY kind,value_normalized,identity_id`, personID)
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

func (s *Store) PersonIdentities(ctx context.Context, personID string) ([]PersonIdentity, error) {
	person, _, err := personByIDTx(ctx, s.db, personID)
	if err != nil {
		return nil, err
	}
	return personIdentitiesTx(ctx, s.db, person.PersonID)
}

type PersonAssertionPage struct {
	After string
	Limit int
}

func (s *Store) PersonDetailPage(ctx context.Context, personID string, page PersonAssertionPage) (PersonDetail, error) {
	if page.Limit == 0 {
		page.Limit = 1000
	}
	if page.Limit < 1 || page.Limit > 1000 || page.After != "" && validateUUIDv4(page.After) != nil {
		return PersonDetail{}, ErrInvalidPerson
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PersonDetail{}, err
	}
	defer func() { _ = tx.Rollback() }()
	person, reachedThrough, err := personByIDTx(ctx, tx, personID)
	if err != nil {
		return PersonDetail{}, err
	}
	identities, err := personIdentitiesTx(ctx, tx, person.PersonID)
	if err != nil {
		return PersonDetail{}, err
	}
	external, err := personExternalIdentitiesTx(ctx, tx, person.PersonID)
	if err != nil {
		return PersonDetail{}, err
	}
	assertions, key, truncated, err := s.personFaceDetailTx(ctx, tx, person.PersonID, page)
	if err != nil {
		return PersonDetail{}, err
	}
	if err := tx.Commit(); err != nil {
		return PersonDetail{}, err
	}
	next := ""
	if truncated && len(assertions) > 0 {
		next = assertions[len(assertions)-1].AssertionID
	}
	return PersonDetail{AssertionsNextCursor: next, KeyFaceID: key, Assertions: assertions, AssertionsTruncated: truncated, Person: person, ReachedThrough: reachedThrough, Identities: identities, External: external}, nil
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

// advancePersonBindingEpochForVersionTx avoids invalidating projections that cannot read this version.
func advancePersonBindingEpochForVersionTx(ctx context.Context, tx *sql.Tx, versionID string) error {
	var hasEventHead bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM document_event_heads WHERE content_version_id=?)`, versionID).Scan(&hasEventHead); err != nil {
		return err
	}
	if !hasEventHead {
		return nil
	}
	return advancePersonBindingEpochTx(ctx, tx)
}
