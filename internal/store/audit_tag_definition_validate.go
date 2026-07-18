package store

import (
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/docbank/internal/audit"
)

const (
	tagDefinitionCreate = "create"
	tagDefinitionRename = "rename"
)

type replayedTagDefinitionChange struct {
	tagID      string
	kind       string
	pre        audit.Record
	post       audit.Record
	change     audit.Record
	digest     string
	candidates []uint64
}

func attachedMutationKind(
	mutation audit.Record, deltaRecords map[string]storedAuditRecord,
) (string, error) {
	digest, err := auditDigestField(mutation, "attached_metadata_change_digest")
	if err != nil {
		return "", err
	}
	delta, ok := deltaRecords[digest]
	if !ok {
		return "", errors.New("attached-metadata mutation lacks its delta")
	}
	changes, err := auditRecordListField(delta.record, "changes")
	if err != nil {
		return "", err
	}
	if len(changes) == 0 {
		return "", errors.New("attached-metadata mutation has no changes")
	}
	for _, change := range changes {
		kind, err := auditTextField(change, "record_kind")
		if err != nil {
			return "", err
		}
		if kind != "tag_definition" {
			continue
		}
		_, hasPre, err := optionalNestedAuditRecord(change, auditPreField)
		if err != nil {
			return "", err
		}
		_, hasPost, err := optionalNestedAuditRecord(change, auditPostField)
		if err != nil {
			return "", err
		}
		switch {
		case hasPre && !hasPost:
			return "tag_delete", nil
		case hasPre && hasPost:
			return "tag_rename", nil
		default:
			return "", errors.New("audited tag definition has an unsupported transition")
		}
	}
	if len(changes) == 1 {
		return auditTextField(changes[0], "record_kind")
	}
	return "", errors.New("attached-metadata mutation has unsupported mixed changes")
}

func (replay *auditedHistoryReplay) applyUnscopedTagDefinitionChange(
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
	digest, err := auditDigestField(allocation.record, "attached_metadata_change_digest")
	if err != nil {
		return err
	}
	deletion, err := attachedDeltaContainsTagDelete(digest, deltaRecords)
	if err != nil {
		return err
	}
	if deletion {
		transition, err := replay.validateTagDeletionDelta(
			operationID, digest, deltaRecords, usedDeltas,
		)
		if err != nil {
			return err
		}
		if len(transition.candidates) != 0 {
			return errors.New("tag deletion omits required audited effects")
		}
		if err := requireAuditUnsigned(
			allocation.record, auditAttachedMetadataChangeCountField, transition.changeCount,
		); err != nil {
			return err
		}
		if err := replay.applyTagDeletionState(transition, nil); err != nil {
			return err
		}
		replay.allocationCount, replay.allocationHead = nextCount, allocation.digest
		return nil
	}
	if err := requireAuditUnsigned(
		allocation.record, auditAttachedMetadataChangeCountField, 1,
	); err != nil {
		return err
	}
	transition, err := replay.validateTagDefinitionDelta(
		operationID, digest, deltaRecords, usedDeltas,
	)
	if err != nil {
		return err
	}
	if transition.kind != tagDefinitionCreate && transition.kind != tagDefinitionRename {
		return errors.New("unscoped tag-definition operation has an unsupported transition")
	}
	transition.candidates, err = replay.auditedTagDefinitionCandidates(transition.tagID)
	if err != nil {
		return err
	}
	if len(transition.candidates) != 0 {
		return errors.New("tag definition change omits required audited effects")
	}
	if err := replay.applyTagDefinitionState(transition, nil); err != nil {
		return err
	}
	replay.allocationCount, replay.allocationHead = nextCount, allocation.digest
	return nil
}

