package store

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/kit/packstore"
)

type metadataProvenanceVersionBinding struct {
	Type               string `json:"type"`
	ProvenanceIdentity string `json:"provenance_identity" db:"provenance_identity"`
	ContentVersionID   string `json:"content_version_id" db:"content_version_id"`
	ObservedAt         string `json:"observed_at" db:"observed_at"`
	BasisRef           string `json:"basis_ref" db:"basis_ref"`
}

var provenanceVersionBindingMetadata = newMetadataTable(metadataTable[metadataProvenanceVersionBinding]{
	record: metadataProvenanceVersionBinding{Type: metadataProvenanceVersionBindingType},
	table:  "provenance_version_bindings", suffix: "ORDER BY provenance_identity,content_version_id",
	validate: validateProvenanceVersionBindingRecord, checkExport: true})

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
