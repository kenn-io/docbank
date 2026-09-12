package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/query"
	"golang.org/x/text/unicode/norm"
)

const (
	// SavedQueryKindQuery identifies a canonical QueryV1 payload.
	SavedQueryKindQuery = "query"
	// SavedQueryKindHighlightSet identifies a canonical HighlightSetV1 payload.
	SavedQueryKindHighlightSet = "highlight_set"

	maxLabelNameBytes             = 256
	maxSavedQueryDescriptionBytes = 4096
)

// ErrInvalidSavedQuery identifies an invalid definition, patch, or list selector.
var ErrInvalidSavedQuery = errors.New("invalid saved query")

// SavedQuery is one stable, named query or literal highlight definition.
type SavedQuery struct {
	ID          string
	Name        string
	Description string
	Kind        string
	Payload     []byte
	Fingerprint string
	Revision    int64
	CreatedAt   string
	UpdatedAt   string
}

// SavedQueryPatch replaces only the explicitly provided mutable fields.
type SavedQueryPatch struct {
	Name        *string
	Description *string
	Payload     *[]byte
}

// CreateSavedQuery stores one validated definition under a fresh stable ID.
func (s *Store) CreateSavedQuery(
	ctx context.Context, name, description, kind string, payload []byte,
) (SavedQuery, error) {
	name, err := normalizeLabelName(name, ErrInvalidSavedQuery)
	if err != nil {
		return SavedQuery{}, err
	}
	description = strings.ReplaceAll(description, "\r\n", "\n")
	if err := validateSavedQueryDescription(description); err != nil {
		return SavedQuery{}, err
	}
	canonical, fingerprint, err := canonicalizeSavedQueryPayload(kind, payload)
	if err != nil {
		return SavedQuery{}, err
	}
	id, err := newUUIDv4()
	if err != nil {
		return SavedQuery{}, fmt.Errorf("allocating saved query ID: %w", err)
	}
	timestamp := nowRFC3339()
	created := SavedQuery{
		ID: id, Name: name, Description: description, Kind: kind,
		Payload: canonical, Fingerprint: fingerprint, Revision: 1,
		CreatedAt: timestamp, UpdatedAt: timestamp,
	}
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO saved_queries(
			id,name,description,kind,payload,fingerprint,revision,created_at,updated_at
		) VALUES(?,?,?,?,?,?,?,?,?)`, created.ID, created.Name, created.Description,
			created.Kind, created.Payload, created.Fingerprint, created.Revision,
			created.CreatedAt, created.UpdatedAt)
		if err != nil {
			if s.driver.IsUniqueViolation(err) {
				return fmt.Errorf("saved query %q: %w", name, ErrExists)
			}
			return fmt.Errorf("creating saved query %q: %w", name, err)
		}
		return nil
	})
	if err != nil {
		return SavedQuery{}, err
	}
	return created, nil
}

// SavedQueryByID returns one saved definition by stable identity.
func (s *Store) SavedQueryByID(ctx context.Context, id string) (SavedQuery, error) {
	if err := validateUUIDv4(id); err != nil {
		return SavedQuery{}, fmt.Errorf("saved query %q: %w", id, ErrNotFound)
	}
	record, err := scanSavedQuery(s.db.QueryRowContext(ctx, `
		SELECT id,name,description,kind,payload,fingerprint,revision,created_at,updated_at
		FROM saved_queries WHERE id=?`, id))
	if err != nil {
		return SavedQuery{}, fmt.Errorf("saved query %q: %w", id, err)
	}
	return record, nil
}

// SavedQueries returns one name-sorted page and a consistent filtered total.
func (s *Store) SavedQueries(
	ctx context.Context, kind string, limit, offset int,
) ([]SavedQuery, int, error) {
	if err := validateSavedQueryPage(kind, limit, offset); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `
		WITH filtered AS (
		  SELECT id,name,description,kind,payload,fingerprint,revision,created_at,updated_at
		  FROM saved_queries WHERE ?='' OR kind=?
		), page AS (
		  SELECT * FROM filtered ORDER BY name,id LIMIT ? OFFSET ?
		), totals AS (SELECT COUNT(*) AS total FROM filtered)
		SELECT totals.total, COALESCE(page.id,''), COALESCE(page.name,''),
		       COALESCE(page.description,''), COALESCE(page.kind,''), page.payload,
		       COALESCE(page.fingerprint,''), COALESCE(page.revision,0),
		       COALESCE(page.created_at,''), COALESCE(page.updated_at,'')
		FROM totals LEFT JOIN page ON true ORDER BY page.name,page.id`,
		kind, kind, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("listing saved queries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var (
		result []SavedQuery
		total  int
	)
	for rows.Next() {
		var record SavedQuery
		if err := rows.Scan(&total, &record.ID, &record.Name, &record.Description,
			&record.Kind, &record.Payload, &record.Fingerprint, &record.Revision,
			&record.CreatedAt, &record.UpdatedAt); err != nil {
			return nil, 0, fmt.Errorf("listing saved queries: scanning page: %w", err)
		}
		if record.ID != "" {
			result = append(result, record)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("listing saved queries: %w", err)
	}
	return result, total, nil
}

// UpdateSavedQuery applies a revision-fenced patch. Canonical no-ops preserve
// the existing revision and timestamps.
func (s *Store) UpdateSavedQuery(
	ctx context.Context, id string, revision int64, patch SavedQueryPatch,
) (SavedQuery, error) {
	if err := validateUUIDv4(id); err != nil {
		return SavedQuery{}, fmt.Errorf("saved query %q: %w", id, ErrNotFound)
	}
	var normalizedName *string
	if patch.Name != nil {
		name, err := normalizeLabelName(*patch.Name, ErrInvalidSavedQuery)
		if err != nil {
			return SavedQuery{}, err
		}
		normalizedName = &name
	}
	if patch.Description != nil {
		description := strings.ReplaceAll(*patch.Description, "\r\n", "\n")
		if err := validateSavedQueryDescription(description); err != nil {
			return SavedQuery{}, err
		}
		patch.Description = &description
	}

	var updated SavedQuery
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		current, err := savedQueryByIDTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := checkSavedQueryRevision(current, revision); err != nil {
			return err
		}
		updated = current
		if normalizedName != nil {
			updated.Name = *normalizedName
		}
		if patch.Description != nil {
			updated.Description = *patch.Description
		}
		if patch.Payload != nil {
			updated.Payload, updated.Fingerprint, err = canonicalizeSavedQueryPayload(
				current.Kind, *patch.Payload,
			)
			if err != nil {
				return err
			}
		}
		if updated.Name == current.Name && updated.Description == current.Description &&
			bytes.Equal(updated.Payload, current.Payload) {
			updated = current
			return nil
		}
		updated.Revision++
		updated.UpdatedAt = max(current.UpdatedAt, nowRFC3339())
		result, err := tx.ExecContext(ctx, `UPDATE saved_queries SET
			name=?,description=?,payload=?,fingerprint=?,revision=?,updated_at=?
			WHERE id=? AND revision=?`, updated.Name, updated.Description, updated.Payload,
			updated.Fingerprint, updated.Revision, updated.UpdatedAt, id, revision)
		if err != nil {
			if s.driver.IsUniqueViolation(err) {
				return fmt.Errorf("saved query %q: %w", updated.Name, ErrExists)
			}
			return fmt.Errorf("updating saved query %s: %w", id, err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("updating saved query %s: reading result: %w", id, err)
		}
		if changed != 1 {
			return fmt.Errorf("saved query %s revision changed: %w", id, ErrStaleRevision)
		}
		return nil
	})
	if err != nil {
		return SavedQuery{}, err
	}
	return updated, nil
}

// DeleteSavedQuery removes one definition at the observed revision.
func (s *Store) DeleteSavedQuery(
	ctx context.Context, id string, revision int64,
) (SavedQuery, error) {
	if err := validateUUIDv4(id); err != nil {
		return SavedQuery{}, fmt.Errorf("saved query %q: %w", id, ErrNotFound)
	}
	var deleted SavedQuery
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		current, err := savedQueryByIDTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := checkSavedQueryRevision(current, revision); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx,
			`DELETE FROM saved_queries WHERE id=? AND revision=?`, id, revision)
		if err != nil {
			return fmt.Errorf("deleting saved query %s: %w", id, err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("deleting saved query %s: reading result: %w", id, err)
		}
		if changed != 1 {
			return fmt.Errorf("saved query %s revision changed: %w", id, ErrStaleRevision)
		}
		deleted = current
		return nil
	})
	if err != nil {
		return SavedQuery{}, err
	}
	return deleted, nil
}

func scanSavedQuery(row interface{ Scan(dest ...any) error }) (SavedQuery, error) {
	var record SavedQuery
	if err := row.Scan(&record.ID, &record.Name, &record.Description, &record.Kind,
		&record.Payload, &record.Fingerprint, &record.Revision, &record.CreatedAt,
		&record.UpdatedAt); errors.Is(err, sql.ErrNoRows) {
		return SavedQuery{}, ErrNotFound
	} else if err != nil {
		return SavedQuery{}, fmt.Errorf("scanning saved query: %w", err)
	}
	return record, nil
}

func savedQueryByIDTx(ctx context.Context, tx *sql.Tx, id string) (SavedQuery, error) {
	record, err := scanSavedQuery(tx.QueryRowContext(ctx, `
		SELECT id,name,description,kind,payload,fingerprint,revision,created_at,updated_at
		FROM saved_queries WHERE id=?`, id))
	if err != nil {
		return SavedQuery{}, fmt.Errorf("saved query %q: %w", id, err)
	}
	return record, nil
}

func checkSavedQueryRevision(record SavedQuery, expected int64) error {
	if record.Revision != expected {
		return fmt.Errorf("saved query %s revision is %d, expected %d: %w",
			record.ID, record.Revision, expected, ErrStaleRevision)
	}
	return nil
}

func normalizeLabelName(name string, invalid error) (string, error) {
	if !utf8.ValidString(name) {
		return "", fmt.Errorf("%w: name is not valid UTF-8", invalid)
	}
	name = norm.NFC.String(name)
	if len(name) < 1 || len(name) > maxLabelNameBytes {
		return "", fmt.Errorf("%w: name must contain 1 to %d UTF-8 bytes",
			invalid, maxLabelNameBytes)
	}
	if strings.TrimFunc(name, unicode.IsSpace) == "" {
		return "", fmt.Errorf("%w: name must not be all whitespace", invalid)
	}
	if strings.ContainsFunc(name, unicode.IsControl) {
		return "", fmt.Errorf("%w: name contains a control character", invalid)
	}
	return name, nil
}

func validateSavedQueryDescription(description string) error {
	if !utf8.ValidString(description) {
		return fmt.Errorf("%w: description is not valid UTF-8", ErrInvalidSavedQuery)
	}
	if len(description) > maxSavedQueryDescriptionBytes {
		return fmt.Errorf("%w: description exceeds %d UTF-8 bytes",
			ErrInvalidSavedQuery, maxSavedQueryDescriptionBytes)
	}
	for _, r := range description {
		if unicode.IsControl(r) && r != '\t' && r != '\n' {
			return fmt.Errorf("%w: description contains a control character", ErrInvalidSavedQuery)
		}
	}
	return nil
}

func canonicalizeSavedQueryPayload(kind string, payload []byte) ([]byte, string, error) {
	switch kind {
	case SavedQueryKindQuery:
		value, err := query.Parse(payload)
		if err != nil {
			return nil, "", fmt.Errorf("%w: query payload: %w", ErrInvalidSavedQuery, err)
		}
		canonical, fingerprint, err := query.CanonicalWithFingerprint(value)
		if err != nil {
			return nil, "", fmt.Errorf("%w: query payload: %w", ErrInvalidSavedQuery, err)
		}
		return canonical, fingerprint, nil
	case SavedQueryKindHighlightSet:
		value, err := query.ParseHighlightSet(payload)
		if err != nil {
			return nil, "", fmt.Errorf("%w: highlight payload: %w", ErrInvalidSavedQuery, err)
		}
		canonical, err := query.CanonicalHighlightSet(value)
		if err != nil {
			return nil, "", fmt.Errorf("%w: highlight payload: %w", ErrInvalidSavedQuery, err)
		}
		fingerprint, err := query.HighlightSetFingerprint(value)
		if err != nil {
			return nil, "", fmt.Errorf("%w: highlight payload: %w", ErrInvalidSavedQuery, err)
		}
		return canonical, fingerprint, nil
	default:
		return nil, "", fmt.Errorf("%w: unknown kind %q", ErrInvalidSavedQuery, kind)
	}
}

func validateSavedQueryPage(kind string, limit, offset int) error {
	if kind != "" && kind != SavedQueryKindQuery && kind != SavedQueryKindHighlightSet {
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidSavedQuery, kind)
	}
	if err := validatePage(limit, offset); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidSavedQuery, err)
	}
	return nil
}
