package store

import (
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/docbank/internal/audit"
)

func (replay *auditedHistoryReplay) applyNodeTrash(
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
		return errors.New("in-scope trash cannot bind an enrollment baseline")
	}
	transition, trashIDs, err := replay.validateNodeTrashTopology(
		mutation.record, operationID, topologyRecords, usedTopology,
	)
	if err != nil {
		return err
	}
	expectedEffects, err := replay.deriveNodeTrashPathEffects(
		transition.postTopology, trashIDs,
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
	if err := replay.validateMemberStateChanges(
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

func (replay *auditedHistoryReplay) validateNodeTrashTopology(
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
			"node trash lacks one unique topology delta",
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
			"in-scope trash topology must change a subtree and its origin parent",
		)
	}
	changeByID := make(map[uint64]audit.Record, len(changes))
	trashSet := make(map[uint64]bool, len(changes)-1)
	storedIDs := make([]uint64, 0, len(changes))
	for _, change := range changes {
		nodeID, err := auditUnsignedField(change, metadataNodeIDField)
		if err != nil {
			return replayedTopologyMutation{}, nil, err
		}
		if _, exists := changeByID[nodeID]; exists {
			return replayedTopologyMutation{}, nil, fmt.Errorf(
				"node trash repeats topology change for %d", nodeID,
			)
		}
		pre, preErr := auditNestedField(change, "pre")
		post, postErr := auditNestedField(change, "post")
		if preErr != nil || postErr != nil {
			return replayedTopologyMutation{}, nil, errors.New(
				"in-scope trash requires complete topology sides",
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
		if preState == auditNodeStateLive && postState == auditNodeStateTrash {
			trashSet[nodeID] = true
		} else if preState != auditNodeStateLive || postState != auditNodeStateLive {
			return replayedTopologyMutation{}, nil, errors.New(
				"node trash contains an unsupported topology state transition",
			)
		}
		changeByID[nodeID] = change
		storedIDs = append(storedIDs, nodeID)
	}
	if !slices.IsSorted(storedIDs) {
		return replayedTopologyMutation{}, nil, errors.New(
			"node trash topology changes are not in canonical node order",
		)
	}
	trashRoot, err := replay.deriveNodeTrashRoot(trashSet)
	if err != nil {
		return replayedTopologyMutation{}, nil, err
	}
	trashIDs, err := replay.liveTopologySubtree(trashRoot)
	if err != nil {
		return replayedTopologyMutation{}, nil, err
	}
	if len(trashIDs) != len(trashSet) {
		return replayedTopologyMutation{}, nil, errors.New(
			"node trash transition does not cover its complete live subtree",
		)
	}
	for _, nodeID := range trashIDs {
		if !trashSet[nodeID] {
			return replayedTopologyMutation{}, nil, fmt.Errorf(
				"node trash omits live subtree node %d", nodeID,
			)
		}
	}
	rootPre, err := auditNestedField(changeByID[trashRoot], "pre")
	if err != nil {
		return replayedTopologyMutation{}, nil, err
	}
	originParent, err := auditOptionalParentIDField(rootPre)
	if err != nil || originParent == nil {
		return replayedTopologyMutation{}, nil, errors.New(
			"node trash root lacks its live origin parent",
		)
	}
	if trashSet[*originParent] || len(changeByID) != len(trashIDs)+1 {
		return replayedTopologyMutation{}, nil, errors.New(
			"node trash changed-node set does not match its subtree and origin parent",
		)
	}
	if _, ok := changeByID[*originParent]; !ok {
		return replayedTopologyMutation{}, nil, errors.New(
			"node trash lacks its origin-parent topology change",
		)
	}
	changedIDs := make([]uint64, 0, len(changeByID))
	for nodeID := range changeByID {
		if !replay.memberSet[nodeID] {
			return replayedTopologyMutation{}, nil, fmt.Errorf(
				"in-scope trash changes unaudited node %d", nodeID,
			)
		}
		changedIDs = append(changedIDs, nodeID)
	}
	slices.Sort(changedIDs)
	rootID, err := auditTopologyRootID(replay.topology)
	if err != nil {
		return replayedTopologyMutation{}, nil, err
	}
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
				"node trash pre-state for %d does not match replay", nodeID,
			)
		}
		expected, err := expectedNodeTrashPost(
			pre, nodeID, trashRoot, rootID, *originParent, trashSet[nodeID], recordedAt,
		)
		if err != nil {
			return replayedTopologyMutation{}, nil, err
		}
		if !auditRecordEqual(expected, post) {
			return replayedTopologyMutation{}, nil, fmt.Errorf(
				"node trash post-state for %d does not match its transition", nodeID,
			)
		}
		postTopology[index] = post
	}
	if err := validateReplayedAuditTopology(postTopology); err != nil {
		return replayedTopologyMutation{}, nil, fmt.Errorf(
			"validating node trash topology: %w", err,
		)
	}
	usedTopology[digest] = true
	return replayedTopologyMutation{
		topologyDigest: digest, postTopology: postTopology, changedIDs: changedIDs,
	}, trashIDs, nil
}

