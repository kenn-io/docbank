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

func (s *Store) PhotoOwner(ctx context.Context, id string) (PhotoOwner, error) {
	return photoOwnerByIDTx(ctx, s.db, id)
}

// PhotoOwners lists owners by enrollment.
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

func enrollPhotoOwnerTx(ctx context.Context, tx *sql.Tx, personID string) (PhotoOwner, error) {
	if _, err := tx.ExecContext(ctx, `INSERT INTO photo_owners(person_id,enrolled_at) VALUES(?,?)`, personID, nowRFC3339()); err != nil {
		return PhotoOwner{}, fmt.Errorf("enrolling photo owner: %w", err)
	}
	return photoOwnerByIDTx(ctx, tx, personID)
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
		if err := s.ensureOwnerFolderTx(ctx, tx, personID); err != nil {
			return err
		}
		var err error
		owner, err = enrollPhotoOwnerTx(ctx, tx, personID)
		return err
	})
	return owner, err
}

// ensureOwnerFolderTx creates /photos/{personID}, adopting any existing
// directory along the way.
func (s *Store) ensureOwnerFolderTx(ctx context.Context, tx *sql.Tx, personID string) error {
	dir, err := liveDirTx(ctx, tx, s.rootID)
	if err != nil {
		return err
	}
	for _, name := range []string{photoOwnerFoldersName, personID} {
		next, err := childByName(ctx, tx, dir.ID, name)
		switch {
		case err == nil:
			if !next.IsDir() {
				return fmt.Errorf("%q is a file: %w", name, ErrNotDir)
			}
			dir = next
		case errors.Is(err, ErrNotFound):
			if dir, err = s.mkdirTx(ctx, tx, dir.ID, name, nowRFC3339()); err != nil {
				return err
			}
		default:
			return err
		}
	}
	return nil
}

// ownerFolderHasContentTx reports whether a live or trashed node sits in
// personID's owner folder.
func ownerFolderHasContentTx(ctx context.Context, tx *sql.Tx, personID string) (bool, error) {
	var found bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM nodes pof JOIN nodes child
		ON child.parent_id=pof.id OR child.trash_parent=pof.id
		WHERE pof.name=? AND `+photoOwnerFolderSQL()+`)`, personID).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("checking photo owner folder: %w", err)
	}
	return found, nil
}

// RemovePhotoOwner ends an enrollment whose folder is empty. The folder
// stays as an ordinary folder.
func (s *Store) RemovePhotoOwner(ctx context.Context, personID string, revision int64) error {
	return s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		if _, err := photoOwnerByIDTx(ctx, tx, personID); err != nil {
			return err
		}
		if err := fencePersonTx(ctx, tx, personID, revision); err != nil {
			return err
		}
		if found, err := ownerFolderHasContentTx(ctx, tx, personID); err != nil {
			return err
		} else if found {
			return fmt.Errorf("person %s: %w", personID, ErrPhotoOwnerReferenced)
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
