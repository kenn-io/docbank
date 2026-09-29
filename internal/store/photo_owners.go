package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const maxPhotoOwnerNameBytes = 256

// PhotoOwner identifies the durable person whose photo assets a request may
// access. The resource owner used by browser caches and downloads is a
// separate value.
type PhotoOwner struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Revision  int64  `json:"revision"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func validatePhotoOwnerName(name string) error {
	if !utf8.ValidString(name) || strings.TrimSpace(name) != name || name == "" || len(name) > maxPhotoOwnerNameBytes {
		return fmt.Errorf("%w: name must be non-empty UTF-8 text of at most %d bytes", ErrInvalidPhotoOwner, maxPhotoOwnerNameBytes)
	}
	return nil
}

func scanPhotoOwner(row interface{ Scan(args ...any) error }) (PhotoOwner, error) {
	var owner PhotoOwner
	if err := row.Scan(&owner.ID, &owner.Name, &owner.Revision, &owner.CreatedAt, &owner.UpdatedAt); err != nil {
		return PhotoOwner{}, err
	}
	if validateUUIDv4(owner.ID) != nil || owner.Revision < 1 || validatePhotoOwnerName(owner.Name) != nil {
		return PhotoOwner{}, fmt.Errorf("%w: malformed owner row", ErrInvalidPhotoOwner)
	}
	return owner, nil
}

func photoOwnerByIDTx(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, id string) (PhotoOwner, error) {
	if validateUUIDv4(id) != nil {
		return PhotoOwner{}, ErrNotFound
	}
	owner, err := scanPhotoOwner(q.QueryRowContext(ctx, `
		SELECT owner_id, name, revision, created_at, updated_at
		FROM photo_owners WHERE owner_id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return PhotoOwner{}, ErrNotFound
	}
	if err != nil {
		return PhotoOwner{}, fmt.Errorf("reading photo owner %q: %w", id, err)
	}
	return owner, nil
}

func (s *Store) PhotoOwner(ctx context.Context, id string) (PhotoOwner, error) {
	return photoOwnerByIDTx(ctx, s.db, id)
}

// PhotoOwners returns owner records in stable UUID order. It never creates a
// default owner, which keeps read-only schema and OpenAPI paths side-effect
// free.
func (s *Store) PhotoOwners(ctx context.Context) ([]PhotoOwner, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT owner_id, name, revision, created_at, updated_at
		FROM photo_owners ORDER BY owner_id`)
	if err != nil {
		return nil, fmt.Errorf("listing photo owners: %w", err)
	}
	defer func() { _ = rows.Close() }()
	owners := make([]PhotoOwner, 0)
	for rows.Next() {
		owner, scanErr := scanPhotoOwner(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		owners = append(owners, owner)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing photo owners: %w", err)
	}
	return owners, nil
}

func defaultPhotoOwnerTx(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}) (PhotoOwner, error) {
	var id sql.NullString
	err := q.QueryRowContext(ctx, `
		SELECT default_owner_id FROM photo_library_settings WHERE singleton=1`).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PhotoOwner{}, ErrNotFound
		}
		return PhotoOwner{}, fmt.Errorf("reading default photo owner: %w", err)
	}
	if !id.Valid || id.String == "" {
		return PhotoOwner{}, ErrNotFound
	}
	owner, err := photoOwnerByIDTx(ctx, q, id.String)
	if errors.Is(err, ErrNotFound) {
		return PhotoOwner{}, fmt.Errorf("default photo owner %q: %w", id.String, ErrInvalidPhotoOwner)
	}
	return owner, err
}

func ensureDefaultPhotoOwnerTx(ctx context.Context, tx *sql.Tx) (PhotoOwner, error) {
	owner, err := defaultPhotoOwnerTx(ctx, tx)
	if err == nil {
		return owner, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return PhotoOwner{}, err
	}
	ownerID, err := newUUIDv4()
	if err != nil {
		return PhotoOwner{}, fmt.Errorf("allocating default photo owner: %w", err)
	}
	now := nowRFC3339()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO photo_owners(owner_id,name,revision,created_at,updated_at)
		VALUES(?, ?, 1, ?, ?)`, ownerID, "Default", now, now); err != nil {
		return PhotoOwner{}, fmt.Errorf("creating default photo owner: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO photo_library_settings(singleton,default_owner_id,preference,revision,updated_at)
		VALUES(1, ?, NULL, 1, ?)
		ON CONFLICT(singleton) DO UPDATE SET default_owner_id=excluded.default_owner_id`, ownerID, now); err != nil {
		return PhotoOwner{}, fmt.Errorf("recording default photo owner: %w", err)
	}
	return PhotoOwner{ID: ownerID, Name: "Default", Revision: 1, CreatedAt: now, UpdatedAt: now}, nil
}

// EnsureDefaultPhotoOwner creates the default identity inside the normal
// logical mutation gate. A caller can use this when an operation needs an
// owner but the vault has never enrolled a photo.
func (s *Store) EnsureDefaultPhotoOwner(ctx context.Context) (PhotoOwner, error) {
	var owner PhotoOwner
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		var err error
		owner, err = ensureDefaultPhotoOwnerTx(ctx, tx)
		return err
	})
	return owner, err
}

func (s *Store) CreatePhotoOwner(ctx context.Context, name string) (PhotoOwner, error) {
	if err := validatePhotoOwnerName(name); err != nil {
		return PhotoOwner{}, err
	}
	var owner PhotoOwner
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		id, err := newUUIDv4()
		if err != nil {
			return err
		}
		now := nowRFC3339()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO photo_owners(owner_id,name,revision,created_at,updated_at)
			VALUES(?,?,1,?,?)`, id, name, now, now); err != nil {
			return fmt.Errorf("creating photo owner: %w", err)
		}
		owner = PhotoOwner{ID: id, Name: name, Revision: 1, CreatedAt: now, UpdatedAt: now}
		return nil
	})
	return owner, err
}

