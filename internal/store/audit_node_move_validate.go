package store

import (
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/docbank/internal/audit"
)

type replayedNodeMove struct {
	topologyDigest, pathEffectDigest string
	postTopology                     []audit.Record
	effects                          []audit.Record
	changedIDs                       []uint64
}

func (replay *auditedHistoryReplay) applyNodeMove(
	vaultID string, mutation, allocation, scopeEntry storedAuditRecord,
	topologyRecords, pathEffectRecords, eventRecords map[string]storedAuditRecord,
	usedTopology, usedPathEffects, usedEvents map[string]bool,
) error {
	operationID, err := auditUUIDField(mutation.record, auditOperationIDField)
	if err != nil {
		return err
	}
	if err := requireAuditUUID(mutation.record, auditVaultIDField, vaultID); err != nil {
		return err
	}
	nextSequence, err := positiveAuditInteger(
		"operation sequence", replay.allocationCount+1,
	)
	if err != nil {
		return err
	}
	if err := requireAuditUnsigned(
		mutation.record, "operation_sequence", nextSequence,
	); err != nil {
		return err
	}
	if err := requireAuditAbsent(mutation.record, "grouping_id"); err != nil {
		return err
	}
	bindings, err := auditRecordListField(mutation.record, "baselines")
	if err != nil {
		return err
	}
	if len(bindings) != 0 {
		return errors.New("in-scope move cannot bind an enrollment baseline")
	}
	move, err := replay.validateNodeMoveTopology(
		mutation.record, operationID, topologyRecords, usedTopology,
	)
	if err != nil {
		return err
	}
	if err := replay.validateNodeMovePathEffects(
		mutation.record, operationID, &move, pathEffectRecords, usedPathEffects,
	); err != nil {
		return err
	}
	if err := replay.validateNodeMoveStateChanges(mutation.record, move.changedIDs); err != nil {
		return err
	}
	if err := replay.validateNodeMoveEvents(
		mutation.record, operationID, move, eventRecords, usedEvents,
	); err != nil {
		return err
	}
	if err := requireAuditAbsentFields(
		mutation.record, "witness_change_digest", "attached_metadata_change_digest",
	); err != nil {
		return err
	}
	if err := requireAuditUnsigned(mutation.record, auditWitnessChangeCountField, 0); err != nil {
		return err
	}
	if err := requireAuditUnsigned(mutation.record, auditAttachedMetadataChangeCountField, 0); err != nil {
		return err
	}
	if err := replay.advanceScope(vaultID, mutation, scopeEntry); err != nil {
		return err
	}
	if err := replay.advanceNodeMoveAllocation(
		vaultID, operationID, mutation, allocation, move,
	); err != nil {
		return err
	}
	return replay.applyNodeMoveState(move)
}