func (replay *auditedHistoryReplay) validateTagDefinitionDelta(
	operationID, digest string, deltaRecords map[string]storedAuditRecord,
	usedDeltas map[string]bool,
) (replayedTagDefinitionChange, error) {
	delta, ok := deltaRecords[digest]
	if !ok || usedDeltas[digest] {
		return replayedTagDefinitionChange{}, errors.New(
			"tag definition change lacks one unique attached-metadata delta",
		)
	}
	if err := requireAuditUUID(delta.record, auditOperationIDField, operationID); err != nil {
		return replayedTagDefinitionChange{}, err
	}
	changes, err := auditRecordListField(delta.record, "changes")
	if err != nil {
		return replayedTagDefinitionChange{}, err
	}
	if len(changes) != 1 {
		return replayedTagDefinitionChange{}, errors.New(
			"tag definition change must contain exactly one attached-metadata change",
		)
	}
	change := changes[0]
	if err := requireAuditText(change, "record_kind", "tag_definition"); err != nil {
		return replayedTagDefinitionChange{}, err
	}
	pre, hasPre, err := optionalNestedAuditRecord(change, auditPreField)
	if err != nil {
		return replayedTagDefinitionChange{}, err
	}
	post, hasPost, err := optionalNestedAuditRecord(change, auditPostField)
	if err != nil {
		return replayedTagDefinitionChange{}, err
	}
	transition := replayedTagDefinitionChange{change: change, digest: digest, pre: pre, post: post}
	switch {
	case !hasPre && hasPost:
		transition.kind = tagDefinitionCreate
	case hasPre && hasPost:
		transition.kind = tagDefinitionRename
	default:
		return replayedTagDefinitionChange{}, errors.New(
			"tag definition delta is neither a creation nor rename",
		)
	}
	record := post
	if err := validateReplayedTagDefinition(record); err != nil {
		return replayedTagDefinitionChange{}, err
	}
	identity, err := attachedAuditIdentity(record)
	if err != nil {
		return replayedTagDefinitionChange{}, err
	}
	storedIdentity, err := auditNestedField(change, "stable_identity")
	if err != nil || !auditRecordEqual(storedIdentity, identity) {
		return replayedTagDefinitionChange{}, errors.New(
			"tag definition identity does not match its record",
		)
	}
	transition.tagID, err = auditUUIDField(record, "tag_id")
	if err != nil {
		return replayedTagDefinitionChange{}, err
	}
	if err := replay.validateTagDefinitionNameAvailable(record, transition.tagID); err != nil {
		return replayedTagDefinitionChange{}, err
	}
	key, err := attachedAuditKey(record)
	if err != nil {
		return replayedTagDefinitionChange{}, err
	}
	current, exists := replay.attachments[key]
	if transition.kind == tagDefinitionCreate {
		if replay.tagDefinitionIDs[transition.tagID] {
			return replayedTagDefinitionChange{}, fmt.Errorf(
				"tag creation reuses definition identity %s", transition.tagID,
			)
		}
	} else {
		if err := validateReplayedTagDefinition(pre); err != nil {
			return replayedTagDefinitionChange{}, err
		}
		preIdentity, err := attachedAuditIdentity(pre)
		if err != nil || !auditRecordEqual(preIdentity, identity) {
			return replayedTagDefinitionChange{}, errors.New("tag rename changes stable identity")
		}
		if !exists || !auditRecordEqual(current, pre) {
			return replayedTagDefinitionChange{}, errors.New(
				"tag rename pre-state does not match replayed metadata",
			)
		}
		if auditRecordEqual(pre, post) {
			return replayedTagDefinitionChange{}, errors.New("tag rename does not change its definition")
		}
	}
	usedDeltas[digest] = true
	return transition, nil
}

func (replay *auditedHistoryReplay) validateTagDefinitionNameAvailable(
	definition audit.Record, tagID string,
) error {
	name, err := auditTextField(definition, "name")
	if err != nil {
		return err
	}
	for _, current := range replay.attachments {
		if current.Kind != "tag_definition" {
			continue
		}
		currentID, err := auditUUIDField(current, "tag_id")
		if err != nil {
			return err
		}
		if currentID == tagID {
			continue
		}
		currentName, err := auditTextField(current, "name")
		if err != nil {
			return err
		}
		if currentName == name {
			return fmt.Errorf("tag definition name %q already exists in replay", name)
		}
	}
	return nil
}

func validateReplayedTagDefinition(definition audit.Record) error {
	if definition.Kind != "tag_definition" {
		return errors.New("tag definition change carries the wrong record kind")
	}
	tagID, err := auditUUIDField(definition, "tag_id")
	if err != nil {
		return err
	}
	name, err := auditTextField(definition, "name")
	if err != nil {
		return err
	}
	normalized, err := NormalizeTagName(name)
	if err != nil {
		return fmt.Errorf("tag %s has an invalid name: %w", tagID, err)
	}
	if normalized != name {
		return fmt.Errorf("tag %s has an invalid canonical name", tagID)
	}
	return nil
}

