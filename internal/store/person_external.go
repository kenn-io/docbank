package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
)

const (
	defaultPersonActorKeyLimit = 100
	maxPersonActorKeyLimit     = 250
)

type PersonExternalIdentity struct {
	PersonID, System, ArchiveID, UID, UIDKind, UIDState string
	LastSeenRevision                                    *int64
	DisplayNameSnapshot, LinkedAt, UpdatedAt            string
}

type ExternalUIDResolution struct {
	PersonID, System, ArchiveID, RequestedUID, ResolvedUID, UIDState string
	Forwarded                                                        bool
}

type PersonActorKeysRequest struct {
	PersonID string
	Limit    int
	Cursor   string
}

type PersonActorKeyPage struct {
	Items      []string
	NextCursor string
	Total      int64
}

func validExternalTuple(system, archiveID, uid string) bool {
	return document.ValidateExternalPersonTuple(system, archiveID, uid) == nil
}

func validExternalIdentityClassification(kind, state string) bool {
	return kind == "vcard_uid" && slices.Contains([]string{"current", "retired", "unlinked"}, state)
}

func validExternalIdentityDetails(snapshot string, revision *int64) bool {
	return len(snapshot) <= document.MaxPersonDisplayNameSnapshotBytes && utf8.ValidString(snapshot) &&
		(revision == nil || *revision >= 0)
}

func (s *Store) LinkExternalIdentity(ctx context.Context, identity PersonExternalIdentity, revision int64) (PersonExternalIdentity, error) {
	if !validExternalTuple(identity.System, identity.ArchiveID, identity.UID) ||
		!validExternalIdentityClassification(identity.UIDKind, identity.UIDState) ||
		!validExternalIdentityDetails(identity.DisplayNameSnapshot, identity.LastSeenRevision) {
		return PersonExternalIdentity{}, ErrInvalidPerson
	}
	now := nowRFC3339()
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		if err := fencePersonTx(ctx, tx, identity.PersonID, revision); err != nil {
			return err
		}
		var owner string
		err := tx.QueryRowContext(ctx, `SELECT person_id FROM person_external_identities WHERE system=? AND archive_id=? AND uid=?`, identity.System, identity.ArchiveID, identity.UID).Scan(&owner)
		isNew := errors.Is(err, sql.ErrNoRows)
		if err == nil && owner != identity.PersonID {
			return ErrPersonIdentityConflict
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_external_identities WHERE person_id=?`, identity.PersonID).Scan(&count); err != nil {
			return err
		}
		if isNew && count >= document.MaxPersonExternalIdentities {
			return ErrPersonIdentityConflict
		}
		if identity.UIDState == "current" {
			var conflict bool
			if err := tx.QueryRowContext(ctx, `SELECT
				EXISTS(SELECT 1 FROM person_external_uid_aliases WHERE system=? AND archive_id=? AND retired_uid=?)
				OR EXISTS(SELECT 1 FROM person_external_identities WHERE person_id=? AND system=? AND archive_id=? AND uid_state='current' AND uid<>?)`,
				identity.System, identity.ArchiveID, identity.UID, identity.PersonID, identity.System, identity.ArchiveID, identity.UID).Scan(&conflict); err != nil {
				return err
			}
			if conflict {
				return ErrPersonIdentityConflict
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO person_external_identities(person_id,system,archive_id,uid,uid_kind,uid_state,last_seen_revision,display_name_snapshot,linked_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(system,archive_id,uid) DO UPDATE SET uid_kind=excluded.uid_kind,uid_state=excluded.uid_state,last_seen_revision=excluded.last_seen_revision,display_name_snapshot=excluded.display_name_snapshot,updated_at=excluded.updated_at`, identity.PersonID, identity.System, identity.ArchiveID, identity.UID, identity.UIDKind, identity.UIDState, identity.LastSeenRevision, identity.DisplayNameSnapshot, now, now)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE persons SET revision=revision+1,updated_at=? WHERE person_id=?`, now, identity.PersonID); err != nil {
			return err
		}
		return advancePersonBindingEpochTx(ctx, tx)
	})
	if err != nil {
		return PersonExternalIdentity{}, err
	}
	items, err := s.PersonExternalIdentities(ctx, identity.PersonID)
	if err != nil {
		return PersonExternalIdentity{}, err
	}
	for _, item := range items {
		if item.System == identity.System && item.ArchiveID == identity.ArchiveID && item.UID == identity.UID {
			return item, nil
		}
	}
	return PersonExternalIdentity{}, ErrNotFound
}

func (s *Store) UnlinkExternalIdentity(ctx context.Context, system, archiveID, uid string, revision int64) error {
	if !validExternalTuple(system, archiveID, uid) {
		return ErrInvalidPerson
	}
	return s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		var personID string
		if err := tx.QueryRowContext(ctx, `SELECT person_id FROM person_external_identities WHERE system=? AND archive_id=? AND uid=?`, system, archiveID, uid).Scan(&personID); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if err := fencePersonTx(ctx, tx, personID, revision); err != nil {
			return err
		}
		now := nowRFC3339()
		if _, err := tx.ExecContext(ctx, `UPDATE person_external_identities SET uid_state='unlinked',updated_at=? WHERE system=? AND archive_id=? AND uid=?`, now, system, archiveID, uid); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE persons SET revision=revision+1,updated_at=? WHERE person_id=?`, now, personID); err != nil {
			return err
		}
		return advancePersonBindingEpochTx(ctx, tx)
	})
}

