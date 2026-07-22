package store

import (
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/docbank/internal/audit"
)

type replayedTagDeletion struct {
	tagID       string
	definition  audit.Record
	assignments map[uint64]audit.Record
	allNodes    []uint64
	candidates  []replayedAuditedTagCandidate
	digest      string
	changeCount uint64
}

func attachedDeltaContainsTagDelete(
	digest string, deltaRecords map[string]storedAuditRecord,
) (bool, error) {
	delta, ok := deltaRecords[digest]
	if !ok {
		return false, errors.New("tag definition change lacks its attached-metadata delta")
	}
	changes, err := auditRecordListField(delta.record, "changes")
	if err != nil {
		return false, err
	}
	for _, change := range changes {
		kind, err := auditTextField(change, "record_kind")
		if err != nil {
			return false, err
		}
		if kind != auditTagDefinitionKind {
			continue
		}
		_, hasPre, err := optionalNestedAuditRecord(change, auditPreField)
		if err != nil {
			return false, err
		}
		_, hasPost, err := optionalNestedAuditRecord(change, auditPostField)
		if err != nil {
			return false, err
		}
		return hasPre && !hasPost, nil
	}
	return false, nil
}

func (replay *auditedHistoryReplay) validateTagDeletionDelta(
	operationID, digest string, deltaRecords map[string]storedAuditRecord,
	usedDeltas map[string]bool,
) (replayedTagDeletion, error) {
	delta, ok := deltaRecords[digest]
	if !ok || usedDeltas[digest] {
		return replayedTagDeletion{}, errors.New(
			"tag deletion lacks one unique attached-metadata delta",
		)
	}
	if err := requireAuditUUID(delta.record, auditOperationIDField, operationID); err != nil {
		return replayedTagDeletion{}, err
	}
	changes, err := auditRecordListField(delta.record, "changes")
	if err != nil {
		return replayedTagDeletion{}, err
	}
	if len(changes) == 0 {
		return replayedTagDeletion{}, errors.New("tag deletion has no attached-metadata changes")
	}
	transition := replayedTagDeletion{
		assignments: make(map[uint64]audit.Record), digest: digest,
	}
	for _, change := range changes {
		transition.changeCount++
		if err := requireAuditAbsent(change, auditPostField); err != nil {
			return replayedTagDeletion{}, errors.New("tag deletion change post-state must be absent")
		}
		pre, err := auditNestedField(change, auditPreField)
		if err != nil {
			return replayedTagDeletion{}, err
		}
		kind, err := auditTextField(change, "record_kind")
		if err != nil || kind != pre.Kind {
			return replayedTagDeletion{}, errors.New("tag deletion change has inconsistent record kind")
		}
		identity, err := attachedAuditIdentity(pre)
		if err != nil {
			return replayedTagDeletion{}, err
		}
		storedIdentity, err := auditNestedField(change, "stable_identity")
		if err != nil || !auditRecordEqual(storedIdentity, identity) {
			return replayedTagDeletion{}, errors.New(
				"tag deletion change identity does not match its record",
			)
		}
		key, err := attachedAuditKey(pre)
		if err != nil {
			return replayedTagDeletion{}, err
		}
		current, exists := replay.attachments[key]
		if !exists || !auditRecordEqual(current, pre) {
			return replayedTagDeletion{}, errors.New(
				"tag deletion pre-state does not match replayed metadata",
			)
		}
		switch pre.Kind {
		case auditTagDefinitionKind:
			if transition.definition.Kind != "" {
				return replayedTagDeletion{}, errors.New("tag deletion repeats its definition tombstone")
			}
			if err := validateReplayedTagDefinition(pre); err != nil {
				return replayedTagDeletion{}, err
			}
			transition.definition = pre
			transition.tagID, err = auditUUIDField(pre, "tag_id")
			if err != nil {
				return replayedTagDeletion{}, err
			}
		case auditTagAssignmentKind:
			nodeID, err := auditUnsignedField(pre, metadataNodeIDField)
			if err != nil {
				return replayedTagDeletion{}, err
			}
			if _, exists := transition.assignments[nodeID]; exists {
				return replayedTagDeletion{}, fmt.Errorf(
					"tag deletion repeats assignment for node %d", nodeID,
				)
			}
			transition.assignments[nodeID] = pre
			transition.allNodes = append(transition.allNodes, nodeID)
		default:
			return replayedTagDeletion{}, fmt.Errorf(
				"tag deletion contains unsupported %s tombstone", pre.Kind,
			)
		}
	}
	if transition.definition.Kind == "" {
		return replayedTagDeletion{}, errors.New("tag deletion lacks its definition tombstone")
	}
	slices.Sort(transition.allNodes)
	for _, nodeID := range transition.allNodes {
		assignmentTagID, err := auditUUIDField(transition.assignments[nodeID], "tag_id")
		if err != nil {
			return replayedTagDeletion{}, err
		}
		if assignmentTagID != transition.tagID {
			return replayedTagDeletion{}, errors.New(
				"tag deletion includes an assignment for another definition",
			)
		}
	}
	expectedNodes, err := replay.tagAssignmentNodes(transition.tagID)
	if err != nil {
		return replayedTagDeletion{}, err
	}
	if !slices.Equal(transition.allNodes, expectedNodes) {
		return replayedTagDeletion{}, errors.New(
			"tag deletion tombstones do not match the complete assignment set",
		)
	}
	usedDeltas[digest] = true
	return transition, nil
}

