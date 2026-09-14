package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/kit/packstore"
)

type metadataProvenanceVersionBinding struct {
	Type               string `json:"type"`
	ProvenanceIdentity string `json:"provenance_identity"`
	ContentVersionID   string `json:"content_version_id"`
	ObservedAt         string `json:"observed_at"`
	BasisRef           string `json:"basis_ref"`
}

func exportProvenanceVersionBindings(
	ctx context.Context, tx metadataQuerier, write metadataWrite,
) error {
	rows, err := tx.QueryContext(ctx, `SELECT provenance_identity,content_version_id,
		observed_at,basis_ref FROM provenance_version_bindings
		ORDER BY provenance_identity,content_version_id`)
	if err != nil {
		return fmt.Errorf("exporting provenance version bindings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := metadataProvenanceVersionBinding{Type: metadataProvenanceVersionBindingType}
		if err := rows.Scan(&record.ProvenanceIdentity, &record.ContentVersionID,
			&record.ObservedAt, &record.BasisRef); err != nil {
			return fmt.Errorf("scanning provenance version binding metadata: %w", err)
		}
		if err := validateProvenanceVersionBindingRecord(record); err != nil {
			return fmt.Errorf("validating provenance version binding metadata for export: %w", err)
		}
		if err := write(record); err != nil {
			return err
		}
	}
	return rowsError(metadataProvenanceVersionBindingType, rows)
}

func importProvenanceVersionBinding(
	ctx context.Context, tx *sql.Tx, record metadataProvenanceVersionBinding,
) error {
	if err := validateProvenanceVersionBindingRecord(record); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO provenance_version_bindings(
		provenance_identity,content_version_id,observed_at,basis_ref
	) VALUES(?,?,?,?)`, record.ProvenanceIdentity, record.ContentVersionID,
		record.ObservedAt, record.BasisRef)
	return err
}

func validateProvenanceVersionBindingRecord(record metadataProvenanceVersionBinding) error {
	if record.Type != metadataProvenanceVersionBindingType {
		return errors.New("invalid provenance version binding record")
	}
	if _, err := packstore.ParseHash(record.ProvenanceIdentity); err != nil {
		return fmt.Errorf("invalid provenance binding identity: %w", err)
	}
	if err := validateUUIDv4(record.ContentVersionID); err != nil {
		return fmt.Errorf("invalid provenance binding content version ID: %w", err)
	}
	if err := validateMetadataTime("provenance binding observed_at", record.ObservedAt); err != nil {
		return err
	}
	if record.BasisRef != provenanceVersionBindingBasis {
		return fmt.Errorf("invalid provenance binding basis %q", record.BasisRef)
	}
	return nil
}

func validateProvenanceVersionBindingRelations(ctx context.Context, tx metadataQuerier) error {
	var invalid bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM provenance_version_bindings b
		JOIN provenance p ON p.identity=b.provenance_identity
		JOIN content_versions cv ON cv.version_id=b.content_version_id
		WHERE p.node_id != cv.node_id
	)`).Scan(&invalid); err != nil {
		return fmt.Errorf("validating provenance version binding relations: %w", err)
	}
	if invalid {
		return errors.New("provenance binding and content version belong to different nodes")
	}
	return nil
}
