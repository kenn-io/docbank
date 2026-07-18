package store

import (
	"errors"
	"fmt"

	"go.kenn.io/docbank/internal/audit"
)

type replayedTagAssignment struct {
	nodeID     uint64
	assignment audit.Record
	change     audit.Record
	digest     string
	assign     bool
}

func (replay *auditedHistoryReplay) applyTagAssignment(
	vaultID string, mutation, allocation, scopeEntry storedAuditRecord,
	deltaRecords, eventRecords map[string]storedAuditRecord,
	usedDeltas, usedEvents map[string]bool,
) error {
	operationID, err := auditUUIDField(mutation.record, auditOperationIDField)
	if err != nil {
		return err
	}
	nextSequence := replay.allocationCount + 1
	auditSequence, err := positiveAuditInteger("operation sequence", nextSequence)
	if err != nil {
		return err
	}
	if err := requireAuditUUID(mutation.record, auditVaultIDField, vaultID); err != nil {
		return err
	}
	if err := requireAuditUnsigned(
		mutation.record, "operation_sequence", auditSequence,
	); err != nil {
		return err
	}
	if err := requireAuditAbsent(mutation.record, "grouping_id"); err != nil {
		return err
	}
	transition, err := replay.validateTagAssignmentDelta(
		mutation.record, operationID, deltaRecords, usedDeltas,
	)
	if err != nil {
		return err
	}
	if err := replay.validateTagAssignmentEvent(
		mutation.record, operationID, transition, eventRecords, usedEvents,
	); err != nil {
		return err
	}
	if err := replay.validateMemberStateChanges(
		mutation.record, []uint64{transition.nodeID},
	); err != nil {
		return err
	}
	bindings, err := auditRecordListField(mutation.record, "baselines")
	if err != nil {
		return err
	}
	if len(bindings) != 0 {
		return errors.New("tag assignment cannot bind an enrollment baseline")
	}
	if err := requireAuditAbsentFields(
		mutation.record, auditTopologyDeltaField, "path_effect_digest",
		"witness_change_digest",
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
		vaultID, operationID, mutation, allocation, transition.digest,
	); err != nil {
		return err
	}
	return replay.applyTagAssignmentState(transition, mutation.record)
}

func (replay *auditedHistoryReplay) validateTagAssignmentDelta(
	mutation audit.Record, operationID string,
	deltaRecords map[string]storedAuditRecord, usedDeltas map[string]bool,
) (replayedTagAssignment, error) {
	digest, err := auditDigestField(mutation, "attached_metadata_change_digest")
	if err != nil {
		return replayedTagAssignment{}, err
	}
	delta, ok := deltaRecords[digest]
	if !ok || usedDeltas[digest] {
		return replayedTagAssignment{}, errors.New(
			"tag assignment lacks one unique attached-metadata delta",
		)
	}
	if err := requireAuditUUID(delta.record, auditOperationIDField, operationID); err != nil {
		return replayedTagAssignment{}, err
	}
	changes, err := auditRecordListField(delta.record, "changes")
	if err != nil {
		return replayedTagAssignment{}, err
	}
	if len(changes) != 1 {
		return replayedTagAssignment{}, errors.New(
			"tag assignment must contain exactly one attached-metadata change",
		)
	}
	change := changes[0]
	if err := requireAuditText(change, "record_kind", "tag_assignment"); err != nil {
		return replayedTagAssignment{}, err
	}
	pre, hasPre, preErr := optionalNestedAuditRecord(change, auditPreField)
	post, hasPost, postErr := optionalNestedAuditRecord(change, auditPostField)
	if preErr != nil {
		return replayedTagAssignment{}, preErr
	}
	if postErr != nil {
		return replayedTagAssignment{}, postErr
	}
	assign := !hasPre && hasPost
	if !assign && (!hasPre || hasPost) {
		return replayedTagAssignment{}, errors.New(
			"tag assignment delta is neither an assignment nor an unassignment",
		)
	}
	assignment := post
	if !hasPost {
		assignment = pre
	}
	if assignment.Kind != "tag_assignment" {
		return replayedTagAssignment{}, errors.New(
			"tag assignment delta carries the wrong record kind",
		)
	}
	identity, err := attachedAuditIdentity(assignment)
	if err != nil {
		return replayedTagAssignment{}, err
	}
	storedIdentity, err := auditNestedField(change, "stable_identity")
	if err != nil || !auditRecordEqual(storedIdentity, identity) {
		return replayedTagAssignment{}, errors.New(
			"tag assignment delta identity does not match its record",
		)
	}
	nodeID, err := auditUnsignedField(assignment, metadataNodeIDField)
	if err != nil || !replay.memberSet[nodeID] {
		return replayedTagAssignment{}, fmt.Errorf(
			"tag assignment targets unaudited node %d", nodeID,
		)
	}
	tagID, err := auditUUIDField(assignment, "tag_id")
	if err != nil {
		return replayedTagAssignment{}, err
	}
	hasDefinition, err := replay.hasTagDefinition(tagID)
	if err != nil {
		return replayedTagAssignment{}, err
	}
	if !hasDefinition {
		return replayedTagAssignment{}, fmt.Errorf(
			"tag assignment references missing tag %s", tagID,
		)
	}
	key, err := attachedAuditKey(assignment)
	if err != nil {
		return replayedTagAssignment{}, err
	}
	current, exists := replay.attachments[key]
	if assign && exists {
		return replayedTagAssignment{}, errors.New("tag assignment already exists in replay")
	}
	if !assign && (!exists || !auditRecordEqual(current, assignment)) {
		return replayedTagAssignment{}, errors.New(
			"tag unassignment does not match replayed metadata",
		)
	}
	usedDeltas[digest] = true
	return replayedTagAssignment{
		nodeID: nodeID, assignment: assignment, change: change,
		digest: digest, assign: assign,
	}, nil
}

