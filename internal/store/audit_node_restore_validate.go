package store

import (
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/docbank/internal/audit"
)

func (replay *auditedHistoryReplay) applyNodeRestore(
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
		return errors.New("in-scope restore cannot bind an enrollment baseline")
	}
	transition, restoredIDs, err := replay.validateNodeRestoreTopology(
		mutation.record, operationID, topologyRecords, usedTopology,
	)
	if err != nil {
		return err
	}
	expectedEffects, err := replay.deriveNodeRestorePathEffects(
		transition.postTopology, restoredIDs,
	)
	if err != nil {
		return err
	}
	if err := replay.validateTopologyPathEffects(
		mutation.record, operationID, &transition, expectedEffects,
		pathEffectRecords, usedPathEffects,
	); err != nil {
		return err
	}
	if err := replay.validateTopologyStateChanges(
		mutation.record, transition.changedIDs,
	); err != nil {
		return err
	}
	if err := replay.validateTopologyEvents(
		mutation.record, operationID, transition, eventRecords, usedEvents,
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
	if err := requireAuditUnsigned(
		mutation.record, auditAttachedMetadataChangeCountField, 0,
	); err != nil {
		return err
	}
	if err := replay.advanceScope(vaultID, mutation, scopeEntry); err != nil {
		return err
	}
	if err := replay.advanceTopologyAllocation(
		vaultID, operationID, mutation, allocation, transition,
	); err != nil {
		return err
	}
	return replay.applyTopologyState(transition)
}

func (replay *auditedHistoryReplay) validateNodeRestoreTopology(
	mutation audit.Record, operationID string,
	topologyRecords map[string]storedAuditRecord, usedTopology map[string]bool,
) (replayedTopologyMutation, []uint64, error) {
	digest, err := auditDigestField(mutation, auditTopologyDeltaField)
	if err != nil {
		return replayedTopologyMutation{}, nil, err
	}
	delta, ok := topologyRecords[digest]
	if !ok || usedTopology[digest] {
		return replayedTopologyMutation{}, nil, errors.New(
			"node restore lacks one unique topology delta",
		)
	}
	if err := requireAuditUUID(delta.record, auditOperationIDField, operationID); err != nil {
		return replayedTopologyMutation{}, nil, err
	}
	changes, err := auditRecordListField(delta.record, "changes")
	if err != nil {
		return replayedTopologyMutation{}, nil, err
	}
	if len(changes) < 2 {
		return replayedTopologyMutation{}, nil, errors.New(
			"in-scope restore topology must change a subtree and its destination parent",
		)
	}
	changeByID := make(map[uint64]audit.Record, len(changes))
	restoredSet := make(map[uint64]bool, len(changes)-1)
	storedIDs := make([]uint64, 0, len(changes))
	for _, change := range changes {
		nodeID, err := auditUnsignedField(change, metadataNodeIDField)
		if err != nil {
			return replayedTopologyMutation{}, nil, err
		}
		if _, exists := changeByID[nodeID]; exists {
			return replayedTopologyMutation{}, nil, fmt.Errorf(
				"node restore repeats topology change for %d", nodeID,
			)
		}
		pre, preErr := auditNestedField(change, "pre")
		post, postErr := auditNestedField(change, "post")
		if preErr != nil || postErr != nil {
			return replayedTopologyMutation{}, nil, errors.New(
				"in-scope restore requires complete topology sides",
			)
		}
		preState, err := auditTextField(pre, auditStateField)
		if err != nil {
			return replayedTopologyMutation{}, nil, err
		}
		postState, err := auditTextField(post, auditStateField)
		if err != nil {
			return replayedTopologyMutation{}, nil, err
		}
		if preState == auditNodeStateTrash && postState == auditNodeStateLive {
			restoredSet[nodeID] = true
		} else if preState != auditNodeStateLive || postState != auditNodeStateLive {
			return replayedTopologyMutation{}, nil, errors.New(
				"node restore contains an unsupported topology state transition",
			)
		}
		changeByID[nodeID] = change
		storedIDs = append(storedIDs, nodeID)
	}
	if !slices.IsSorted(storedIDs) {
		return replayedTopologyMutation{}, nil, errors.New(
			"node restore topology changes are not in canonical node order",
		)
	}
	restoreRoot, originParent, originName, err := replay.deriveNodeRestoreRoot(restoredSet)
	if err != nil {
		return replayedTopologyMutation{}, nil, err
	}
	restoredIDs, err := replay.trashedTopologySubtree(restoreRoot)
	if err != nil {
		return replayedTopologyMutation{}, nil, err
	}
	if len(restoredIDs) != len(restoredSet) {
		return replayedTopologyMutation{}, nil, errors.New(
			"node restore transition does not cover its complete trash subtree",
		)
	}
	for _, nodeID := range restoredIDs {
		if !restoredSet[nodeID] {
			return replayedTopologyMutation{}, nil, fmt.Errorf(
				"node restore omits trash subtree node %d", nodeID,
			)
		}
	}
	if restoredSet[originParent] || len(changeByID) != len(restoredIDs)+1 {
		return replayedTopologyMutation{}, nil, errors.New(
			"node restore changed-node set does not match its subtree and destination parent",
		)
	}
	if _, ok := changeByID[originParent]; !ok {
		return replayedTopologyMutation{}, nil, errors.New(
			"node restore lacks its destination-parent topology change",
		)
	}
	if err := requireLiveDirectoryTopology(replay.topology, originParent); err != nil {
		return replayedTopologyMutation{}, nil, err
	}
	finalName, err := replay.nextFreeTopologyName(originParent, originName)
	if err != nil {
		return replayedTopologyMutation{}, nil, err
	}
	changedIDs := make([]uint64, 0, len(changeByID))
	for nodeID := range changeByID {
		if !replay.memberSet[nodeID] {
			return replayedTopologyMutation{}, nil, fmt.Errorf(
				"in-scope restore changes unaudited node %d", nodeID,
			)
		}
		changedIDs = append(changedIDs, nodeID)
	}
	slices.Sort(changedIDs)
	recordedAt, err := auditField(mutation, auditRecordedAtField)
	if err != nil {
		return replayedTopologyMutation{}, nil, err
	}
	postTopology := slices.Clone(replay.topology)
	for _, nodeID := range changedIDs {
		change := changeByID[nodeID]
		pre, err := auditNestedField(change, "pre")
		if err != nil {
			return replayedTopologyMutation{}, nil, err
		}
		post, err := auditNestedField(change, "post")
		if err != nil {
			return replayedTopologyMutation{}, nil, err
		}
		index, ok := replay.topologyIndex[nodeID]
		if !ok || !auditRecordEqual(replay.topology[index], pre) {
			return replayedTopologyMutation{}, nil, fmt.Errorf(
				"node restore pre-state for %d does not match replay", nodeID,
			)
		}
		expected, err := expectedNodeRestorePost(
			pre, nodeID, restoreRoot, originParent, finalName,
			restoredSet[nodeID], recordedAt,
		)
		if err != nil {
			return replayedTopologyMutation{}, nil, err
		}
		if !auditRecordEqual(expected, post) {
			return replayedTopologyMutation{}, nil, fmt.Errorf(
				"node restore post-state for %d does not match its transition", nodeID,
			)
		}
		postTopology[index] = post
	}
	if err := validateReplayedAuditTopology(postTopology); err != nil {
		return replayedTopologyMutation{}, nil, fmt.Errorf(
			"validating node restore topology: %w", err,
		)
	}
	usedTopology[digest] = true
	return replayedTopologyMutation{
		topologyDigest: digest, postTopology: postTopology, changedIDs: changedIDs,
	}, restoredIDs, nil
}