func (replay *auditedHistoryReplay) deriveNodeTrashRoot(
	trashSet map[uint64]bool,
) (uint64, error) {
	var root uint64
	for nodeID := range trashSet {
		index, ok := replay.topologyIndex[nodeID]
		if !ok {
			return 0, fmt.Errorf("node trash references missing topology node %d", nodeID)
		}
		parentID, err := auditOptionalParentIDField(replay.topology[index])
		if err != nil {
			return 0, err
		}
		if parentID == nil || trashSet[*parentID] {
			continue
		}
		if root != 0 {
			return 0, errors.New("node trash transition has multiple subtree roots")
		}
		root = nodeID
	}
	if root == 0 {
		return 0, errors.New("node trash transition lacks one subtree root")
	}
	return root, nil
}

func (replay *auditedHistoryReplay) liveTopologySubtree(root uint64) ([]uint64, error) {
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
		if state == auditNodeStateLive && parentID != nil {
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

func expectedNodeTrashPost(
	pre audit.Record, nodeID, trashRoot, vaultRoot, originParent uint64,
	trashed bool, recordedAt audit.Value,
) (audit.Record, error) {
	expected, err := replaceAuditRecordField(pre, "modified_at", recordedAt)
	if err != nil || !trashed {
		return expected, err
	}
	trashState, err := audit.Text(auditNodeStateTrash)
	if err != nil {
		return audit.Record{}, err
	}
	expected, err = replaceAuditRecordField(expected, auditStateField, trashState)
	if err != nil {
		return audit.Record{}, err
	}
	expected, err = replaceAuditRecordField(expected, "trashed_at", recordedAt)
	if err != nil || nodeID != trashRoot {
		return expected, err
	}
	name, err := auditNameBytesField(pre)
	if err != nil {
		return audit.Record{}, err
	}
	origin := audit.Record{Kind: "known_origin", Fields: []audit.Field{
		{Name: metadataNodeIDField, Value: audit.Unsigned(nodeID)},
		{Name: "parent_id", Value: audit.Unsigned(originParent)},
		{Name: "name", Value: audit.Bytes(name)},
	}}
	expected, err = replaceAuditRecordField(expected, "parent_id", audit.Unsigned(vaultRoot))
	if err != nil {
		return audit.Record{}, err
	}
	return replaceAuditRecordField(expected, auditOriginField, audit.Nested(origin))
}

func auditTopologyRootID(topology []audit.Record) (uint64, error) {
	var root uint64
	for _, node := range topology {
		parentID, err := auditOptionalParentIDField(node)
		if err != nil {
			return 0, err
		}
		if parentID != nil {
			continue
		}
		nodeID, err := auditUnsignedField(node, metadataNodeIDField)
		if err != nil {
			return 0, err
		}
		if root != 0 {
			return 0, errors.New("audit topology has multiple vault roots")
		}
		root = nodeID
	}
	if root == 0 {
		return 0, errors.New("audit topology lacks its vault root")
	}
	return root, nil
}

func (replay *auditedHistoryReplay) deriveNodeTrashPathEffects(
	postTopology []audit.Record, trashIDs []uint64,
) ([]audit.Record, error) {
	scopeID, err := audit.UUID(replay.scopeID)
	if err != nil {
		return nil, err
	}
	live, err := audit.Text(auditNodeStateLive)
	if err != nil {
		return nil, err
	}
	trash, err := audit.Text(auditNodeStateTrash)
	if err != nil {
		return nil, err
	}
	effects := make([]audit.Record, 0, len(trashIDs))
	for _, nodeID := range trashIDs {
		priorPath, priorLive, err := auditLivePath(
			replay.topology, replay.topologyIndex, nodeID,
		)
		if err != nil {
			return nil, err
		}
		if !priorLive {
			return nil, fmt.Errorf("node trash source %d is not live", nodeID)
		}
		postPath, err := auditKnownTrashPath(
			postTopology, replay.topologyIndex, nodeID,
		)
		if err != nil {
			return nil, err
		}
		effects = append(effects, audit.Record{Kind: "path_effect", Fields: []audit.Field{
			{Name: auditScopeIDField, Value: scopeID},
			{Name: "member_node_id", Value: audit.Unsigned(nodeID)},
			{Name: "old", Value: audit.Nested(audit.Record{Kind: auditPathStateKind, Fields: []audit.Field{
				{Name: auditPathField, Value: audit.Bytes(priorPath)}, {Name: auditStateField, Value: live},
			}})},
			{Name: "new", Value: audit.Nested(audit.Record{Kind: auditPathStateKind, Fields: []audit.Field{
				{Name: auditPathField, Value: audit.Bytes(postPath)}, {Name: auditStateField, Value: trash},
			}})},
		}})
	}
	return effects, nil
}

func auditKnownTrashPath(
	topology []audit.Record, topologyIndex map[uint64]int, nodeID uint64,
) ([]byte, error) {
	index, ok := topologyIndex[nodeID]
	if !ok {
		return nil, fmt.Errorf("audit topology lacks node %d", nodeID)
	}
	current := topology[index]
	if err := requireAuditText(current, auditStateField, auditNodeStateTrash); err != nil {
		return nil, err
	}
	visited := make(map[uint64]bool)
	var descendants [][]byte
	for {
		currentID, err := auditUnsignedField(current, metadataNodeIDField)
		if err != nil {
			return nil, err
		}
		if visited[currentID] {
			return nil, errors.New("audit topology contains a trash-parent cycle")
		}
		visited[currentID] = true
		originValue, err := auditField(current, auditOriginField)
		if err != nil {
			return nil, err
		}
		if !originValue.IsAbsent() {
			origin, ok := originValue.RecordValue()
			if !ok || origin.Kind != "known_origin" {
				return nil, errors.New("audited trash transition lacks a known origin")
			}
			if err := requireAuditUnsigned(origin, metadataNodeIDField, currentID); err != nil {
				return nil, err
			}
			originParent, err := auditUnsignedField(origin, "parent_id")
			if err != nil {
				return nil, err
			}
			originName, err := auditNameBytesField(origin)
			if err != nil {
				return nil, err
			}
			parentPath, liveParent, err := auditLivePath(
				topology, topologyIndex, originParent,
			)
			if err != nil {
				return nil, err
			}
			if !liveParent {
				return nil, errors.New("audited trash origin parent is not live")
			}
			result := append([]byte("@trash/known"), parentPath...)
			if result[len(result)-1] != '/' {
				result = append(result, '/')
			}
			result = append(result, originName...)
			for _, name := range slices.Backward(descendants) {
				result = append(result, '/')
				result = append(result, name...)
			}
			return result, nil
		}
		name, err := auditNameBytesField(current)
		if err != nil {
			return nil, err
		}
		descendants = append(descendants, name)
		parentID, err := auditOptionalParentIDField(current)
		if err != nil || parentID == nil {
			return nil, errors.New("trash descendant lacks its parent")
		}
		parentIndex, ok := topologyIndex[*parentID]
		if !ok {
			return nil, fmt.Errorf("trash descendant references missing parent %d", *parentID)
		}
		current = topology[parentIndex]
		if err := requireAuditText(current, auditStateField, auditNodeStateTrash); err != nil {
			return nil, err
		}
	}
}
