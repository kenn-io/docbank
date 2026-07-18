package store

import (
	"errors"
	"fmt"

	"go.kenn.io/docbank/internal/audit"
)

func (replay *auditedHistoryReplay) applyUnauditedTagCreation(
	vaultID string, allocation storedAuditRecord,
	deltaRecords map[string]storedAuditRecord, usedDeltas map[string]bool,
) error {
	operationID, err := auditUUIDField(allocation.record, auditOperationIDField)
	if err != nil {
		return err
	}
	nextCount, err := replay.validateAllocationBase(vaultID, operationID, allocation)
	if err != nil {
		return err
	}
	if err := requireAuditBool(allocation.record, "has_audited_mutation", false); err != nil {
		return err
	}
	if err := requireAuditAbsent(allocation.record, "mutation_hash"); err != nil {
		return err
	}
	if err := requireAuditBool(
		allocation.record, "has_attached_metadata_change", true,
	); err != nil {
		return err
	}
	if err := requireAuditUnsigned(
		allocation.record, auditAttachedMetadataChangeCountField, 1,
	); err != nil {
		return err
	}
	digest, err := auditDigestField(allocation.record, "attached_metadata_change_digest")
	if err != nil {
		return err
	}
	definition, err := replay.validateTagCreationDelta(
		operationID, digest, deltaRecords, usedDeltas,
	)
	if err != nil {
		return err
	}
	key, err := attachedAuditKey(definition)
	if err != nil {
		return err
	}
	replay.attachments[key] = definition
	replay.allocationCount, replay.allocationHead = nextCount, allocation.digest
	return nil
}

func (replay *auditedHistoryReplay) validateTagCreationDelta(
	operationID, digest string, deltaRecords map[string]storedAuditRecord,
	usedDeltas map[string]bool,
) (audit.Record, error) {
	delta, ok := deltaRecords[digest]
	if !ok || usedDeltas[digest] {
		return audit.Record{}, errors.New(
			"tag creation lacks one unique attached-metadata delta",
		)
	}
	if err := requireAuditUUID(delta.record, auditOperationIDField, operationID); err != nil {
		return audit.Record{}, err
	}
	changes, err := auditRecordListField(delta.record, "changes")
	if err != nil {
		return audit.Record{}, err
	}
	if len(changes) != 1 {
		return audit.Record{}, errors.New(
			"tag creation must contain exactly one attached-metadata change",
		)
	}
	change := changes[0]
	if err := requireAuditText(change, "record_kind", "tag_definition"); err != nil {
		return audit.Record{}, err
	}
	if err := requireAuditAbsent(change, auditPreField); err != nil {
		return audit.Record{}, errors.New("tag creation pre-state must be absent")
	}
	definition, err := auditNestedField(change, auditPostField)
	if err != nil {
		return audit.Record{}, err
	}
	if definition.Kind != "tag_definition" {
		return audit.Record{}, errors.New("tag creation post-state is not a tag definition")
	}
	identity, err := attachedAuditIdentity(definition)
	if err != nil {
		return audit.Record{}, err
	}
	storedIdentity, err := auditNestedField(change, "stable_identity")
	if err != nil {
		return audit.Record{}, err
	}
	if !auditRecordEqual(storedIdentity, identity) {
		return audit.Record{}, errors.New("tag creation identity does not match its definition")
	}
	tagID, err := auditUUIDField(definition, "tag_id")
	if err != nil {
		return audit.Record{}, err
	}
	name, err := auditTextField(definition, "name")
	if err != nil {
		return audit.Record{}, err
	}
	normalized, err := NormalizeTagName(name)
	if err != nil {
		return audit.Record{}, fmt.Errorf("tag %s has an invalid name: %w", tagID, err)
	}
	if normalized != name {
		return audit.Record{}, fmt.Errorf("tag %s has an invalid canonical name", tagID)
	}
	key, err := attachedAuditKey(definition)
	if err != nil {
		return audit.Record{}, err
	}
	if _, exists := replay.attachments[key]; exists {
		return audit.Record{}, fmt.Errorf("tag creation reuses definition identity %s", tagID)
	}
	usedDeltas[digest] = true
	return definition, nil
}