func (replay *auditedHistoryReplay) deriveNodeRestoreRoot(
	restoredSet map[uint64]bool,
) (uint64, uint64, []byte, error) {
	var root, originParent uint64
	var originName []byte
	for nodeID := range restoredSet {
		index, ok := replay.topologyIndex[nodeID]
		if !ok {
			return 0, 0, nil, fmt.Errorf(
				"node restore references missing topology node %d", nodeID,
			)
		}
		originValue, err := auditField(replay.topology[index], auditOriginField)
		if err != nil {
			return 0, 0, nil, err
		}
		if originValue.IsAbsent() {
			continue
		}
		if root != 0 {
			return 0, 0, nil, errors.New("node restore transition has multiple trash roots")
		}
		origin, ok := originValue.RecordValue()
		if !ok || origin.Kind != "known_origin" {
			return 0, 0, nil, errors.New(
				"in-scope restore requires one known trash origin",
			)
		}
		if err := requireAuditUnsigned(origin, metadataNodeIDField, nodeID); err != nil {
			return 0, 0, nil, err
		}
		originParent, err = auditUnsignedField(origin, "parent_id")
		if err != nil {
			return 0, 0, nil, err
		}
		originName, err = auditNameBytesField(origin)
		if err != nil {
			return 0, 0, nil, err
		}
		root = nodeID
	}
	if root == 0 {
		return 0, 0, nil, errors.New("node restore transition lacks one trash root")
	}
	return root, originParent, originName, nil
}