func (replay *auditedHistoryReplay) validateNodeMoveTopology(
	mutation audit.Record, operationID string,
	topologyRecords map[string]storedAuditRecord, usedTopology map[string]bool,
) (replayedNodeMove, error) {
	digest, err := auditDigestField(mutation, auditTopologyDeltaField)
	if err != nil {
		return replayedNodeMove{}, err
	}
	delta, ok := topologyRecords[digest]
	if !ok || usedTopology[digest] {
		return replayedNodeMove{}, errors.New("node move lacks one unique topology delta")
	}
	if err := requireAuditUUID(delta.record, auditOperationIDField, operationID); err != nil {
		return replayedNodeMove{}, err
	}
	changes, err := auditRecordListField(delta.record, "changes")
	if err != nil {
		return replayedNodeMove{}, err
	}
	if len(changes) < 2 || len(changes) > 3 {
		return replayedNodeMove{}, errors.New("in-scope move topology must change two or three nodes")
	}
	postTopology := slices.Clone(replay.topology)
	changedIDs := make([]uint64, 0, len(changes))
	var movedID, oldParentID, newParentID uint64
	for _, change := range changes {
		nodeID, err := auditUnsignedField(change, metadataNodeIDField)
		if err != nil {
			return replayedNodeMove{}, err
		}
		if !replay.memberSet[nodeID] {
			return replayedNodeMove{}, fmt.Errorf("in-scope move changes unaudited node %d", nodeID)
		}
		pre, preErr := optionalAuditNestedField(change, "pre")
		post, postErr := optionalAuditNestedField(change, "post")
		if preErr != nil || postErr != nil || pre == nil || post == nil {
			return replayedNodeMove{}, errors.New("in-scope move requires complete topology sides")
		}
		index, ok := replay.topologyIndex[nodeID]
		if !ok || !auditRecordEqual(replay.topology[index], *pre) {
			return replayedNodeMove{}, fmt.Errorf("node move pre-state for %d does not match replay", nodeID)
		}
		pathChanged, priorParent, resultingParent, err := validateNodeMoveTopologyChange(
			mutation, *pre, *post,
		)
		if err != nil {
			return replayedNodeMove{}, err
		}
		if pathChanged {
			if movedID != 0 {
				return replayedNodeMove{}, errors.New("node move topology changes multiple paths directly")
			}
			movedID, oldParentID, newParentID = nodeID, priorParent, resultingParent
		}
		postTopology[index] = *post
		changedIDs = append(changedIDs, nodeID)
	}
	if movedID == 0 || !slices.Contains(changedIDs, oldParentID) ||
		!slices.Contains(changedIDs, newParentID) {
		return replayedNodeMove{}, errors.New("node move topology lacks its changed parent state")
	}
	wantChanges := 2
	if oldParentID != newParentID {
		wantChanges = 3
	}
	if len(changes) != wantChanges {
		return replayedNodeMove{}, errors.New("node move topology has an unexpected changed-node set")
	}
	if err := requireLiveDirectoryTopology(postTopology, oldParentID); err != nil {
		return replayedNodeMove{}, err
	}
	if err := requireLiveDirectoryTopology(postTopology, newParentID); err != nil {
		return replayedNodeMove{}, err
	}
	usedTopology[digest] = true
	return replayedNodeMove{
		topologyDigest: digest, postTopology: postTopology, changedIDs: changedIDs,
	}, nil
}

func validateNodeMoveTopologyChange(
	mutation, pre, post audit.Record,
) (bool, uint64, uint64, error) {
	preID, err := auditUnsignedField(pre, metadataNodeIDField)
	if err != nil {
		return false, 0, 0, err
	}
	if err := requireAuditUnsigned(post, metadataNodeIDField, preID); err != nil {
		return false, 0, 0, err
	}
	preParent, err := auditOptionalParentIDField(pre)
	if err != nil {
		return false, 0, 0, err
	}
	postParent, err := auditOptionalParentIDField(post)
	if err != nil {
		return false, 0, 0, err
	}
	preName, err := auditBytesField(pre, "name")
	if err != nil {
		return false, 0, 0, err
	}
	postName, err := auditBytesField(post, "name")
	if err != nil {
		return false, 0, 0, err
	}
	parentsEqual := (preParent == nil) == (postParent == nil) &&
		(preParent == nil || *preParent == *postParent)
	pathChanged := !parentsEqual || !slices.Equal(preName, postName)
	if pathChanged && (preParent == nil || postParent == nil) {
		return false, 0, 0, errors.New("node move cannot add or remove the vault root")
	}
	modifiedAt, err := auditField(mutation, auditRecordedAtField)
	if err != nil {
		return false, 0, 0, err
	}
	expected, err := replaceAuditRecordField(pre, "modified_at", modifiedAt)
	if err != nil {
		return false, 0, 0, err
	}
	if pathChanged {
		expected, err = replaceAuditRecordField(expected, "parent_id", audit.Unsigned(*postParent))
		if err != nil {
			return false, 0, 0, err
		}
		expected, err = replaceAuditRecordField(expected, "name", audit.Bytes(postName))
		if err != nil {
			return false, 0, 0, err
		}
	}
	if !auditRecordEqual(expected, post) {
		return false, 0, 0, fmt.Errorf("node move post-state for %d has unsupported changes", preID)
	}
	if !pathChanged {
		return false, 0, 0, nil
	}
	return true, *preParent, *postParent, nil
}

