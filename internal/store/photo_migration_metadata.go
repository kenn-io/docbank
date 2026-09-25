package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"fmt"

	"go.kenn.io/docbank/internal/photomigration"
)

type metadataPhotoMigrationRun struct {
	Type           string `json:"type"`
	RunID          string `json:"run_id"`
	SourceKind     string `json:"source_kind"`
	SourceIdentity string `json:"source_identity"`
	CreatedAt      string `json:"created_at"`
	ReportJSON     []byte `json:"report_json" format:"byte"`
	OwnerMapJSON   []byte `json:"owner_map_json" format:"byte"`
}

type metadataPhotoMigrationMap struct {
	Type            string `json:"type"`
	RunID           string `json:"run_id"`
	SourceTable     string `json:"source_table"`
	SourceID        string `json:"source_id"`
	DestinationKind string `json:"destination_kind"`
	DestinationID   string `json:"destination_id"`
	Disposition     string `json:"disposition"`
}

func exportPhotoMigrationMetadata(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	rows, err := q.QueryContext(ctx, `SELECT run_id,source_kind,source_identity,created_at,report_json,owner_map_json FROM photo_migration_runs ORDER BY run_id`)
	if err != nil {
		return fmt.Errorf("exporting photo migration runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := metadataPhotoMigrationRun{Type: metadataPhotoMigrationRunType}
		if err := rows.Scan(&record.RunID, &record.SourceKind, &record.SourceIdentity, &record.CreatedAt, &record.ReportJSON, &record.OwnerMapJSON); err != nil {
			return err
		}
		if err := validatePhotoMigrationRunRecord(record); err != nil {
			return err
		}
		if err := write(record); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = q.QueryContext(ctx, `SELECT run_id,source_table,source_id,destination_kind,destination_id,disposition FROM photo_migration_map ORDER BY run_id,source_table,source_id`)
	if err != nil {
		return fmt.Errorf("exporting photo migration map: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := metadataPhotoMigrationMap{Type: metadataPhotoMigrationMapType}
		if err := rows.Scan(&record.RunID, &record.SourceTable, &record.SourceID, &record.DestinationKind, &record.DestinationID, &record.Disposition); err != nil {
			return err
		}
		if err := validatePhotoMigrationMapRecord(record); err != nil {
			return err
		}
		if err := write(record); err != nil {
			return err
		}
	}
	return rows.Err()
}

func validatePhotoMigrationRunRecord(record metadataPhotoMigrationRun) error {
	if record.Type != metadataPhotoMigrationRunType {
		return errors.New("invalid photo migration run record type")
	}
	report, err := photomigration.DecodeReport(record.ReportJSON)
	if err != nil {
		return err
	}
	template, err := photomigration.DecodeOwnerMapTemplate(record.OwnerMapJSON)
	if err != nil {
		return err
	}
	run := PhotoMigrationRun{ID: record.RunID, Source: photomigration.Source{Kind: record.SourceKind, Identity: record.SourceIdentity}, CreatedAt: record.CreatedAt, Report: report, OwnerMap: template}
	return validatePhotoMigrationRun(run)
}

func validatePhotoMigrationMapRecord(record metadataPhotoMigrationMap) error {
	if record.Type != metadataPhotoMigrationMapType {
		return errors.New("invalid photo migration map record type")
	}
	if err := validateUUIDv4(record.RunID); err != nil {
		return err
	}
	if record.SourceTable == "" || record.SourceID == "" || record.DestinationKind == "" || record.DestinationID == "" {
		return errors.New("photo migration map source and destination fields are required")
	}
	return ValidatePhotoMigrationDisposition(record.Disposition)
}

func importPhotoMigrationMetadataRecord(ctx context.Context, tx *sql.Tx, kind string, raw jsontext.Value) error {
	switch kind {
	case metadataPhotoMigrationRunType:
		var record metadataPhotoMigrationRun
		if err := decodeMetadataRecord(raw, &record); err != nil {
			return err
		}
		if err := validatePhotoMigrationRunRecord(record); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO photo_migration_runs(run_id,source_kind,source_identity,created_at,report_json,owner_map_json) VALUES(?,?,?,?,?,?)`, record.RunID, record.SourceKind, record.SourceIdentity, record.CreatedAt, record.ReportJSON, record.OwnerMapJSON)
		return err
	case metadataPhotoMigrationMapType:
		var record metadataPhotoMigrationMap
		if err := decodeMetadataRecord(raw, &record); err != nil {
			return err
		}
		if err := validatePhotoMigrationMapRecord(record); err != nil {
			return err
		}
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM photo_migration_runs WHERE run_id=?`, record.RunID).Scan(&exists); err != nil {
			return fmt.Errorf("photo migration map references unknown run: %w", err)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO photo_migration_map(run_id,source_table,source_id,destination_kind,destination_id,disposition) VALUES(?,?,?,?,?,?)`, record.RunID, record.SourceTable, record.SourceID, record.DestinationKind, record.DestinationID, record.Disposition)
		return err
	default:
		return fmt.Errorf("unknown photo migration metadata record %q", kind)
	}
}
