package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type metadataSavedQuery struct {
	Type        string `json:"type"`
	ID          string `json:"saved_query_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Kind        string `json:"kind"`
	Payload     []byte `json:"payload" format:"byte"`
	Fingerprint string `json:"fingerprint"`
	Revision    int64  `json:"revision"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

func exportSavedQueries(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `SELECT
		id,name,description,kind,payload,fingerprint,revision,created_at,updated_at
		FROM saved_queries ORDER BY id`)
	if err != nil {
		return fmt.Errorf("exporting saved queries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := metadataSavedQuery{Type: metadataSavedQueryType}
		if err := rows.Scan(&record.ID, &record.Name, &record.Description, &record.Kind,
			&record.Payload, &record.Fingerprint, &record.Revision, &record.CreatedAt,
			&record.UpdatedAt); err != nil {
			return fmt.Errorf("scanning saved query metadata: %w", err)
		}
		if err := validateSavedQueryMetadataRecord(record); err != nil {
			return fmt.Errorf("validating saved query metadata for export: %w", err)
		}
		if err := write(record); err != nil {
			return err
		}
	}
	return rowsError("saved query", rows)
}

func importSavedQueryMetadata(
	ctx context.Context, tx *sql.Tx, rawRecord metadataSavedQuery,
) error {
	if err := validateSavedQueryMetadataRecord(rawRecord); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO saved_queries(
		id,name,description,kind,payload,fingerprint,revision,created_at,updated_at
	) VALUES(?,?,?,?,?,?,?,?,?)`, rawRecord.ID, rawRecord.Name, rawRecord.Description,
		rawRecord.Kind, rawRecord.Payload, rawRecord.Fingerprint, rawRecord.Revision,
		rawRecord.CreatedAt, rawRecord.UpdatedAt)
	return err
}

func validateSavedQueryMetadataRecord(record metadataSavedQuery) error {
	if record.Type != metadataSavedQueryType || record.Revision < 1 {
		return errors.New("invalid saved query record")
	}
	if err := validateUUIDv4(record.ID); err != nil {
		return fmt.Errorf("invalid saved query ID: %w", err)
	}
	name, err := normalizeSavedQueryName(record.Name)
	if err != nil {
		return err
	}
	if name != record.Name {
		return errors.New("saved query name is not canonical NFC")
	}
	if err := validateSavedQueryDescription(record.Description); err != nil {
		return err
	}
	canonical, fingerprint, err := canonicalizeSavedQueryPayload(record.Kind, record.Payload)
	if err != nil {
		return err
	}
	if !bytes.Equal(canonical, record.Payload) {
		return errors.New("saved query payload is not canonical")
	}
	if record.Fingerprint != fingerprint {
		return errors.New("saved query fingerprint does not match canonical payload")
	}
	if err := validateMetadataTime("saved query created_at", record.CreatedAt); err != nil {
		return err
	}
	if err := validateMetadataTime("saved query updated_at", record.UpdatedAt); err != nil {
		return err
	}
	if record.UpdatedAt < record.CreatedAt {
		return errors.New("saved query updated_at precedes created_at")
	}
	return nil
}