func auditBytesField(record audit.Record, name string) ([]byte, error) {
	value, err := auditField(record, name)
	if err != nil {
		return nil, err
	}
	result, ok := value.BytesValue()
	if !ok {
		return nil, fmt.Errorf("audit field %s.%s is not bytes", record.Kind, name)
	}
	return result, nil
}

func requireLiveDirectoryTopology(topology []audit.Record, nodeID uint64) error {
	for _, node := range topology {
		id, err := auditUnsignedField(node, metadataNodeIDField)
		if err != nil {
			return err
		}
		if id != nodeID {
			continue
		}
		if err := requireAuditText(node, "node_kind", "dir"); err != nil {
			return err
		}
		return requireAuditText(node, "state", "live")
	}
	return fmt.Errorf("node move parent %d is absent from topology", nodeID)
}

func (replay *auditedHistoryReplay) validateNodeMovePathEffects(
	mutation audit.Record, operationID string, move *replayedNodeMove,
	pathEffectRecords map[string]storedAuditRecord, usedPathEffects map[string]bool,
) error {
	digest, err := auditDigestField(mutation, "path_effect_digest")
	if err != nil {
		return err
	}
	list, ok := pathEffectRecords[digest]
	if !ok || usedPathEffects[digest] {
		return errors.New("node move lacks one unique path-effect list")
	}
	if err := requireAuditUUID(list.record, auditOperationIDField, operationID); err != nil {
		return err
	}
	if err := requireAuditDigest(list.record, auditTopologyDeltaField, move.topologyDigest); err != nil {
		return err
	}
	stored, err := auditRecordListField(list.record, "effects")
	if err != nil {
		return err
	}
	expected, err := replay.deriveNodeMovePathEffects(move.postTopology)
	if err != nil {
		return err
	}
	if len(expected) == 0 || !equalAuditRecordLists(stored, expected) {
		return errors.New("node move path effects do not match replayed topology")
	}
	if err := requireAuditUnsigned(mutation, "path_effect_count", uint64(len(expected))); err != nil {
		return err
	}
	if err := requireAuditDigest(mutation, "path_effect_digest", digest); err != nil {
		return err
	}
	move.pathEffectDigest, move.effects = digest, expected
	usedPathEffects[digest] = true
	return nil
}

func (replay *auditedHistoryReplay) deriveNodeMovePathEffects(
	postTopology []audit.Record,
) ([]audit.Record, error) {
	scopeID, err := audit.UUID(replay.scopeID)
	if err != nil {
		return nil, err
	}
	live, err := audit.Text("live")
	if err != nil {
		return nil, err
	}
	memberIDs := slices.Clone(replay.members)
	slices.Sort(memberIDs)
	var effects []audit.Record
	for _, nodeID := range memberIDs {
		priorPath, priorLive, err := auditLivePath(replay.topology, nodeID)
		if err != nil {
			return nil, err
		}
		postPath, postLive, err := auditLivePath(postTopology, nodeID)
		if err != nil {
			return nil, err
		}
		if !priorLive || !postLive || slices.Equal(priorPath, postPath) {
			continue
		}
		effects = append(effects, audit.Record{Kind: "path_effect", Fields: []audit.Field{
			{Name: auditScopeIDField, Value: scopeID},
			{Name: "member_node_id", Value: audit.Unsigned(nodeID)},
			{Name: "old", Value: audit.Nested(audit.Record{Kind: "path_state", Fields: []audit.Field{
				{Name: "path", Value: audit.Bytes(priorPath)}, {Name: "state", Value: live},
			}})},
			{Name: "new", Value: audit.Nested(audit.Record{Kind: "path_state", Fields: []audit.Field{
				{Name: "path", Value: audit.Bytes(postPath)}, {Name: "state", Value: live},
			}})},
		}})
	}
	return effects, nil
}