func (replay *auditedHistoryReplay) auditedTagDefinitionCandidates(
	tagID string,
) ([]uint64, error) {
	seen := make(map[uint64]bool)
	for _, record := range replay.attachments {
		if record.Kind != "tag_assignment" {
			continue
		}
		candidateTagID, err := auditUUIDField(record, "tag_id")
		if err != nil {
			return nil, err
		}
		if candidateTagID != tagID {
			continue
		}
		nodeID, err := auditUnsignedField(record, metadataNodeIDField)
		if err != nil {
			return nil, err
		}
		if replay.memberSet[nodeID] {
			seen[nodeID] = true
		}
	}
	result := make([]uint64, 0, len(seen))
	for nodeID := range seen {
		result = append(result, nodeID)
	}
	slices.Sort(result)
	return result, nil
}

func (replay *auditedHistoryReplay) applyTagDefinitionRename(
	vaultID string, mutation, allocation, scopeEntry storedAuditRecord,
	deltaRecords, eventRecords map[string]storedAuditRecord,
	usedDeltas, usedEvents map[string]bool,
) error {
	operationID, err := auditUUIDField(mutation.record, auditOperationIDField)
	if err != nil {
		return err
	}
	auditSequence, err := positiveAuditInteger("operation sequence", replay.allocationCount+1)
	if err != nil {
		return err
	}
	if err := requireAuditUUID(mutation.record, auditVaultIDField, vaultID); err != nil {
		return err
	}
	if err := requireAuditUnsigned(mutation.record, "operation_sequence", auditSequence); err != nil {
		return err
	}
	if err := requireAuditAbsent(mutation.record, "grouping_id"); err != nil {
		return err
	}
	digest, err := auditDigestField(mutation.record, "attached_metadata_change_digest")
	if err != nil {
		return err
	}
	transition, err := replay.validateTagDefinitionDelta(
		operationID, digest, deltaRecords, usedDeltas,
	)
	if err != nil {
		return err
	}
	if transition.kind != tagDefinitionRename {
		return errors.New("audited tag-definition mutation is not a rename")
	}
	transition.candidates, err = replay.auditedTagDefinitionCandidates(transition.tagID)
	if err != nil {
		return err
	}
	if len(transition.candidates) == 0 {
		return errors.New("tag rename fabricates an audited mutation without affected members")
	}
	if err := replay.validateTagRenameEvents(
		mutation.record, operationID, transition, eventRecords, usedEvents,
	); err != nil {
		return err
	}
	if err := replay.validateMemberStateChanges(mutation.record, transition.candidates); err != nil {
		return err
	}
	bindings, err := auditRecordListField(mutation.record, "baselines")
	if err != nil {
		return err
	}
	if len(bindings) != 0 {
		return errors.New("tag rename cannot bind an enrollment baseline")
	}
	if err := requireAuditAbsentFields(
		mutation.record, auditTopologyDeltaField, "path_effect_digest", "witness_change_digest",
	); err != nil {
		return err
	}
	for _, field := range []string{"path_effect_count", auditWitnessChangeCountField} {
		if err := requireAuditUnsigned(mutation.record, field, 0); err != nil {
			return err
		}
	}
	if err := requireAuditUnsigned(
		mutation.record, auditAttachedMetadataChangeCountField, 1,
	); err != nil {
		return err
	}
	if err := requireAuditDigest(
		mutation.record, "attached_metadata_change_digest", transition.digest,
	); err != nil {
		return err
	}
	if err := replay.advanceScope(vaultID, mutation, scopeEntry); err != nil {
		return err
	}
	if err := replay.advanceAllocation(
		vaultID, operationID, mutation, allocation, transition.digest, 1,
	); err != nil {
		return err
	}
	return replay.applyTagDefinitionState(transition, &mutation.record)
}

