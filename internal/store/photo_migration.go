package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/docbank/internal/photomigration"
)

const (
	photoMigrationStorageVersion         = 26
	PhotoMigrationDispositionMigrated    = "migrated"
	PhotoMigrationDispositionQuarantined = "quarantined"
	PhotoMigrationDispositionRebuildable = "rebuildable"
)

type PhotoMigrationRun struct {
	ID        string
	Source    photomigration.Source
	CreatedAt string
	Report    photomigration.Report
	OwnerMap  photomigration.OwnerMapTemplate
}

type PhotoMigrationRunPage struct {
	Total int
	Items []PhotoMigrationRun
}

func ValidatePhotoMigrationDisposition(disposition string) error {
	switch disposition {
	case PhotoMigrationDispositionMigrated, PhotoMigrationDispositionQuarantined, PhotoMigrationDispositionRebuildable:
		return nil
	default:
		return fmt.Errorf("unsupported photo migration disposition %q", disposition)
	}
}

func (s *Store) SavePhotoMigrationRun(ctx context.Context, run PhotoMigrationRun) error {
	if err := validatePhotoMigrationRun(run); err != nil {
		return err
	}
	reportJSON, err := photomigration.EncodeReport(run.Report)
	if err != nil {
		return err
	}
	ownerJSON, err := photomigration.EncodeOwnerMapTemplate(run.OwnerMap)
	if err != nil {
		return err
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO photo_migration_runs(
			run_id,source_kind,source_identity,created_at,report_json,owner_map_json
		) VALUES(?,?,?,?,?,?)`, run.ID, run.Source.Kind, run.Source.Identity,
			run.CreatedAt, reportJSON, ownerJSON)
		if err != nil {
			return fmt.Errorf("saving photo migration run: %w", err)
		}
		return nil
	})
}

func (s *Store) ListPhotoMigrationRuns(ctx context.Context, offset, limit int) (PhotoMigrationRunPage, error) {
	if offset < 0 || limit < 1 || limit > 50 {
		return PhotoMigrationRunPage{}, errors.New("invalid photo migration run page")
	}
	var page PhotoMigrationRunPage
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_migration_runs`).Scan(&page.Total); err != nil {
		return page, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT run_id,source_kind,source_identity,created_at,report_json,owner_map_json
		FROM photo_migration_runs ORDER BY created_at DESC,run_id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return page, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		run, err := scanPhotoMigrationRun(rows)
		if err != nil {
			return PhotoMigrationRunPage{}, err
		}
		page.Items = append(page.Items, run)
	}
	return page, rows.Err()
}

func (s *Store) PhotoMigrationRun(ctx context.Context, id string) (PhotoMigrationRun, error) {
	if err := validateUUIDv4(id); err != nil {
		return PhotoMigrationRun{}, err
	}
	row := s.db.QueryRowContext(ctx, `SELECT run_id,source_kind,source_identity,created_at,report_json,owner_map_json
		FROM photo_migration_runs WHERE run_id=?`, id)
	run, err := scanPhotoMigrationRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return PhotoMigrationRun{}, ErrNotFound
	}
	return run, err
}

func validatePhotoMigrationRun(run PhotoMigrationRun) error {
	if err := validateUUIDv4(run.ID); err != nil {
		return fmt.Errorf("invalid migration run ID: %w", err)
	}
	if run.Source.Kind != photomigration.SourceInstall && run.Source.Kind != photomigration.SourceArchive {
		return errors.New("invalid migration source kind")
	}
	if run.Source.Identity == "" || strings.ContainsAny(run.Source.Identity, "/\\:\x00\r\n") {
		return errors.New("invalid migration source identity")
	}
	if run.CreatedAt == "" {
		return errors.New("migration run created_at is required")
	}
	if _, err := time.Parse(time.RFC3339Nano, run.CreatedAt); err != nil {
		return fmt.Errorf("invalid migration run created_at: %w", err)
	}
	if err := photomigration.ValidateReport(run.Report); err != nil {
		return err
	}
	if err := photomigration.ValidateOwnerMapTemplate(run.OwnerMap); err != nil {
		return err
	}
	if run.OwnerMap.Source != run.Source || run.Report.Source != run.Source {
		return errors.New("migration run source does not match report and owner map")
	}
	return nil
}

func scanPhotoMigrationRun(row scanner) (PhotoMigrationRun, error) {
	var run PhotoMigrationRun
	var kind, identity, created string
	var reportJSON, ownerJSON []byte
	if err := row.Scan(&run.ID, &kind, &identity, &created, &reportJSON, &ownerJSON); err != nil {
		return run, err
	}
	report, err := photomigration.DecodeReport(reportJSON)
	if err != nil {
		return PhotoMigrationRun{}, fmt.Errorf("decode saved migration report: %w", err)
	}
	owner, err := photomigration.DecodeOwnerMapTemplate(ownerJSON)
	if err != nil {
		return PhotoMigrationRun{}, fmt.Errorf("decode saved owner map: %w", err)
	}
	run.Source = photomigration.Source{Kind: kind, Identity: identity}
	run.CreatedAt, run.Report, run.OwnerMap = created, report, owner
	if err := validatePhotoMigrationRun(run); err != nil {
		return PhotoMigrationRun{}, err
	}
	return run, nil
}