func auditLivePath(topology []audit.Record, nodeID uint64) ([]byte, bool, error) {
	byID := make(map[uint64]audit.Record, len(topology))
	for _, node := range topology {
		id, err := auditUnsignedField(node, metadataNodeIDField)
		if err != nil {
			return nil, false, err
		}
		byID[id] = node
	}
	current, ok := byID[nodeID]
	if !ok {
		return nil, false, fmt.Errorf("audit topology lacks node %d", nodeID)
	}
	state, err := auditTextField(current, "state")
	if err != nil {
		return nil, false, err
	}
	if state != "live" {
		return nil, false, nil
	}
	visited := make(map[uint64]bool)
	var components [][]byte
	for {
		id, err := auditUnsignedField(current, metadataNodeIDField)
		if err != nil {
			return nil, false, err
		}
		if visited[id] {
			return nil, false, errors.New("audit topology contains a live-parent cycle")
		}
		visited[id] = true
		parentID, err := auditOptionalParentIDField(current)
		if err != nil {
			return nil, false, err
		}
		if parentID == nil {
			break
		}
		name, err := auditBytesField(current, "name")
		if err != nil {
			return nil, false, err
		}
		components = append(components, name)
		parent, exists := byID[*parentID]
		if !exists {
			return nil, false, fmt.Errorf("audit topology node %d has missing live parent %d", id, *parentID)
		}
		if err := requireAuditText(parent, "state", "live"); err != nil {
			return nil, false, err
		}
		current = parent
	}
	result := []byte{'/'}
	for _, v := range slices.Backward(components) {
		if len(result) > 1 {
			result = append(result, '/')
		}
		result = append(result, v...)
	}
	return result, true, nil
}