func (s *Store) RenamePhotoOwner(ctx context.Context, id string, revision int64, name string) (PhotoOwner, error) {
	if validateUUIDv4(id) != nil || revision < 1 {
		return PhotoOwner{}, fmt.Errorf("%w: invalid owner or revision", ErrInvalidPhotoOwner)
	}
	if err := validatePhotoOwnerName(name); err != nil {
		return PhotoOwner{}, err
	}
	var owner PhotoOwner
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		current, err := photoOwnerByIDTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if current.Revision != revision {
			return fmt.Errorf("owner %s at revision %d, expected %d: %w", id, current.Revision, revision, ErrStaleRevision)
		}
		if current.Name == name {
			owner = current
			return nil
		}
		now := nowRFC3339()
		if _, err := tx.ExecContext(ctx, `UPDATE photo_owners SET name=?,revision=revision+1,updated_at=? WHERE owner_id=? AND revision=?`, name, now, id, revision); err != nil {
			return fmt.Errorf("renaming photo owner: %w", err)
		}
		owner = current
		owner.Name, owner.Revision, owner.UpdatedAt = name, revision+1, now
		return nil
	})
	return owner, err
}

func (s *Store) RemovePhotoOwner(ctx context.Context, id string, revision int64) error {
	if validateUUIDv4(id) != nil || revision < 1 {
		return fmt.Errorf("%w: invalid owner or revision", ErrInvalidPhotoOwner)
	}
	return s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		owner, err := photoOwnerByIDTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if owner.Revision != revision {
			return fmt.Errorf("owner %s at revision %d, expected %d: %w", id, owner.Revision, revision, ErrStaleRevision)
		}
		var refs int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_assets WHERE owner_id=?`, id).Scan(&refs); err != nil {
			return fmt.Errorf("checking photo owner references: %w", err)
		}
		if refs != 0 {
			return fmt.Errorf("owner %s still owns %d photo assets: %w", id, refs, ErrPhotoOwnerReferenced)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE photo_library_settings SET default_owner_id=NULL WHERE singleton=1 AND default_owner_id=?`, id); err != nil {
			return fmt.Errorf("clearing default photo owner: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM photo_owners WHERE owner_id=? AND revision=?`, id, revision); err != nil {
			return fmt.Errorf("removing photo owner: %w", err)
		}
		return nil
	})
}
