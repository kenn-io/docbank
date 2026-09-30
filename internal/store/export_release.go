package store

import (
	"context"
	"database/sql"

	"go.kenn.io/docbank/document/bundle"
)

// ReleaseExportJob authorizes a terminal job before removing its archive.
// The caller excludes new archive leases until this transaction finishes.
// Archive removal must tolerate a retry after a rolled-back transaction.
func (s *Store) ReleaseExportJob(ctx context.Context, owner, id string, removeArchive func() error) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var actual string
		if err := tx.QueryRowContext(ctx, `SELECT owner FROM export_jobs WHERE id=?`, id).Scan(&actual); err != nil {
			if err == sql.ErrNoRows {
				return ErrNotFound
			}
			return err
		}
		if actual != owner {
			return ErrNotFound
		}
		job, err := loadExportJob(ctx, tx, id)
		if err != nil {
			return err
		}
		if job.State != "completed" && job.State != "canceled" && job.State != exportFailedState {
			return bundle.ErrConflict
		}
		if err := removeArchive(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM export_jobs WHERE id=?`, id); err != nil {
			return err
		}
		return reduceExportRetention(ctx, tx, job.PlanID)
	})
}
