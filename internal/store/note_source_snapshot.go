package store

import (
	"context"
	"database/sql"
)

// NotePublicationSourceSnapshot binds an exact accepted source and current
// archive head to the database epoch observed in the same read transaction.
type NotePublicationSourceSnapshot struct {
	DatabaseEpoch string
	Source        NotePublicationSource
}

func (s *Store) NotePublicationSource(ctx context.Context, name, ref string) (NotePublicationSourceSnapshot, error) {
	var snapshot NotePublicationSourceSnapshot
	if err := ValidatePushName(name); err != nil {
		return snapshot, err
	}
	if err := ValidatePushRef(ref); err != nil {
		return snapshot, err
	}
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT database_epoch FROM note_publication_state WHERE singleton=1`).Scan(&snapshot.DatabaseEpoch); err != nil {
			return err
		}
		var err error
		snapshot.Source, err = s.publishedNoteSourceTx(ctx, tx, name, ref)
		return err
	})
	if err != nil {
		return NotePublicationSourceSnapshot{}, err
	}
	return snapshot, nil
}