// RecordExternalUIDAliases retires any current identities for retiredUIDs and
// advances their owners' revisions in the same transaction as recording aliases.
func (s *Store) RecordExternalUIDAliases(ctx context.Context, system, archiveID, survivingUID string, retiredUIDs []string) error {
	if !validExternalTuple(system, archiveID, survivingUID) || len(retiredUIDs) > document.MaxPersonExternalIdentities {
		return ErrInvalidPerson
	}
	return s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		now := nowRFC3339()
		for _, retired := range retiredUIDs {
			if !validExternalTuple(system, archiveID, retired) || retired == survivingUID {
				return ErrInvalidPerson
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO person_external_uid_aliases(system,archive_id,retired_uid,surviving_uid,observed_at) VALUES(?,?,?,?,?) ON CONFLICT(system,archive_id,retired_uid) DO UPDATE SET surviving_uid=excluded.surviving_uid,observed_at=excluded.observed_at`, system, archiveID, retired, survivingUID, now); err != nil {
				return err
			}
			// An alias and its current identity must transition together so readers
			// never see an active key whose resolution points somewhere else.
			if _, err := tx.ExecContext(ctx, `UPDATE persons SET revision=revision+1,updated_at=? WHERE person_id=(
				SELECT person_id FROM person_external_identities WHERE system=? AND archive_id=? AND uid=? AND uid_state='current')`, now, system, archiveID, retired); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE person_external_identities SET uid_state='retired',updated_at=? WHERE system=? AND archive_id=? AND uid=? AND uid_state='current'`, now, system, archiveID, retired); err != nil {
				return err
			}
		}
		for _, retired := range retiredUIDs {
			if err := validateExternalAliasChainTx(ctx, tx, system, archiveID, retired); err != nil {
				return err
			}
		}
		return advancePersonBindingEpochTx(ctx, tx)
	})
}

func validateExternalAliasChainTx(ctx context.Context, tx *sql.Tx, system, archiveID, uid string) error {
	start := uid
	seen := map[string]bool{}
	for hops := range 33 {
		if seen[uid] {
			return errors.New("external UID alias cycle")
		}
		seen[uid] = true
		var next string
		err := tx.QueryRowContext(ctx, `SELECT surviving_uid FROM person_external_uid_aliases WHERE system=? AND archive_id=? AND retired_uid=?`, system, archiveID, uid).Scan(&next)
		if errors.Is(err, sql.ErrNoRows) {
			// A new target must also stay reachable from every existing inbound alias.
			var tooLong bool
			if err := tx.QueryRowContext(ctx, `WITH RECURSIVE ancestors(uid,depth) AS (
				SELECT ?,? UNION ALL
				SELECT a.retired_uid,ancestors.depth+1 FROM person_external_uid_aliases a
				JOIN ancestors ON a.surviving_uid=ancestors.uid
				WHERE a.system=? AND a.archive_id=? AND ancestors.depth<33
			) SELECT EXISTS(SELECT 1 FROM ancestors WHERE depth>32)`, start, hops, system, archiveID).Scan(&tooLong); err != nil {
				return err
			}
			if tooLong {
				return errors.New("external UID alias hop limit")
			}
			return nil
		}
		if err != nil {
			return err
		}
		uid = next
	}
	return errors.New("external UID alias hop limit")
}

func resolvePersonUIDTx(ctx context.Context, tx *sql.Tx, system, archive, uid string) (ExternalUIDResolution, error) {
	requested := uid
	seen := map[string]bool{}
	for range 33 {
		if seen[uid] {
			return ExternalUIDResolution{}, errors.New("external UID alias cycle")
		}
		seen[uid] = true
		var person, state string
		err := tx.QueryRowContext(ctx, `SELECT person_id,uid_state FROM person_external_identities WHERE system=? AND archive_id=? AND uid=?`, system, archive, uid).Scan(&person, &state)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return ExternalUIDResolution{}, err
		}
		if state == "unlinked" {
			return ExternalUIDResolution{}, ErrNotFound
		}
		var next string
		aliasErr := tx.QueryRowContext(ctx, `SELECT surviving_uid FROM person_external_uid_aliases WHERE system=? AND archive_id=? AND retired_uid=?`, system, archive, uid).Scan(&next)
		if aliasErr == nil {
			uid = next
			continue
		}
		if !errors.Is(aliasErr, sql.ErrNoRows) {
			return ExternalUIDResolution{}, aliasErr
		}
		if err == nil && state == "current" {
			resolvedPerson, err := resolvePersonIDTx(ctx, tx, person)
			if err != nil {
				return ExternalUIDResolution{}, err
			}
			return ExternalUIDResolution{PersonID: resolvedPerson, System: system, ArchiveID: archive, RequestedUID: requested, ResolvedUID: uid, UIDState: state, Forwarded: uid != requested}, nil
		}
		return ExternalUIDResolution{}, ErrNotFound
	}
	return ExternalUIDResolution{}, errors.New("external UID alias hop limit")
}