func optionalNestedAuditRecord(record audit.Record, field string) (audit.Record, bool, error) {
	value, err := auditField(record, field)
	if err != nil {
		return audit.Record{}, false, err
	}
	if value.IsAbsent() {
		return audit.Record{}, false, nil
	}
	nested, ok := value.RecordValue()
	if !ok {
		return audit.Record{}, false, fmt.Errorf(
			"audit field %s.%s must be a record", record.Kind, field,
		)
	}
	return nested, true, nil
}

func (replay *auditedHistoryReplay) hasTagDefinition(tagID string) (bool, error) {
	for _, record := range replay.attachments {
		if record.Kind != "tag_definition" {
			continue
		}
		id, err := auditUUIDField(record, "tag_id")
		if err != nil {
			return false, err
		}
		if id == tagID {
			return true, nil
		}
	}
	return false, nil
}

func (replay *auditedHistoryReplay) validateTagAssignmentEvent(
	mutation audit.Record, operationID string, transition replayedTagAssignment,
	eventRecords map[string]storedAuditRecord, usedEvents map[string]bool,
) error {
	events, err := auditRecordListField(mutation, "events")
	if err != nil {
		return err
	}
	if len(events) != 1 {
		return errors.New("tag assignment mutation must contain one scope event")
	}
	event := events[0]
	if err := validateAuditEventWrapper(
		operationID, 0, event, eventRecords, usedEvents,
	); err != nil {
		return err
	}
	kind := "tag_assign"
	if !transition.assign {
		kind = "tag_unassign"
	}
	identity, err := attachedAuditIdentity(transition.assignment)
	if err != nil {
		return err
	}
	storedIdentity, err := auditNestedField(event, "attachment_identity")
	if err != nil || !auditRecordEqual(storedIdentity, identity) {
		return errors.New("tag assignment event identity does not match its attachment")
	}
	state := replay.states[transition.nodeID]
	revision, err := auditUnsignedField(state, "node_revision")
	if err != nil {
		return err
	}
	current, err := auditOptionalUUIDField(state, "current_version_id")
	if err != nil {
		return err
	}
	checks := []func() error{
		func() error { return requireAuditUUID(event, auditOperationIDField, operationID) },
		func() error { return requireAuditUnsigned(event, metadataNodeIDField, transition.nodeID) },
		func() error { return requireAuditText(event, "event_kind", kind) },
		func() error { return requireAuditUUID(event, auditScopeIDField, replay.scopeID) },
		func() error { return requireAuditText(event, "attachment_kind", "tag_assignment") },
		func() error { return requireAuditUnsigned(event, auditEventOrdinalField, 0) },
		func() error { return requireAuditUnsigned(event, "prior_node_revision", revision) },
		func() error { return requireAuditUnsigned(event, "resulting_node_revision", revision+1) },
		func() error { return requireAuditOptionalUUID(event, "prior_current_version_id", current) },
		func() error { return requireAuditOptionalUUID(event, "resulting_current_version_id", current) },
		func() error { return requireMatchingEventEnvelope(mutation, event) },
		func() error {
			return requireAuditAbsentFields(
				event, "target_node_id", "source_version_id", auditTopologyDeltaField, "baseline_digest",
			)
		},
	}
	for _, check := range checks {
		if err := check(); err != nil {
			return err
		}
	}
	for _, field := range []string{auditPreField, auditPostField} {
		want, err := auditField(transition.change, field)
		if err != nil {
			return err
		}
		got, err := auditField(event, field)
		if err != nil {
			return err
		}
		if want.IsAbsent() != got.IsAbsent() {
			return fmt.Errorf("tag assignment event %s side does not match its delta", field)
		}
		if want.IsAbsent() {
			continue
		}
		wantRecord, wantOK := want.RecordValue()
		gotRecord, gotOK := got.RecordValue()
		if !wantOK || !gotOK || !auditRecordEqual(wantRecord, gotRecord) {
			return fmt.Errorf("tag assignment event %s side does not match its delta", field)
		}
	}
	return nil
}

func (replay *auditedHistoryReplay) applyTagAssignmentState(
	transition replayedTagAssignment, mutation audit.Record,
) error {
	key, err := attachedAuditKey(transition.assignment)
	if err != nil {
		return err
	}
	if transition.assign {
		replay.attachments[key] = transition.assignment
	} else {
		delete(replay.attachments, key)
	}
	state := replay.states[transition.nodeID]
	revision, err := auditUnsignedField(state, "node_revision")
	if err != nil {
		return err
	}
	current, err := auditField(state, "current_version_id")
	if err != nil {
		return err
	}
	replay.states[transition.nodeID] = audit.Record{Kind: "member_state", Fields: []audit.Field{
		{Name: metadataNodeIDField, Value: audit.Unsigned(transition.nodeID)},
		{Name: "node_revision", Value: audit.Unsigned(revision + 1)},
		{Name: "current_version_id", Value: current},
	}}
	index, ok := replay.topologyIndex[transition.nodeID]
	if !ok {
		return fmt.Errorf("tagged audited node %d is absent from topology", transition.nodeID)
	}
	modifiedAt, err := auditField(mutation, auditRecordedAtField)
	if err != nil {
		return err
	}
	replay.topology[index], err = replaceAuditRecordField(
		replay.topology[index], "modified_at", modifiedAt,
	)
	return err
}