func (replay *auditedHistoryReplay) validateNodeMoveStateChanges(
	mutation audit.Record, changedIDs []uint64,
) error {
	changes, err := auditRecordListField(mutation, "member_state_changes")
	if err != nil {
		return err
	}
	if len(changes) != len(changedIDs) {
		return errors.New("node move member-state changes do not match changed topology")
	}
	for index, nodeID := range changedIDs {
		state := replay.states[nodeID]
		priorRevision, err := auditUnsignedField(state, "node_revision")
		if err != nil {
			return err
		}
		current, err := auditOptionalUUIDField(state, "current_version_id")
		if err != nil {
			return err
		}
		change := changes[index]
		checks := []func() error{
			func() error { return requireAuditUnsigned(change, metadataNodeIDField, nodeID) },
			func() error { return requireAuditUnsigned(change, "prior_revision", priorRevision) },
			func() error { return requireAuditUnsigned(change, "resulting_revision", priorRevision+1) },
			func() error { return requireAuditOptionalUUID(change, "prior_current_version_id", current) },
			func() error { return requireAuditOptionalUUID(change, "resulting_current_version_id", current) },
		}
		for _, check := range checks {
			if err := check(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (replay *auditedHistoryReplay) validateNodeMoveEvents(
	mutation audit.Record, operationID string, move replayedNodeMove,
	eventRecords map[string]storedAuditRecord, usedEvents map[string]bool,
) error {
	events, err := auditRecordListField(mutation, "events")
	if err != nil {
		return err
	}
	if len(events) != len(move.effects) {
		return errors.New("node move event set does not match its path effects")
	}
	changed := make(map[uint64]bool, len(move.changedIDs))
	for _, id := range move.changedIDs {
		changed[id] = true
	}
	ordinal := uint64(0)
	for index, event := range events {
		effect := move.effects[index]
		nodeID, err := auditUnsignedField(effect, "member_node_id")
		if err != nil {
			return err
		}
		if err := validateCreationEventWrapper(
			operationID, ordinal, event, eventRecords, usedEvents,
		); err != nil {
			return err
		}
		state := replay.states[nodeID]
		priorRevision, err := auditUnsignedField(state, "node_revision")
		if err != nil {
			return err
		}
		resultingRevision := priorRevision
		if changed[nodeID] {
			resultingRevision++
		}
		current, err := auditOptionalUUIDField(state, "current_version_id")
		if err != nil {
			return err
		}
		pre, err := auditNestedField(effect, "old")
		if err != nil {
			return err
		}
		post, err := auditNestedField(effect, "new")
		if err != nil {
			return err
		}
		checks := []func() error{
			func() error { return requireAuditUUID(event, auditOperationIDField, operationID) },
			func() error { return requireAuditUnsigned(event, metadataNodeIDField, nodeID) },
			func() error { return requireAuditText(event, "event_kind", "node_path") },
			func() error { return requireAuditUUID(event, auditScopeIDField, replay.scopeID) },
			func() error { return requireAuditUnsigned(event, auditEventOrdinalField, ordinal) },
			func() error { return requireAuditUnsigned(event, "prior_node_revision", priorRevision) },
			func() error { return requireAuditUnsigned(event, "resulting_node_revision", resultingRevision) },
			func() error { return requireAuditOptionalUUID(event, "prior_current_version_id", current) },
			func() error { return requireAuditOptionalUUID(event, "resulting_current_version_id", current) },
			func() error { return requireAuditDigest(event, auditTopologyDeltaField, move.topologyDigest) },
		}
		for _, check := range checks {
			if err := check(); err != nil {
				return err
			}
		}
		if err := requireMatchingEventEnvelope(mutation, event); err != nil {
			return err
		}
		ordinal++
		if err := requireAuditAbsentFields(
			event, "target_node_id", "attachment_kind", "attachment_identity",
			"source_version_id", "baseline_digest",
		); err != nil {
			return err
		}
		eventPre, err := auditNestedField(event, "pre")
		if err != nil || !auditRecordEqual(eventPre, pre) {
			return errors.New("node move event pre-state does not match its path effect")
		}
		eventPost, err := auditNestedField(event, "post")
		if err != nil || !auditRecordEqual(eventPost, post) {
			return errors.New("node move event post-state does not match its path effect")
		}
	}
	return nil
}

func (replay *auditedHistoryReplay) advanceNodeMoveAllocation(
	vaultID, operationID string, mutation, entry storedAuditRecord, move replayedNodeMove,
) error {
	nextCount := replay.allocationCount + 1
	auditCount, err := positiveAuditInteger("allocation entry count", nextCount)
	if err != nil {
		return err
	}
	checks := []func() error{
		func() error { return requireAuditUUID(entry.record, auditVaultIDField, vaultID) },
		func() error { return requireAuditUUID(entry.record, "lineage_id", replay.lineageID) },
		func() error { return requireAuditUUID(entry.record, auditOperationIDField, operationID) },
		func() error { return requireAuditDigest(entry.record, "previous_head", replay.allocationHead) },
		func() error { return requireAuditUnsigned(entry.record, "operation_sequence", auditCount) },
		func() error { return requireAuditUnsigned(entry.record, "operation_sequence_high_water", auditCount) },
		func() error { return requireAuditUnsigned(entry.record, "node_id_high_water", replay.nodeHighWater) },
		func() error { return requireAuditBool(entry.record, "has_audited_mutation", true) },
		func() error { return requireAuditBool(entry.record, "has_topology_change", true) },
		func() error { return requireAuditDigest(entry.record, auditTopologyDeltaField, move.topologyDigest) },
		func() error { return requireAuditBool(entry.record, "has_witness_change", false) },
		func() error { return requireAuditUnsigned(entry.record, auditWitnessChangeCountField, 0) },
		func() error { return requireAuditAbsent(entry.record, "witness_change_digest") },
		func() error { return requireAuditBool(entry.record, "has_attached_metadata_change", false) },
		func() error { return requireAuditUnsigned(entry.record, auditAttachedMetadataChangeCountField, 0) },
		func() error { return requireAuditAbsent(entry.record, "attached_metadata_change_digest") },
	}
	for _, check := range checks {
		if err := check(); err != nil {
			return err
		}
	}
	allocated, err := auditUnsignedListField(entry.record, "allocated_node_ids")
	if err != nil {
		return err
	}
	if len(allocated) != 0 {
		return errors.New("node move allocation cannot allocate node IDs")
	}
	mutationDigest, err := hashAuditRecord(mutation.record)
	if err != nil {
		return err
	}
	if err := requireAuditDigest(entry.record, "mutation_hash", mutationDigest.text); err != nil {
		return err
	}
	replay.allocationCount, replay.allocationHead = nextCount, entry.digest
	return nil
}

func (replay *auditedHistoryReplay) applyNodeMoveState(move replayedNodeMove) error {
	for _, nodeID := range move.changedIDs {
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
	}
	replay.topology = move.postTopology
	return nil
}
