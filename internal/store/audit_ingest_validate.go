package store

import (
	"errors"
	"fmt"

	"go.kenn.io/docbank/internal/audit"
)

func (replay *auditedHistoryReplay) validateNodeCreationAttachedMetadata(
	mutation audit.Record, operationID string, creation *replayedNodeCreation,
	deltaRecords map[string]storedAuditRecord, usedDeltas map[string]bool,
) error {
	count, err := auditUnsignedField(mutation, auditAttachedMetadataChangeCountField)
	if err != nil {
		return err
	}
	if count == 0 {
		if err := requireAuditAbsentFields(
			mutation, "grouping_id", "attached_metadata_change_digest",
		); err != nil {
			return err
		}
		if len(creation.baselineAttachments) != 0 {
			return errors.New("direct node creation baseline cannot contain attached metadata")
		}
		return nil
	}
	if creation.version == nil || count > 2 {
		return errors.New("audited ingest has an invalid attached-metadata change count")
	}
	ingestID, err := auditUUIDField(mutation, "grouping_id")
	if err != nil {
		return fmt.Errorf("reading audited ingest grouping ID: %w", err)
	}
	digest, err := auditDigestField(mutation, "attached_metadata_change_digest")
	if err != nil {
		return err
	}
	delta, ok := deltaRecords[digest]
	if !ok || usedDeltas[digest] {
		return errors.New("audited ingest lacks one unique attached-metadata delta")
	}
	if err := requireAuditUUID(delta.record, auditOperationIDField, operationID); err != nil {
		return err
	}
	changes, err := auditRecordListField(delta.record, "changes")
	if err != nil {
		return err
	}
	if uint64(len(changes)) != count {
		return errors.New("audited ingest attachment count does not match its delta")
	}
	var ingestRecord, provenanceRecord *audit.Record
	for index := range changes {
		post, err := validateAuditedIngestAddition(changes[index])
		if err != nil {
			return err
		}
		key, err := attachedAuditKey(post)
		if err != nil {
			return err
		}
		if _, exists := replay.attachments[key]; exists {
			return fmt.Errorf("audited ingest reuses attached metadata identity %s", post.Kind)
		}
		switch post.Kind {
		case metadataIngestType:
			if ingestRecord != nil {
				return errors.New("audited ingest adds more than one ingest record")
			}
			ingestRecord = &post
		case metadataProvenanceType:
			if provenanceRecord != nil {
				return errors.New("audited ingest adds more than one provenance record")
			}
			provenanceRecord = &post
		default:
			return fmt.Errorf("audited ingest adds unsupported %s metadata", post.Kind)
		}
	}
	if provenanceRecord == nil {
		return errors.New("audited ingest lacks its provenance addition")
	}
	if err := validateAuditedIngestProvenance(*provenanceRecord, creation.childID, ingestID); err != nil {
		return err
	}
	if ingestRecord == nil {
		existing, err := replay.ingestAttachment(ingestID)
		if err != nil {
			return err
		}
		ingestRecord = &existing
	} else if err := requireAuditUUID(*ingestRecord, "ingest_id", ingestID); err != nil {
		return err
	}
	expectedBaseline := []audit.Record{*ingestRecord, *provenanceRecord}
	if err := sortAuditRecordsByCanonicalIdentity(expectedBaseline, attachedAuditIdentity); err != nil {
		return err
	}
	if !equalAuditRecordLists(expectedBaseline, creation.baselineAttachments) {
		return errors.New("audited ingest baseline does not match its attached metadata")
	}
	creation.attachmentChanges = changes
	creation.attachmentDigest = digest
	creation.provenance = provenanceRecord
	creation.ingestID = ingestID
	usedDeltas[digest] = true
	return nil
}

func validateAuditedIngestAddition(change audit.Record) (audit.Record, error) {
	if err := requireAuditAbsent(change, "pre"); err != nil {
		return audit.Record{}, errors.New("audited ingest attachment change is not an addition")
	}
	post, err := auditNestedField(change, "post")
	if err != nil {
		return audit.Record{}, err
	}
	kind, err := auditTextField(change, "record_kind")
	if err != nil {
		return audit.Record{}, err
	}
	if kind != post.Kind {
		return audit.Record{}, errors.New("audited ingest attachment kind does not match its post record")
	}
	identity, err := auditNestedField(change, "stable_identity")
	if err != nil {
		return audit.Record{}, err
	}
	wantIdentity, err := attachedAuditIdentity(post)
	if err != nil {
		return audit.Record{}, err
	}
	if !auditRecordEqual(identity, wantIdentity) {
		return audit.Record{}, errors.New("audited ingest attachment identity does not match its post record")
	}
	return post, nil
}

func validateAuditedIngestProvenance(record audit.Record, nodeID uint64, ingestID string) error {
	if err := requireAuditUnsigned(record, metadataNodeIDField, nodeID); err != nil {
		return err
	}
	if err := requireAuditUUID(record, "ingest_id", ingestID); err != nil {
		return err
	}
	return requireAuditAbsent(record, "supersedes")
}

func (replay *auditedHistoryReplay) ingestAttachment(ingestID string) (audit.Record, error) {
	for _, record := range replay.attachments {
		if record.Kind != metadataIngestType {
			continue
		}
		id, err := auditUUIDField(record, "ingest_id")
		if err != nil {
			return audit.Record{}, err
		}
		if id == ingestID {
			return record, nil
		}
	}
	return audit.Record{}, fmt.Errorf("audited ingest references missing run %s", ingestID)
}