func (replay *auditedHistoryReplay) validateTagRenameEvents(
	mutation audit.Record, operationID string, transition replayedTagDefinitionChange,
	eventRecords map[string]storedAuditRecord, usedEvents map[string]bool,
) error {
	events, err := auditRecordListField(mutation, "events")
	if err != nil {
		return err
	}
	if len(events) != len(transition.candidates) {
		return errors.New("tag rename event set does not match assigned audited nodes")
	}
	identity, err := attachedAuditIdentity(transition.pre)
	if err != nil {
		return err
	}
	ordinal := uint64(0)
	for index, nodeID := range transition.candidates {
		event := events[index]
		if err := validateAuditEventWrapper(
			operationID, ordinal, event, eventRecords, usedEvents,
		); err != nil {
			return err
		}
		state := replay.states[nodeID]
		revision, err := auditUnsignedField(state, "node_revision")
		if err != nil {
			return err
		}
		current, err := auditOptionalUUIDField(state, "current_version_id")
		if err != nil {
			return err
		}
		storedIdentity, err := auditNestedField(event, "attachment_identity")
		if err != nil || !auditRecordEqual(storedIdentity, identity) {
			return errors.New("tag rename event identity does not match its definition")
		}
		checks := []func() error{
			func() error { return requireAuditUUID(event, auditOperationIDField, operationID) },
			func() error { return requireAuditUnsigned(event, metadataNodeIDField, nodeID) },
			func() error { return requireAuditText(event, "event_kind", "tag_rename") },
			func() error { return requireAuditUUID(event, auditScopeIDField, replay.scopeID) },
			func() error { return requireAuditText(event, "attachment_kind", "tag_definition") },
			func() error { return requireAuditUnsigned(event, auditEventOrdinalField, ordinal) },
			func() error { return requireAuditUnsigned(event, "prior_node_revision", revision) },
			func() error { return requireAuditUnsigned(event, "resulting_node_revision", revision+1) },
			func() error { return requireAuditOptionalUUID(event, "prior_current_version_id", current) },
			func() error { return requireAuditOptionalUUID(event, "resulting_current_version_id", current) },
			func() error { return requireMatchingEventEnvelope(mutation, event) },
			func() error {
				return requireAuditAbsentFields(
					event, auditTargetNodeIDField, "source_version_id", auditTopologyDeltaField,
					"baseline_digest",
				)
			},
		}
		for _, check := range checks {
			if err := check(); err != nil {
				return err
			}
		}
		for _, side := range []struct {
			name string
			want audit.Record
		}{{auditPreField, transition.pre}, {auditPostField, transition.post}} {
			got, err := auditNestedField(event, side.name)
			if err != nil || !auditRecordEqual(got, side.want) {
				return fmt.Errorf("tag rename event %s does not match its delta", side.name)
			}
		}
		ordinal++
	}
	return nil
}

func (replay *auditedHistoryReplay) applyTagDefinitionState(
	transition replayedTagDefinitionChange, mutation *audit.Record,
) error {
	record := transition.post
	key, err := attachedAuditKey(record)
	if err != nil {
		return err
	}
	replay.attachments[key] = record
	replay.tagDefinitionIDs[transition.tagID] = true
	return replay.applyTagAffectedNodeState(transition.candidates, mutation)
}

func (replay *auditedHistoryReplay) applyTagAffectedNodeState(
	candidates []uint64, mutation *audit.Record,
) error {
	if len(candidates) == 0 {
		return nil
	}
	if mutation == nil {
		return errors.New("scoped tag-definition change lacks its mutation")
	}
	modifiedAt, err := auditField(*mutation, auditRecordedAtField)
	if err != nil {
		return err
	}
	for _, nodeID := range candidates {
		state := replay.states[nodeID]
		revision, err := auditUnsignedField(state, "node_revision")
		if err != nil {
			return err
		}
		current, err := auditField(state, "current_version_id")
		if err != nil {
			return err
		}
		replay.states[nodeID] = audit.Record{Kind: "member_state", Fields: []audit.Field{
			{Name: metadataNodeIDField, Value: audit.Unsigned(nodeID)},
			{Name: "node_revision", Value: audit.Unsigned(revision + 1)},
			{Name: "current_version_id", Value: current},
		}}
		index, ok := replay.topologyIndex[nodeID]
		if !ok {
			return fmt.Errorf("tagged audited node %d is absent from topology", nodeID)
		}
		replay.topology[index], err = replaceAuditRecordField(
			replay.topology[index], "modified_at", modifiedAt,
		)
		if err != nil {
			return err
		}
	}
	return nil
}
