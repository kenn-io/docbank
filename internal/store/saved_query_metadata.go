package store

import (
	"bytes"
	"errors"
	"fmt"
)

type metadataSavedQuery struct {
	Type        string `json:"type"`
	ID          string `json:"saved_query_id" db:"id"`
	Name        string `json:"name" db:"name"`
	Description string `json:"description" db:"description"`
	Kind        string `json:"kind" db:"kind"`
	Payload     []byte `json:"payload" format:"byte" db:"payload"`
	Fingerprint string `json:"fingerprint" db:"fingerprint"`
	Revision    int64  `json:"revision" db:"revision"`
	CreatedAt   string `json:"created_at" db:"created_at"`
	UpdatedAt   string `json:"updated_at" db:"updated_at"`
}

var savedQueryMetadata = newMetadataTable(metadataTable[metadataSavedQuery]{
	record: metadataSavedQuery{Type: metadataSavedQueryType}, table: "saved_queries",
	suffix: "ORDER BY id", validate: validateSavedQueryMetadataRecord, checkExport: true})

func validateSavedQueryMetadataRecord(record metadataSavedQuery) error {
	if record.Type != metadataSavedQueryType || record.Revision < 1 {
		return errors.New("invalid saved query record")
	}
	if err := validateUUIDv4(record.ID); err != nil {
		return fmt.Errorf("invalid saved query ID: %w", err)
	}
	name, err := normalizeLabelName(record.Name, ErrInvalidSavedQuery)
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