func (replay *auditedHistoryReplay) trashedTopologySubtree(root uint64) ([]uint64, error) {
	rootIndex, ok := replay.topologyIndex[root]
	if !ok {
		return nil, fmt.Errorf("audit topology lacks trash root %d", root)
	}
	rootStamp, err := auditField(replay.topology[rootIndex], "trashed_at")
	if err != nil {
		return nil, err
	}
	rootTime, ok := rootStamp.TimestampValue()
	if !ok {
		return nil, errors.New("audit trash root lacks its trash timestamp")
	}
	children := make(map[uint64][]uint64)
	for _, node := range replay.topology {
		nodeID, err := auditUnsignedField(node, metadataNodeIDField)
		if err != nil {
			return nil, err
		}
		state, err := auditTextField(node, auditStateField)
		if err != nil {
			return nil, err
		}
		parentID, err := auditOptionalParentIDField(node)
		if err != nil {
			return nil, err
		}
		stamp, err := auditField(node, "trashed_at")
		if err != nil {
			return nil, err
		}
		stampTime, hasStamp := stamp.TimestampValue()
		if state == auditNodeStateTrash && hasStamp && stampTime == rootTime && parentID != nil {
			children[*parentID] = append(children[*parentID], nodeID)
		}
	}
	var result []uint64
	var add func(uint64)
	add = func(nodeID uint64) {
		result = append(result, nodeID)
		slices.Sort(children[nodeID])
		for _, child := range children[nodeID] {
			add(child)
		}
	}
	add(root)
	slices.Sort(result)
	return result, nil
}

func (replay *auditedHistoryReplay) nextFreeTopologyName(
	parentID uint64, name []byte,
) ([]byte, error) {
	base, ext := splitSuffix(string(name))
	occupied := make(map[string]bool)
	for _, node := range replay.topology {
		state, err := auditTextField(node, auditStateField)
		if err != nil {
			return nil, err
		}
		if state != auditNodeStateLive {
			continue
		}
		candidateParent, err := auditOptionalParentIDField(node)
		if err != nil {
			return nil, err
		}
		if candidateParent == nil || *candidateParent != parentID {
			continue
		}
		candidateName, err := auditNameBytesField(node)
		if err != nil {
			return nil, err
		}
		occupied[string(candidateName)] = true
	}
	for suffix := 1; ; suffix++ {
		candidate := suffixedName(base, ext, suffix)
		if !occupied[candidate] {
			return []byte(candidate), nil
		}
	}
}

func expectedNodeRestorePost(
	pre audit.Record, nodeID, restoreRoot, originParent uint64, finalName []byte,
	restored bool, recordedAt audit.Value,
) (audit.Record, error) {
	expected, err := replaceAuditRecordField(pre, "modified_at", recordedAt)
	if err != nil || !restored {
		return expected, err
	}
	liveState, err := audit.Text(auditNodeStateLive)
	if err != nil {
		return audit.Record{}, err
	}
	expected, err = replaceAuditRecordField(expected, auditStateField, liveState)
	if err != nil {
		return audit.Record{}, err
	}
	expected, err = replaceAuditRecordField(expected, "trashed_at", audit.Absent())
	if err != nil {
		return audit.Record{}, err
	}
	expected, err = replaceAuditRecordField(expected, auditOriginField, audit.Absent())
	if err != nil || nodeID != restoreRoot {
		return expected, err
	}
	expected, err = replaceAuditRecordField(expected, "parent_id", audit.Unsigned(originParent))
	if err != nil {
		return audit.Record{}, err
	}
	return replaceAuditRecordField(expected, "name", audit.Bytes(finalName))
}

func (replay *auditedHistoryReplay) deriveNodeRestorePathEffects(
	postTopology []audit.Record, restoredIDs []uint64,
) ([]audit.Record, error) {
	scopeID, err := audit.UUID(replay.scopeID)
	if err != nil {
		return nil, err
	}
	trash, err := audit.Text(auditNodeStateTrash)
	if err != nil {
		return nil, err
	}
	live, err := audit.Text(auditNodeStateLive)
	if err != nil {
		return nil, err
	}
	effects := make([]audit.Record, 0, len(restoredIDs))
	for _, nodeID := range restoredIDs {
		priorPath, err := auditKnownTrashPath(
			replay.topology, replay.topologyIndex, nodeID,
		)
		if err != nil {
			return nil, err
		}
		postPath, postLive, err := auditLivePath(
			postTopology, replay.topologyIndex, nodeID,
		)
		if err != nil {
			return nil, err
		}
		if !postLive {
			return nil, fmt.Errorf("node restore result %d is not live", nodeID)
		}
		effects = append(effects, audit.Record{Kind: "path_effect", Fields: []audit.Field{
			{Name: auditScopeIDField, Value: scopeID},
			{Name: "member_node_id", Value: audit.Unsigned(nodeID)},
			{Name: "old", Value: audit.Nested(audit.Record{Kind: auditPathStateKind, Fields: []audit.Field{
				{Name: auditPathField, Value: audit.Bytes(priorPath)}, {Name: auditStateField, Value: trash},
			}})},
			{Name: "new", Value: audit.Nested(audit.Record{Kind: auditPathStateKind, Fields: []audit.Field{
				{Name: auditPathField, Value: audit.Bytes(postPath)}, {Name: auditStateField, Value: live},
			}})},
		}})
	}
	return effects, nil
}
