package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const metadataCollectionLabelType = "collection_label"

type metadataCollectionLabel struct {
	Type      string  `json:"type"`
	IngestID  string  `json:"ingest_id"`
	Label     *string `json:"label"`
	Revision  int64   `json:"revision"`
	UpdatedAt string  `json:"updated_at"`
}

func exportCollectionLabels(
	ctx context.Context, tx metadataQuerier, write metadataWrite,
) error {
	rows, err := tx.QueryContext(ctx, `SELECT ingest_id,label,revision,updated_at
		FROM collection_labels ORDER BY ingest_id`)
	if err != nil {
		return fmt.Errorf("exporting collection labels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := metadataCollectionLabel{Type: metadataCollectionLabelType}
		var label sql.NullString
		if err := rows.Scan(&record.IngestID, &label, &record.Revision, &record.UpdatedAt); err != nil {
			return fmt.Errorf("scanning collection label metadata: %w", err)
		}
		record.Label = stringPtr(label)
		if err := validateCollectionLabelRecord(record); err != nil {
			return fmt.Errorf("validating collection label metadata for export: %w", err)
		}
		if err := write(record); err != nil {
			return err
		}
	}
	return rowsError(metadataCollectionLabelType, rows)
}

func validateCollectionLabelRecord(record metadataCollectionLabel) error {
	if record.Type != metadataCollectionLabelType || record.Revision < 1 {
		return errors.New("invalid collection label record")
	}
	if record.Revision == 1 && record.Label == nil {
		return errors.New("collection label revision one must carry its initial label")
	}
	if err := validateUUIDv4(record.IngestID); err != nil {
		return fmt.Errorf("invalid collection label ingest ID: %w", err)
	}
	if record.Label != nil {
		normalized, err := normalizeLabelName(*record.Label, ErrInvalidCollectionLabel)
		if err != nil {
			return err
		}
		if normalized != *record.Label {
			return errors.New("collection label is not canonical NFC")
		}
	}
	return validateMetadataTime("collection label updated_at", record.UpdatedAt)
}

func importCollectionLabel(
	ctx context.Context, tx *sql.Tx, record metadataCollectionLabel,
) error {
	if err := validateCollectionLabelRecord(record); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO collection_labels(
		ingest_id,label,revision,updated_at
	) VALUES(?,?,?,?)`, record.IngestID, record.Label, record.Revision, record.UpdatedAt)
	return err
}

func validateCollectionLabelMetadataState(ctx context.Context, tx metadataQuerier) error {
	if err := exportCollectionLabels(ctx, tx, func(any) error { return nil }); err != nil {
		return err
	}
	checks := []struct {
		message string
		query   string
	}{
		{
			message: "collection label references a caller-supplied ingest",
			query: `SELECT EXISTS(SELECT 1 FROM collection_labels l
				JOIN ingests i ON i.id=l.ingest_id
				WHERE i.source_kind LIKE 'embedded:%')`,
		},
		{
			message: "collection label timestamp precedes ingest start",
			query: `SELECT EXISTS(SELECT 1 FROM collection_labels l
				JOIN ingests i ON i.id=l.ingest_id WHERE l.updated_at<i.started_at)`,
		},
		{
			message: "initial collection label timestamp differs from ingest start",
			query: `SELECT EXISTS(SELECT 1 FROM collection_labels l
				JOIN ingests i ON i.id=l.ingest_id
				WHERE l.revision=1 AND l.updated_at<>i.started_at)`,
		},
	}
	for _, check := range checks {
		var failed bool
		if err := tx.QueryRowContext(ctx, check.query).Scan(&failed); err != nil {
			return fmt.Errorf("validating metadata (%s): %w", check.message, err)
		}
		if failed {
			return errors.New(check.message)
		}
	}
	return nil
}