func (replay *auditedHistoryReplay) tagAssignmentNodes(tagID string) ([]uint64, error) {
	var result []uint64
	for _, record := range replay.attachments {
		if record.Kind != auditTagAssignmentKind {
			continue
		}
		currentTagID, err := auditUUIDField(record, "tag_id")
		if err != nil {
			return nil, err
		}
		if currentTagID != tagID {
			continue
		}
		nodeID, err := auditUnsignedField(record, metadataNodeIDField)
		if err != nil {
			return nil, err
		}
		result = append(result, nodeID)
	}
	slices.Sort(result)
	return result, nil
}

func (replay *auditedHistoryReplay) applyTagDefinitionDelete(
	vaultID string, mutation, allocation storedAuditRecord,
	scopeIDs []string, scopeEntries map[string]storedAuditRecord,
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
	transition, err := replay.validateTagDeletionDelta(
		operationID, digest, deltaRecords, usedDeltas,
	)
	if err != nil {
		return err
	}
	transition.candidates, err = replay.auditedTagDefinitionCandidates(transition.tagID)
	if err != nil {
		return err
	}
	if len(transition.candidates) == 0 {
		return errors.New("tag deletion fabricates an audited mutation without affected members")
	}
	if err := requireAuditedTagScopeFanout("deletion", scopeIDs, transition.candidates); err != nil {
		return err
	}
	if err := replay.validateTagDeleteEvents(
		mutation.record, operationID, transition, eventRecords, usedEvents,
	); err != nil {
		return err
	}
	if err := replay.validateMemberStateChanges(
		mutation.record, auditedTagCandidateNodeIDs(transition.candidates),
	); err != nil {
		return err
	}
	bindings, err := auditRecordListField(mutation.record, "baselines")
	if err != nil {
		return err
	}
	if len(bindings) != 0 {
		return errors.New("tag deletion cannot bind an enrollment baseline")
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
		mutation.record, auditAttachedMetadataChangeCountField, transition.changeCount,
	); err != nil {
		return err
	}
	if err := requireAuditDigest(
		mutation.record, "attached_metadata_change_digest", transition.digest,
	); err != nil {
		return err
	}
	if err := replay.advanceScopes(vaultID, mutation, scopeIDs, scopeEntries); err != nil {
		return err
	}
	if err := replay.advanceAllocation(
		vaultID, operationID, mutation, allocation, transition.digest, transition.changeCount,
	); err != nil {
		return err
	}
	return replay.applyTagDeletionState(transition, &mutation.record)
}

