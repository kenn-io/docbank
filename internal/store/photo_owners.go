package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// PhotoOwner is a person enrolled as a photo owner. Its name is always the
// person's current display name.
type PhotoOwner struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	EnrolledAt string `json:"enrolled_at"`
}

const photoOwnerColumns = `o.person_id, p.display_name, o.enrolled_at
	FROM photo_owners o JOIN persons p ON p.person_id=o.person_id`

// defaultPhotoOwnerSQL selects the default owner: the earliest enrollment.
const defaultPhotoOwnerSQL = `(SELECT person_id FROM photo_owners ORDER BY enrolled_at, rowid LIMIT 1)`

func scanPhotoOwner(row interface{ Scan(args ...any) error }) (PhotoOwner, error) {
	var owner PhotoOwner
	err := row.Scan(&owner.ID, &owner.Name, &owner.EnrolledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PhotoOwner{}, ErrNotFound
	}
	if err != nil {
		return PhotoOwner{}, fmt.Errorf("reading photo owner: %w", err)
	}
	return owner, nil
}

func photoOwnerByIDTx(ctx context.Context, q rowQuerier, id string) (PhotoOwner, error) {
	return scanPhotoOwner(q.QueryRowContext(ctx, `SELECT `+photoOwnerColumns+` WHERE o.person_id=?`, id))
}

func defaultPhotoOwnerTx(ctx context.Context, q rowQuerier) (PhotoOwner, error) {
	return scanPhotoOwner(q.QueryRowContext(ctx, `SELECT `+photoOwnerColumns+` WHERE o.person_id=`+defaultPhotoOwnerSQL))
}

func (s *Store) PhotoOwner(ctx context.Context, id string) (PhotoOwner, error) {
	return photoOwnerByIDTx(ctx, s.db, id)
}

// PhotoOwners lists owners by enrollment, so the default owner comes first.
func (s *Store) PhotoOwners(ctx context.Context) ([]PhotoOwner, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+photoOwnerColumns+` ORDER BY o.enrolled_at, o.rowid`)
	if err != nil {
		return nil, fmt.Errorf("listing photo owners: %w", err)
	}
	defer func() { _ = rows.Close() }()
	owners := make([]PhotoOwner, 0)
	for rows.Next() {
		owner, err := scanPhotoOwner(rows)
		if err != nil {
			return nil, err
		}
		owners = append(owners, owner)
	}
	return owners, rows.Err()
}

// ensureDefaultPhotoOwnerTx enrolls an operator person named "Default" on
// first use inside the caller's logical transaction.
func ensureDefaultPhotoOwnerTx(ctx context.Context, tx *sql.Tx) (PhotoOwner, error) {
	owner, err := defaultPhotoOwnerTx(ctx, tx)
	if !errors.Is(err, ErrNotFound) {
		return owner, err
	}
	person, err := insertPersonTx(ctx, tx, "Default", "operator")
	if err != nil {
		return PhotoOwner{}, err
	}
	return enrollPhotoOwnerTx(ctx, tx, person.PersonID)
}

func enrollPhotoOwnerTx(ctx context.Context, tx *sql.Tx, personID string) (PhotoOwner, error) {
	if _, err := tx.ExecContext(ctx, `INSERT INTO photo_owners(person_id,enrolled_at) VALUES(?,?)`, personID, nowRFC3339()); err != nil {
		return PhotoOwner{}, fmt.Errorf("enrolling photo owner: %w", err)
	}
	return photoOwnerByIDTx(ctx, tx, personID)
}

// EnsureDefaultPhotoOwner registers the default owner under the logical
// mutation gate when the vault has none.
func (s *Store) EnsureDefaultPhotoOwner(ctx context.Context) (PhotoOwner, error) {
	owner, err := defaultPhotoOwnerTx(ctx, s.db)
	if !errors.Is(err, ErrNotFound) {
		return owner, err
	}
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		var err error
		owner, err = ensureDefaultPhotoOwnerTx(ctx, tx)
		return err
	})
	return owner, err
}

// ResolvePhotoOwner resolves the request's owner without writing. It
// reports false when the default owner has not been created yet and can be.
func (s *Store) ResolvePhotoOwner(ctx context.Context) (string, bool, error) {
	if ownerID, _ := PhotoOwnerFromContext(ctx); ownerID != "" {
		_, err := s.PhotoOwner(ctx, ownerID)
		return ownerID, true, err
	}
	owner, err := defaultPhotoOwnerTx(ctx, s.db)
	if errors.Is(err, ErrNotFound) {
		// Audited vaults can never register the default owner, so they resolve to no owner.
		var audited bool
		if err := s.db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM audit_authority WHERE singleton=1)`,
		).Scan(&audited); err != nil {
			return "", false, fmt.Errorf("checking audit authority: %w", err)
		}
		return "", audited, nil
	}
	return owner.ID, true, err
}

// PhotoOwnerForWrite resolves the request's owner for work that will create
// photos later, registering the default owner on first use. Audited vaults
// cannot register one and get an empty id, which matches no owner.
func (s *Store) PhotoOwnerForWrite(ctx context.Context) (string, error) {
	if ownerID, ok, err := s.ResolvePhotoOwner(ctx); ok || err != nil {
		return ownerID, err
	}
	owner, err := s.EnsureDefaultPhotoOwner(ctx)
	if errors.Is(err, ErrAuditMutationUnsupported) {
		return "", nil
	}
	return owner.ID, err
}

// EnrollPhotoOwner enrolls an active person at its current revision.
func (s *Store) EnrollPhotoOwner(ctx context.Context, personID string, revision int64) (PhotoOwner, error) {
	var owner PhotoOwner
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		if err := fencePersonTx(ctx, tx, personID, revision); errors.Is(err, ErrPersonRetired) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if _, err := photoOwnerByIDTx(ctx, tx, personID); err == nil {
			return fmt.Errorf("person %s: %w", personID, ErrPhotoOwnerEnrolled)
		}
		var err error
		owner, err = enrollPhotoOwnerTx(ctx, tx, personID)
		return err
	})
	return owner, err
}

// RemovePhotoOwner ends an enrollment that no photo asset references.
func (s *Store) RemovePhotoOwner(ctx context.Context, personID string, revision int64) error {
	return s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		if _, err := photoOwnerByIDTx(ctx, tx, personID); err != nil {
			return err
		}
		if err := fencePersonTx(ctx, tx, personID, revision); err != nil {
			return err
		}
		if err := refusePhotoOwnerReferencedTx(ctx, tx, `SELECT 1 FROM photo_assets WHERE owner_id=?`, personID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM photo_owners WHERE person_id=?`, personID)
		return err
	})
}

// refuseEnrolledPersonTx keeps a people edit from orphaning or moving photos.
func refuseEnrolledPersonTx(ctx context.Context, tx *sql.Tx, personID string) error {
	return refusePhotoOwnerReferencedTx(ctx, tx, `SELECT 1 FROM photo_owners WHERE person_id=?`, personID)
}

func refusePhotoOwnerReferencedTx(ctx context.Context, tx *sql.Tx, query, personID string) error {
	var referenced bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(`+query+`)`, personID).Scan(&referenced); err != nil {
		return fmt.Errorf("checking photo owner references: %w", err)
	}
	if referenced {
		return fmt.Errorf("person %s: %w", personID, ErrPhotoOwnerReferenced)
	}
	return nil
}
