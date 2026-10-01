package store

import (
	"context"
	"errors"
	"fmt"
)

const metadataCollectionLabelType = "collection_label"

type metadataCollectionLabel struct {
	Type      string  `json:"type"`
	IngestID  string  `json:"ingest_id" db:"ingest_id"`
	Label     *string `json:"label" db:"label"`
	Revision  int64   `json:"revision" db:"revision"`
	UpdatedAt string  `json:"updated_at" db:"updated_at"`
}

var collectionLabelMetadata = newMetadataTable(metadataTable[metadataCollectionLabel]{
	record: metadataCollectionLabel{Type: metadataCollectionLabelType}, table: "collection_labels",
	suffix: "ORDER BY ingest_id", validate: validateCollectionLabelRecord, checkExport: true})

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

func validateCollectionLabelMetadataState(ctx context.Context, tx metadataQuerier) error {
	if err := collectionLabelMetadata.validateRows(ctx, tx); err != nil {
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