func (replay *auditedHistoryReplay) validateTagDeleteEvents(
	mutation audit.Record, operationID string, transition replayedTagDeletion,
	eventRecords map[string]storedAuditRecord, usedEvents map[string]bool,
) error {
	events, err := auditRecordListField(mutation, "events")
	if err != nil {
		return err
	}
	if len(events) != len(transition.candidates)*2 {
		return errors.New("tag deletion event set does not match assigned audited nodes")
	}
	ordinal := uint64(0)
	for _, candidate := range transition.candidates {
		state := replay.states[candidate.nodeID]
		revision, err := auditUnsignedField(state, "node_revision")
		if err != nil {
			return err
		}
		current, err := auditOptionalUUIDField(state, "current_version_id")
		if err != nil {
			return err
		}
		expected := []struct {
			kind       string
			attachment audit.Record
		}{
			{"tag_delete", transition.definition},
			{"tag_unassign", transition.assignments[candidate.nodeID]},
		}
		for _, item := range expected {
			event := events[ordinal]
			if err := validateAuditEventWrapper(
				operationID, ordinal, event, eventRecords, usedEvents,
			); err != nil {
				return err
			}
			if err := validateTagDeleteEvent(
				mutation, event, operationID, ordinal, candidate.nodeID, revision, current,
				candidate.scopeID, item.kind, item.attachment,
			); err != nil {
				return err
			}
			ordinal++
		}
	}
	return nil
}

func validateTagDeleteEvent(
	mutation, event audit.Record, operationID string, ordinal, nodeID, revision uint64,
	current *string, scopeID, kind string, attachment audit.Record,
) error {
	identity, err := attachedAuditIdentity(attachment)
	if err != nil {
		return err
	}
	storedIdentity, err := auditNestedField(event, "attachment_identity")
	if err != nil || !auditRecordEqual(storedIdentity, identity) {
		return fmt.Errorf("%s event identity does not match its attachment", kind)
	}
	checks := []func() error{
		func() error { return requireAuditUUID(event, auditOperationIDField, operationID) },
		func() error { return requireAuditUnsigned(event, metadataNodeIDField, nodeID) },
		func() error { return requireAuditText(event, "event_kind", kind) },
		func() error { return requireAuditUUID(event, auditScopeIDField, scopeID) },
		func() error { return requireAuditText(event, "attachment_kind", attachment.Kind) },
		func() error { return requireAuditUnsigned(event, auditEventOrdinalField, ordinal) },
		func() error { return requireAuditUnsigned(event, "prior_node_revision", revision) },
		func() error { return requireAuditUnsigned(event, "resulting_node_revision", revision+1) },
		func() error { return requireAuditOptionalUUID(event, "prior_current_version_id", current) },
		func() error { return requireAuditOptionalUUID(event, "resulting_current_version_id", current) },
		func() error { return requireMatchingEventEnvelope(mutation, event) },
		func() error {
			return requireAuditAbsentFields(
				event, auditTargetNodeIDField, auditSourceVersionIDField, auditTopologyDeltaField,
				auditBaselineDigestField, auditPostField,
			)
		},
	}
	for _, check := range checks {
		if err := check(); err != nil {
			return err
		}
	}
	pre, err := auditNestedField(event, auditPreField)
	if err != nil || !auditRecordEqual(pre, attachment) {
		return fmt.Errorf("%s event pre-state does not match its delta", kind)
	}
	return nil
}

func (replay *auditedHistoryReplay) applyTagDeletionState(
	transition replayedTagDeletion, mutation *audit.Record,
) error {
	definitionKey, err := attachedAuditKey(transition.definition)
	if err != nil {
		return err
	}
	delete(replay.attachments, definitionKey)
	for _, assignment := range transition.assignments {
		key, err := attachedAuditKey(assignment)
		if err != nil {
			return err
		}
		delete(replay.attachments, key)
	}
	return replay.applyTagAffectedNodeState(
		auditedTagCandidateNodeIDs(transition.candidates), mutation,
	)
}
