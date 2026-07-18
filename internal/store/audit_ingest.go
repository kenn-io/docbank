package store

import (
	"database/sql"
	"fmt"

	"go.kenn.io/docbank/internal/audit"
)

// auditedCreationMetadata is the attached authority introduced by an audited
// file creation. A filesystem ingest adds one provenance fact and may also
// publish the ingest run that fact references.
type auditedCreationMetadata struct {
	groupingID      audit.Value
	baselineRecords []audit.Record
	changes         []audit.Record
	delta           audit.Record
	deltaDigest     auditRecordHash
	provenance      audit.Record
}

func auditedCreationBaselineAttachments(metadata *auditedCreationMetadata) []audit.Record {
	if metadata == nil {
		return nil
	}
	return metadata.baselineRecords
}

func makeAuditedIngestCreationMetadata(
	ingest metadataIngest, provenance metadataProvenance, ingestAdded bool,
	operationID string,
) (auditedCreationMetadata, error) {
	ingestRecord, err := ingestAuditRecord(ingest)
	if err != nil {
		return auditedCreationMetadata{}, err
	}
	provenanceRecord, err := provenanceAuditRecord(
		provenance.Identity, provenance.NodeID, provenance.IngestID,
		provenance.OriginalPath,
		nullString(provenance.OriginalMTime), nullString(provenance.Supersedes),
	)
	if err != nil {
		return auditedCreationMetadata{}, err
	}
	baseline := []audit.Record{ingestRecord, provenanceRecord}
	if err := sortAuditRecordsByCanonicalIdentity(baseline, attachedAuditIdentity); err != nil {
		return auditedCreationMetadata{}, fmt.Errorf("sorting audited ingest baseline metadata: %w", err)
	}
	changes := make([]audit.Record, 0, 2)
	if ingestAdded {
		change, err := makeAttachedMetadataAddition(ingestRecord)
		if err != nil {
			return auditedCreationMetadata{}, err
		}
		changes = append(changes, change)
	}
	provenanceChange, err := makeAttachedMetadataAddition(provenanceRecord)
	if err != nil {
		return auditedCreationMetadata{}, err
	}
	changes = append(changes, provenanceChange)
	operationValue, err := audit.UUID(operationID)
	if err != nil {
		return auditedCreationMetadata{}, err
	}
	delta := audit.Record{Kind: "attached_metadata_delta", Fields: []audit.Field{
		{Name: auditOperationIDField, Value: operationValue},
		{Name: "changes", Value: audit.List(auditNestedValues(changes)...)},
	}}
	digest, err := hashAuditRecord(delta)
	if err != nil {
		return auditedCreationMetadata{}, err
	}
	groupingID, err := audit.UUID(ingest.ID)
	if err != nil {
		return auditedCreationMetadata{}, err
	}
	return auditedCreationMetadata{
		groupingID: groupingID, baselineRecords: baseline, changes: changes,
		delta: delta, deltaDigest: digest, provenance: provenanceRecord,
	}, nil
}

func ingestAuditRecord(ingest metadataIngest) (audit.Record, error) {
	id, err := audit.UUID(ingest.ID)
	if err != nil {
		return audit.Record{}, err
	}
	startedAt, err := audit.Timestamp(ingest.StartedAt)
	if err != nil {
		return audit.Record{}, err
	}
	sourceKind, err := audit.Text(ingest.SourceKind)
	if err != nil {
		return audit.Record{}, err
	}
	return audit.Record{Kind: metadataIngestType, Fields: []audit.Field{
		{Name: "ingest_id", Value: id},
		{Name: "started_at", Value: startedAt},
		{Name: "source_kind", Value: sourceKind},
		{Name: "source_desc", Value: audit.Bytes([]byte(ingest.SourceDesc))},
	}}, nil
}

func makeAttachedMetadataAddition(record audit.Record) (audit.Record, error) {
	return makeAttachedMetadataPresenceChange(record, true)
}

func makeAttachedMetadataPresenceChange(
	record audit.Record, add bool,
) (audit.Record, error) {
	kind, err := audit.Text(record.Kind)
	if err != nil {
		return audit.Record{}, err
	}
	identity, err := attachedAuditIdentity(record)
	if err != nil {
		return audit.Record{}, err
	}
	pre, post := audit.Absent(), audit.Nested(record)
	if !add {
		pre, post = post, pre
	}
	return audit.Record{Kind: "attached_metadata_change", Fields: []audit.Field{
		{Name: "record_kind", Value: kind},
		{Name: "stable_identity", Value: audit.Nested(identity)},
		{Name: auditPreField, Value: pre},
		{Name: auditPostField, Value: post},
	}}, nil
}

func nullString(value *string) sql.NullString {
	if value == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *value, Valid: true}
}