func (s *Store) ResolveExternalPersonUID(ctx context.Context, system, archiveID, uid string) (ExternalUIDResolution, error) {
	if !validExternalTuple(system, archiveID, uid) {
		return ExternalUIDResolution{}, ErrInvalidPerson
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ExternalUIDResolution{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := resolvePersonUIDTx(ctx, tx, system, archiveID, uid)
	if err != nil {
		return ExternalUIDResolution{}, err
	}
	return result, tx.Commit()
}

func (s *Store) PersonExternalIdentities(ctx context.Context, personID string) ([]PersonExternalIdentity, error) {
	person, _, err := s.PersonByID(ctx, personID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT person_id,system,archive_id,uid,uid_kind,uid_state,last_seen_revision,display_name_snapshot,linked_at,updated_at FROM person_external_identities WHERE person_id=? ORDER BY system,archive_id,uid`, person.PersonID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := []PersonExternalIdentity{}
	for rows.Next() {
		var item PersonExternalIdentity
		var lastSeen sql.NullInt64
		if err := rows.Scan(&item.PersonID, &item.System, &item.ArchiveID, &item.UID, &item.UIDKind, &item.UIDState, &lastSeen, &item.DisplayNameSnapshot, &item.LinkedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		if lastSeen.Valid {
			item.LastSeenRevision = &lastSeen.Int64
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) ActorKeysForPerson(ctx context.Context, request PersonActorKeysRequest) (PersonActorKeyPage, error) {
	if request.Limit == 0 {
		request.Limit = defaultPersonActorKeyLimit
	}
	if request.Limit < 1 || request.Limit > maxPersonActorKeyLimit {
		return PersonActorKeyPage{}, ErrInvalidPerson
	}
	last, err := decodePersonActorCursor(request.PersonID, request.Cursor)
	if err != nil {
		return PersonActorKeyPage{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PersonActorKeyPage{}, err
	}
	defer func() { _ = tx.Rollback() }()
	resolvedPersonID, err := resolvePersonIDTx(ctx, tx, request.PersonID)
	if err != nil {
		return PersonActorKeyPage{}, err
	}
	keys := map[string]struct{}{}
	err = func() (retErr error) {
		rows, err := tx.QueryContext(ctx, `SELECT kind,value_normalized FROM person_identities WHERE person_id=?`, resolvedPersonID)
		if err != nil {
			return err
		}
		defer func() { retErr = errors.Join(retErr, rows.Close()) }()
		for rows.Next() {
			var kind, value string
			if err := rows.Scan(&kind, &value); err != nil {
				return err
			}
			if value != "" {
				keys[kind+":"+value] = struct{}{}
			}
		}
		return rows.Err()
	}()
	if err != nil {
		return PersonActorKeyPage{}, err
	}
	err = func() (retErr error) {
		rows, err := tx.QueryContext(ctx, `SELECT system,archive_id,uid FROM person_external_identities WHERE person_id=? AND uid_state='current'`, resolvedPersonID)
		if err != nil {
			return err
		}
		defer func() { retErr = errors.Join(retErr, rows.Close()) }()
		for rows.Next() {
			var system, archive, uid string
			if err := rows.Scan(&system, &archive, &uid); err != nil {
				return err
			}
			key, err := document.ExternalPersonActorKey(system, archive, uid)
			if err != nil {
				return err
			}
			keys[key] = struct{}{}
		}
		return rows.Err()
	}()
	if err != nil {
		return PersonActorKeyPage{}, err
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		if key > last {
			ordered = append(ordered, key)
		}
	}
	slices.Sort(ordered)
	total := int64(len(keys))
	page := PersonActorKeyPage{Items: ordered, Total: total}
	if len(page.Items) > request.Limit {
		page.Items = page.Items[:request.Limit]
		page.NextCursor = encodePersonActorCursor(request.PersonID, page.Items[len(page.Items)-1])
	}
	if err := tx.Commit(); err != nil {
		return PersonActorKeyPage{}, err
	}
	return page, nil
}

func encodePersonActorCursor(personID, last string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(personID + "\x00" + last))
}

func decodePersonActorCursor(personID, cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	if len(cursor) > base64.RawURLEncoding.EncodedLen(len(personID)+1+document.MaxActorKeyBytes) {
		return "", ErrInvalidPerson
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(cursor)
	if err != nil {
		return "", ErrInvalidPerson
	}
	owner, last, ok := strings.Cut(string(raw), "\x00")
	if !ok || owner != personID || last == "" || len(last) > document.MaxActorKeyBytes {
		return "", ErrInvalidPerson
	}
	return last, nil
}
