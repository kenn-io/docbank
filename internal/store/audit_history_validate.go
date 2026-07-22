package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/docbank/internal/audit"
)

type replayAuditTopologyNode struct {
	record       audit.Record
	parentID     *uint64
	originParent *uint64
	kind         string
	state        string
}

func deriveInitialAuditMembersFromRecords(
	topology []audit.Record, targetNodeID uint64,
) ([]uint64, error) {
	nodes, err := replayAuditTopology(topology)
	if err != nil {
		return nil, err
	}
	target, ok := nodes[targetNodeID]
	if !ok || target.kind != nodeKindDir || target.state != auditNodeStateLive {
		return nil, fmt.Errorf("audit enrollment target %d must be a live directory", targetNodeID)
	}
	children := make(map[uint64][]uint64)
	for nodeID, node := range nodes {
		if node.parentID != nil {
			children[*node.parentID] = append(children[*node.parentID], nodeID)
		}
	}
	members := map[uint64]bool{targetNodeID: true}
	addReplayAuditDescendants(targetNodeID, nodes, children, members, true)
	for changed := true; changed; {
		changed = false
		for nodeID, node := range nodes {
			if node.state != auditNodeStateTrash || members[nodeID] {
				continue
			}
			adopt := node.originParent != nil && members[*node.originParent]
			if node.originParent == nil && target.parentID == nil {
				adopt = true
			}
			if adopt {
				members[nodeID] = true
				addReplayAuditDescendants(nodeID, nodes, children, members, false)
				changed = true
			}
		}
	}
	if target.parentID == nil && len(members) != len(nodes) {
		return nil, fmt.Errorf("root audit enrollment covers %d extant nodes, want %d",
			len(members), len(nodes))
	}
	return sortedAuditMembers(members), nil
}

func replayAuditTopology(topology []audit.Record) (map[uint64]replayAuditTopologyNode, error) {
	result := make(map[uint64]replayAuditTopologyNode, len(topology))
	for _, record := range topology {
		nodeID, err := auditUnsignedField(record, metadataNodeIDField)
		if err != nil || nodeID == 0 {
			return nil, fmt.Errorf("reading audit topology node ID: %w", err)
		}
		if _, exists := result[nodeID]; exists {
			return nil, fmt.Errorf("audit topology repeats node %d", nodeID)
		}
		parentID, err := auditOptionalParentIDField(record)
		if err != nil {
			return nil, err
		}
		kind, err := auditTextField(record, "node_kind")
		if err != nil {
			return nil, err
		}
		state, err := auditTextField(record, auditStateField)
		if err != nil {
			return nil, err
		}
		originValue, err := auditField(record, auditOriginField)
		if err != nil {
			return nil, err
		}
		var originParent *uint64
		if !originValue.IsAbsent() {
			origin, ok := originValue.RecordValue()
			if !ok {
				return nil, fmt.Errorf("audit topology node %d has invalid origin", nodeID)
			}
			if origin.Kind == "known_origin" {
				originParent, err = auditOptionalParentIDField(origin)
				if err != nil || originParent == nil {
					return nil, fmt.Errorf("audit topology node %d has invalid known origin", nodeID)
				}
			}
		}
		result[nodeID] = replayAuditTopologyNode{
			record: record, parentID: parentID, originParent: originParent,
			kind: kind, state: state,
		}
	}
	return result, nil
}

func validateReplayedAuditTopology(topology []audit.Record) error {
	nodes, err := replayAuditTopology(topology)
	if err != nil {
		return err
	}
	type liveSibling struct {
		parentID uint64
		name     string
	}
	liveNames := make(map[liveSibling]uint64)
	var rootID uint64
	for nodeID, node := range nodes {
		nameBytes, err := auditNameBytesField(node.record)
		if err != nil {
			return err
		}
		if node.parentID == nil {
			if rootID != 0 || len(nameBytes) != 0 || node.kind != nodeKindDir ||
				node.state != auditNodeStateLive {
				return errors.New("audit topology has an invalid vault root")
			}
			rootID = nodeID
			continue
		}
		name := string(nameBytes)
		normalized, err := NormalizeName(name)
		if err != nil || normalized != name {
			return fmt.Errorf("audit topology node %d has invalid canonical name", nodeID)
		}
		if node.state != auditNodeStateLive {
			continue
		}
		parent, ok := nodes[*node.parentID]
		if !ok || parent.state != auditNodeStateLive || parent.kind != nodeKindDir {
			return fmt.Errorf("live audit topology node %d has an invalid parent", nodeID)
		}
		key := liveSibling{parentID: *node.parentID, name: name}
		if siblingID, exists := liveNames[key]; exists {
			return fmt.Errorf(
				"live audit topology nodes %d and %d have the same parent and name",
				siblingID, nodeID,
			)
		}
		liveNames[key] = nodeID
	}
	if rootID == 0 {
		return errors.New("audit topology lacks a vault root")
	}
	return nil
}

func addReplayAuditDescendants(
	root uint64, nodes map[uint64]replayAuditTopologyNode,
	children map[uint64][]uint64, members map[uint64]bool, liveOnly bool,
) {
	for _, child := range children[root] {
		if members[child] || (liveOnly && nodes[child].state != auditNodeStateLive) {
			continue
		}
		members[child] = true
		addReplayAuditDescendants(child, nodes, children, members, liveOnly)
	}
}

func validateInitialAuditMemberProjection(
	members []uint64, topology, states, versions []audit.Record,
) error {
	nodes, err := replayAuditTopology(topology)
	if err != nil {
		return err
	}
	if len(states) != len(members) {
		return errors.New("audit enrollment member-state count does not match membership")
	}
	versionByID := make(map[string]audit.Record, len(versions))
	versionNode := make(map[string]uint64, len(versions))
	latestRevision := make(map[uint64]uint64)
	for _, version := range versions {
		versionID, err := auditUUIDField(version, "version_id")
		if err != nil {
			return err
		}
		nodeID, err := auditUnsignedField(version, metadataNodeIDField)
		if err != nil {
			return err
		}
		revision, err := auditUnsignedField(version, "node_revision")
		if err != nil || revision == 0 {
			return fmt.Errorf("audit content version %s has invalid revision", versionID)
		}
		if _, exists := versionByID[versionID]; exists {
			return fmt.Errorf("audit enrollment repeats content version %s", versionID)
		}
		versionByID[versionID], versionNode[versionID] = version, nodeID
		latestRevision[nodeID] = max(latestRevision[nodeID], revision)
	}
	for index, state := range states {
		nodeID, err := auditUnsignedField(state, metadataNodeIDField)
		if err != nil {
			return err
		}
		if nodeID != members[index] {
			return errors.New("audit enrollment member states do not follow membership order")
		}
		revision, err := auditUnsignedField(state, "node_revision")
		if err != nil || revision == 0 {
			return fmt.Errorf("audit member %d has invalid baseline revision", nodeID)
		}
		current, err := auditOptionalUUIDField(state, "current_version_id")
		if err != nil {
			return err
		}
		node, exists := nodes[nodeID]
		if !exists {
			return fmt.Errorf("audit member %d is absent from topology genesis", nodeID)
		}
		if node.kind == nodeKindDir {
			if current != nil || latestRevision[nodeID] != 0 {
				return fmt.Errorf("audited directory %d carries content authority", nodeID)
			}
			continue
		}
		if current == nil || versionNode[*current] != nodeID {
			return fmt.Errorf("audited file %d lacks its baseline content version", nodeID)
		}
		currentRevision, err := auditUnsignedField(versionByID[*current], "node_revision")
		if err != nil {
			return err
		}
		if currentRevision != latestRevision[nodeID] || currentRevision > revision {
			return fmt.Errorf("audited file %d baseline head is not its latest version", nodeID)
		}
	}
	return nil
}

type auditedHistoryReplay struct {
	scopeID          string
	lineageID        string
	members          []uint64
	memberSet        map[uint64]bool
	states           map[uint64]audit.Record
	versions         map[string]audit.Record
	attachments      map[string]audit.Record
	tagDefinitionIDs map[string]bool
	topology         []audit.Record
	topologyIndex    map[uint64]int
	scopeHead        string
	scopeEntryCount  int64
	allocationHead   string
	allocationCount  int64
	nodeHighWater    uint64
	baselines        map[string]auditBaselineProjection
	memberBaselines  map[uint64]string
	scopes           map[string]*auditScopeReplay
}

type auditScopeReplay struct {
	projection      initialAuditScope
	members         []uint64
	memberSet       map[uint64]bool
	memberBaselines map[uint64]string
	head            string
	entryCount      int64
}

type auditBaselineProjection struct {
	scopeID, operationID string
	targetNodeID         uint64
}

func validateAuditedHistory(
	ctx context.Context, tx metadataQuerier, vaultID string, nodeSequence int64,
	authority initialAuditAuthority, scopes []initialAuditScope, scope initialAuditScope,
	records, initial map[string][]storedAuditRecord,
) error {
	if err := validateStoredAuditRecordSchemas(records); err != nil {
		return err
	}
	replay, err := newAuditedHistoryReplay(authority, scope, initial)
	if err != nil {
		return err
	}
	mutations, err := auditRecordsByOptionalSequence(
		records["canonical_mutation"], authority.sequence,
	)
	if err != nil {
		return err
	}
	allocations, err := auditRecordsBySequence(records["allocation_entry"], authority.sequence)
	if err != nil {
		return err
	}
	entries, err := auditScopeRecordsByScope(records["scope_chain_entry"], scopes)
	if err != nil {
		return err
	}
	events, err := auditEventRecordsByID(records[auditEventField])
	if err != nil {
		return err
	}
	usedEvents := map[string]bool{}
	baselines := auditRecordsByDigest(records["enrollment_baseline"])
	topologyDeltas := auditRecordsByDigest(records[auditTopologyDeltaField])
	pathEffectLists := auditRecordsByDigest(records["path_effect_list"])
	attachmentDeltas := auditRecordsByDigest(records["attached_metadata_delta"])
	usedBaselines := map[string]bool{initial["enrollment_baseline"][0].digest: true}
	usedTopologyDeltas := map[string]bool{}
	usedPathEffectLists := map[string]bool{}
	usedAttachmentDeltas := map[string]bool{}
	for sequence := int64(2); sequence <= authority.sequence; sequence++ {
		allocation := allocations[sequence]
		mutation, hasMutation := mutations[sequence]
		if !hasMutation {
			err = replay.applyUnscopedTagDefinitionChange(
				vaultID, allocation, attachmentDeltas, usedAttachmentDeltas,
			)
			if err != nil {
				return fmt.Errorf("validating audit operation %d: %w", sequence, err)
			}
			continue
		}
		bindings, bindingErr := auditRecordListField(mutation.record, "baselines")
		if bindingErr != nil {
			return fmt.Errorf("validating audit operation %d: %w", sequence, bindingErr)
		}
		topologyValue, topologyErr := auditField(mutation.record, auditTopologyDeltaField)
		if topologyErr != nil {
			return fmt.Errorf("validating audit operation %d: %w", sequence, topologyErr)
		}
		mutationScopeID, scopeErr := auditMutationScopeID(mutation.record)
		if scopeErr != nil {
			return fmt.Errorf("validating audit operation %d: %w", sequence, scopeErr)
		}
		if len(bindings) != 0 && topologyValue.IsAbsent() && replay.scopes[mutationScopeID] == nil {
			err = replay.applyAdditionalScopeEnrollment(
				vaultID, mutation, allocation, scopes, entries, baselines, events,
				usedBaselines, usedEvents,
			)
			if err != nil {
				return fmt.Errorf("validating audit operation %d: %w", sequence, err)
			}
			continue
		}
		if err := replay.activateScope(mutationScopeID); err != nil {
			return fmt.Errorf("validating audit operation %d: %w", sequence, err)
		}
		scopeEntry, ok := entries[mutationScopeID][replay.scopeEntryCount+1]
		if !ok {
			return fmt.Errorf("validating audit operation %d: audit scope chain is incomplete", sequence)
		}
		if len(bindings) == 0 && topologyValue.IsAbsent() {
			attachedCount, attachedErr := auditUnsignedField(
				mutation.record, auditAttachedMetadataChangeCountField,
			)
			if attachedErr != nil {
				err = attachedErr
			} else if attachedCount != 0 {
				kind, kindErr := attachedMutationKind(mutation.record, attachmentDeltas)
				if kindErr != nil {
					err = kindErr
				} else if kind == "tag_rename" {
					err = replay.applyTagDefinitionRename(
						vaultID, mutation, allocation, scopeEntry,
						attachmentDeltas, events, usedAttachmentDeltas, usedEvents,
					)
				} else if kind == "tag_delete" {
					err = replay.applyTagDefinitionDelete(
						vaultID, mutation, allocation, scopeEntry,
						attachmentDeltas, events, usedAttachmentDeltas, usedEvents,
					)
				} else {
					err = replay.applyTagAssignment(
						vaultID, mutation, allocation, scopeEntry,
						attachmentDeltas, events, usedAttachmentDeltas, usedEvents,
					)
				}
			} else {
				err = replay.applyContentTransition(
					vaultID, mutation, allocation, scopeEntry, events, usedEvents,
				)
			}
		} else if len(bindings) != 0 {
			err = replay.applyNodeCreation(
				vaultID, mutation, allocation, scopeEntry,
				baselines, topologyDeltas, attachmentDeltas, events, usedBaselines,
				usedTopologyDeltas, usedAttachmentDeltas, usedEvents,
			)
		} else {
			kind, kindErr := classifyAuditedTopologyMutation(mutation.record, topologyDeltas)
			if kindErr != nil {
				err = kindErr
			} else if kind == auditedTopologyTrash {
				err = replay.applyNodeTrash(
					vaultID, mutation, allocation, scopeEntry,
					topologyDeltas, pathEffectLists, events,
					usedTopologyDeltas, usedPathEffectLists, usedEvents,
				)
			} else if kind == auditedTopologyRestore {
				err = replay.applyNodeRestore(
					vaultID, mutation, allocation, scopeEntry,
					topologyDeltas, pathEffectLists, events,
					usedTopologyDeltas, usedPathEffectLists, usedEvents,
				)
			} else {
				err = replay.applyNodeMove(
					vaultID, mutation, allocation, scopeEntry,
					topologyDeltas, pathEffectLists, events,
					usedTopologyDeltas, usedPathEffectLists, usedEvents,
				)
			}
		}
		if err != nil {
			return fmt.Errorf("validating audit operation %d: %w", sequence, err)
		}
	}
	initialEventID := *initial[auditEventField][0].index.eventID
	usedEvents[initialEventID] = true
	if len(usedEvents) != len(events) {
		return errors.New("audit history contains an unbound event record")
	}
	if len(usedBaselines) != len(baselines) {
		return errors.New("audit history contains an unbound enrollment baseline")
	}
	if len(usedTopologyDeltas) != len(topologyDeltas) {
		return errors.New("audit history contains an unbound topology delta")
	}
	if len(usedPathEffectLists) != len(pathEffectLists) {
		return errors.New("audit history contains an unbound path-effect list")
	}
	if len(usedAttachmentDeltas) != len(attachmentDeltas) {
		return errors.New("audit history contains an unbound attached-metadata delta")
	}
	if authority.sequence != replay.allocationCount || authority.allocationHead != replay.allocationHead {
		return errors.New("audit allocation authority does not match replayed history")
	}
	replay.saveActiveScope()
	for _, projection := range scopes {
		replayed := replay.scopes[projection.scopeID]
		if replayed == nil || projection.entryCount != replayed.entryCount ||
			projection.chainHead != replayed.head {
			return errors.New("audit scope authority does not match replayed history")
		}
	}
	finalNodeHighWater, err := positiveAuditNodeID(nodeSequence)
	if err != nil {
		return err
	}
	if finalNodeHighWater != replay.nodeHighWater {
		return errors.New("audit node allocation high-water mark does not match replayed history")
	}
	return replay.reconcileCurrentState(ctx, tx)
}

func (replay *auditedHistoryReplay) applyAdditionalScopeEnrollment(
	vaultID string, mutation, allocation storedAuditRecord,
	projections []initialAuditScope,
	entries map[string]map[int64]storedAuditRecord,
	baselines map[string]storedAuditRecord,
	events map[string]storedAuditRecord,
	usedBaselines, usedEvents map[string]bool,
) error {
	bindings, err := auditRecordListField(mutation.record, "baselines")
	if err != nil || len(bindings) != 1 {
		return errors.New("additional audit enrollment must bind one baseline")
	}
	scopeID, err := auditUUIDField(bindings[0], auditScopeIDField)
	if err != nil {
		return err
	}
	var scope initialAuditScope
	for _, candidate := range projections {
		if candidate.scopeID == scopeID {
			scope = candidate
			break
		}
	}
	if scope.scopeID == "" || replay.scopes[scopeID] != nil {
		return errors.New("additional audit enrollment has an invalid scope projection")
	}
	baselineDigest, err := auditDigestField(bindings[0], "baseline_digest")
	if err != nil {
		return err
	}
	baseline, ok := baselines[baselineDigest]
	if !ok || usedBaselines[baselineDigest] {
		return errors.New("additional audit enrollment lacks one unique baseline")
	}
	if err := validateAuditEnrollmentBaseline(
		vaultID, scope, baseline, replay.topology, replay.attachments,
	); err != nil {
		return err
	}
	members, err := auditUnsignedListField(baseline.record, "members")
	if err != nil {
		return err
	}
	for _, member := range members {
		for existingScopeID, existing := range replay.scopes {
			if existing.memberSet[member] {
				return fmt.Errorf("additional audit scope overlaps scope %s at node %d", existingScopeID, member)
			}
		}
	}
	eventList, err := auditRecordListField(mutation.record, "events")
	if err != nil || len(eventList) != 1 {
		return errors.New("additional audit enrollment must contain one event")
	}
	eventID, err := auditDigestField(eventList[0], "event_id")
	if err != nil {
		return err
	}
	eventWrapper, ok := events[eventID]
	if !ok || usedEvents[eventID] {
		return errors.New("additional audit enrollment lacks one unique event wrapper")
	}
	wrapped, err := auditNestedField(eventWrapper.record, auditEventField)
	if err != nil || !auditRecordEqual(wrapped, eventList[0]) {
		return errors.New("additional audit event wrapper does not match its mutation")
	}
	sequence, err := positiveAuditInteger("operation sequence", *mutation.index.operationSequence)
	if err != nil {
		return err
	}
	if err := validateAuditEnrollmentMutation(
		vaultID, scope, baseline, eventWrapper, mutation, sequence,
	); err != nil {
		return err
	}
	scopeEntry, ok := entries[scopeID][1]
	if !ok {
		return errors.New("additional audit scope lacks its initial chain entry")
	}
	if err := validateInitialScopeChain(vaultID, scope, mutation, scopeEntry); err != nil {
		return err
	}
	operationID, err := auditUUIDField(mutation.record, auditOperationIDField)
	if err != nil {
		return err
	}
	if operationID != scope.operationID {
		return errors.New("additional audit scope operation does not match its projection")
	}
	if err := replay.advanceAllocation(vaultID, operationID, mutation, allocation, "", 0); err != nil {
		return err
	}
	states, err := auditRecordListField(baseline.record, "member_states")
	if err != nil {
		return err
	}
	versions, err := auditRecordListField(baseline.record, "versions")
	if err != nil {
		return err
	}
	memberBaselines := make(map[uint64]string, len(members))
	for _, member := range members {
		memberBaselines[member] = baselineDigest
	}
	for _, state := range states {
		nodeID, err := auditUnsignedField(state, metadataNodeIDField)
		if err != nil {
			return err
		}
		if _, exists := replay.states[nodeID]; exists {
			return fmt.Errorf("additional audit enrollment repeats protected node %d", nodeID)
		}
		replay.states[nodeID] = state
	}
	for _, version := range versions {
		versionID, err := auditUUIDField(version, "version_id")
		if err != nil {
			return err
		}
		if _, exists := replay.versions[versionID]; exists {
			return fmt.Errorf("additional audit enrollment repeats protected version %s", versionID)
		}
		replay.versions[versionID] = version
	}
	replay.baselines[baselineDigest] = auditBaselineProjection{
		scopeID: scopeID, operationID: operationID, targetNodeID: scope.targetNodeID,
	}
	replay.scopes[scopeID] = &auditScopeReplay{
		projection: scope, members: slices.Clone(members), memberSet: auditMemberSet(members),
		memberBaselines: memberBaselines, head: scopeEntry.digest, entryCount: 1,
	}
	usedBaselines[baselineDigest] = true
	usedEvents[eventID] = true
	return replay.activateScope(scopeID)
}

func validateAuditEnrollmentBaseline(
	vaultID string, scope initialAuditScope, baseline storedAuditRecord,
	topology []audit.Record, attachments map[string]audit.Record,
) error {
	checks := []func() error{
		func() error { return requireAuditUUID(baseline.record, auditVaultIDField, vaultID) },
		func() error { return requireAuditUUID(baseline.record, auditScopeIDField, scope.scopeID) },
		func() error { return requireAuditUnsigned(baseline.record, "target_node_id", scope.targetNodeID) },
		func() error { return requireAuditUUID(baseline.record, auditOperationIDField, scope.operationID) },
		func() error { return requireAuditText(baseline.record, "cause", "explicit") },
	}
	for _, check := range checks {
		if err := check(); err != nil {
			return err
		}
	}
	members, err := auditUnsignedListField(baseline.record, "members")
	if err != nil {
		return err
	}
	expectedMembers, err := deriveInitialAuditMembersFromRecords(topology, scope.targetNodeID)
	if err != nil {
		return err
	}
	if !slices.Equal(members, expectedMembers) {
		return errors.New("additional audit enrollment members do not match the protected closure")
	}
	states, err := auditRecordListField(baseline.record, "member_states")
	if err != nil {
		return err
	}
	versions, err := auditRecordListField(baseline.record, "versions")
	if err != nil {
		return err
	}
	if err := validateInitialAuditMemberProjection(members, topology, states, versions); err != nil {
		return err
	}
	allAttachments := make([]audit.Record, 0, len(attachments))
	for _, record := range attachments {
		allAttachments = append(allAttachments, record)
	}
	expectedAttachments, err := auditRecordsForNodes(allAttachments, auditMemberSet(members))
	if err != nil {
		return err
	}
	storedAttachments, err := auditRecordListField(baseline.record, "attachments")
	if err != nil {
		return err
	}
	if !equalAuditRecordLists(storedAttachments, expectedAttachments) {
		return errors.New("additional audit enrollment attachments do not match replayed metadata")
	}
	return validateInitialBaselineTopology(
		baseline.record, members, scope.operationID, topology,
	)
}

const (
	auditedTopologyMove    = "move"
	auditedTopologyTrash   = "trash"
	auditedTopologyRestore = "restore"
)

func classifyAuditedTopologyMutation(
	mutation audit.Record, topologyRecords map[string]storedAuditRecord,
) (string, error) {
	digest, err := auditDigestField(mutation, auditTopologyDeltaField)
	if err != nil {
		return "", err
	}
	delta, ok := topologyRecords[digest]
	if !ok {
		return "", errors.New("topology mutation lacks its topology delta")
	}
	changes, err := auditRecordListField(delta.record, "changes")
	if err != nil {
		return "", err
	}
	var sawTrash, sawRestore bool
	for _, change := range changes {
		pre, preErr := auditNestedField(change, "pre")
		post, postErr := auditNestedField(change, "post")
		if preErr != nil || postErr != nil {
			return "", errors.New("topology mutation requires complete pre/post states")
		}
		preState, err := auditTextField(pre, auditStateField)
		if err != nil {
			return "", err
		}
		postState, err := auditTextField(post, auditStateField)
		if err != nil {
			return "", err
		}
		switch {
		case preState == auditNodeStateLive && postState == auditNodeStateLive:
		case preState == auditNodeStateLive && postState == auditNodeStateTrash:
			sawTrash = true
		case preState == auditNodeStateTrash && postState == auditNodeStateLive:
			sawRestore = true
		default:
			return "", fmt.Errorf(
				"unsupported audited topology state transition %s to %s", preState, postState,
			)
		}
	}
	if sawTrash && sawRestore {
		return "", errors.New("audited topology mutation mixes trash and restore transitions")
	}
	if sawTrash {
		return auditedTopologyTrash, nil
	}
	if sawRestore {
		return auditedTopologyRestore, nil
	}
	return auditedTopologyMove, nil
}

func auditRecordsByDigest(records []storedAuditRecord) map[string]storedAuditRecord {
	result := make(map[string]storedAuditRecord, len(records))
	for _, record := range records {
		result[record.digest] = record
	}
	return result
}

func validateStoredAuditRecordSchemas(records map[string][]storedAuditRecord) error {
	kinds := make([]string, 0, len(records))
	for kind := range records {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	for _, kind := range kinds {
		stored := records[kind]
		for _, record := range stored {
			if err := audit.Validate(record.record); err != nil {
				return fmt.Errorf("validating stored %s audit record: %w", kind, err)
			}
		}
	}
	return nil
}

func newAuditedHistoryReplay(
	authority initialAuditAuthority, scope initialAuditScope,
	initial map[string][]storedAuditRecord,
) (*auditedHistoryReplay, error) {
	baseline := initial["enrollment_baseline"][0].record
	members, err := auditUnsignedListField(baseline, "members")
	if err != nil {
		return nil, err
	}
	states, err := auditRecordListField(baseline, "member_states")
	if err != nil {
		return nil, err
	}
	versions, err := auditRecordListField(baseline, "versions")
	if err != nil {
		return nil, err
	}
	attachments, err := auditRecordListField(
		initial["attached_metadata_genesis"][0].record, "records",
	)
	if err != nil {
		return nil, err
	}
	if err := validateGenesisProvenanceIngests(attachments); err != nil {
		return nil, err
	}
	topology, err := auditRecordListField(initial["topology_genesis"][0].record, "nodes")
	if err != nil {
		return nil, err
	}
	nodeHighWater, err := auditUnsignedField(initial["allocation_genesis"][0].record, "node_id_high_water")
	if err != nil {
		return nil, err
	}
	initialBaseline := initial["enrollment_baseline"][0]
	replay := &auditedHistoryReplay{
		scopeID:          scope.scopeID,
		lineageID:        authority.lineageID,
		members:          members,
		memberSet:        auditMemberSet(members),
		states:           make(map[uint64]audit.Record, len(states)),
		versions:         make(map[string]audit.Record, len(versions)),
		attachments:      make(map[string]audit.Record, len(attachments)),
		tagDefinitionIDs: make(map[string]bool),
		topology:         slices.Clone(topology),
		topologyIndex:    make(map[uint64]int, len(topology)),
		scopeHead:        initial["scope_chain_entry"][0].digest,
		scopeEntryCount:  1,
		allocationHead:   initial["allocation_entry"][0].digest,
		allocationCount:  1,
		nodeHighWater:    nodeHighWater,
		baselines: map[string]auditBaselineProjection{
			initialBaseline.digest: {
				scopeID: scope.scopeID, operationID: scope.operationID,
				targetNodeID: scope.targetNodeID,
			},
		},
		memberBaselines: make(map[uint64]string, len(members)),
		scopes:          make(map[string]*auditScopeReplay),
	}
	for _, member := range members {
		replay.memberBaselines[member] = initialBaseline.digest
	}
	for _, state := range states {
		nodeID, err := auditUnsignedField(state, metadataNodeIDField)
		if err != nil {
			return nil, err
		}
		replay.states[nodeID] = state
	}
	for _, version := range versions {
		versionID, err := auditUUIDField(version, "version_id")
		if err != nil {
			return nil, err
		}
		replay.versions[versionID] = version
	}
	for _, attachment := range attachments {
		key, err := attachedAuditKey(attachment)
		if err != nil {
			return nil, err
		}
		if _, exists := replay.attachments[key]; exists {
			return nil, errors.New("audit attachment genesis repeats an identity")
		}
		replay.attachments[key] = attachment
		if attachment.Kind == auditTagDefinitionKind {
			tagID, err := auditUUIDField(attachment, "tag_id")
			if err != nil {
				return nil, err
			}
			replay.tagDefinitionIDs[tagID] = true
		}
	}
	for index, node := range topology {
		nodeID, err := auditUnsignedField(node, metadataNodeIDField)
		if err != nil {
			return nil, err
		}
		replay.topologyIndex[nodeID] = index
	}
	replay.saveActiveScope()
	replay.scopes[scope.scopeID].projection = scope
	return replay, nil
}

func (replay *auditedHistoryReplay) saveActiveScope() {
	if replay.scopeID == "" {
		return
	}
	current := replay.scopes[replay.scopeID]
	if current == nil {
		current = &auditScopeReplay{projection: initialAuditScope{scopeID: replay.scopeID}}
		replay.scopes[replay.scopeID] = current
	}
	current.members = slices.Clone(replay.members)
	current.memberSet = replay.memberSet
	current.memberBaselines = replay.memberBaselines
	current.head = replay.scopeHead
	current.entryCount = replay.scopeEntryCount
}

func (replay *auditedHistoryReplay) activateScope(scopeID string) error {
	if replay.scopeID == scopeID {
		return nil
	}
	replay.saveActiveScope()
	next := replay.scopes[scopeID]
	if next == nil {
		return fmt.Errorf("audit mutation references unknown scope %s", scopeID)
	}
	replay.scopeID = scopeID
	replay.members = slices.Clone(next.members)
	replay.memberSet = next.memberSet
	replay.memberBaselines = next.memberBaselines
	replay.scopeHead = next.head
	replay.scopeEntryCount = next.entryCount
	return nil
}

func validateGenesisProvenanceIngests(attachments []audit.Record) error {
	ingests := make(map[string]bool)
	for _, record := range attachments {
		if record.Kind != metadataIngestType {
			continue
		}
		ingestID, err := auditUUIDField(record, "ingest_id")
		if err != nil {
			return fmt.Errorf("reading genesis ingest identity: %w", err)
		}
		ingests[ingestID] = true
	}
	for _, record := range attachments {
		if record.Kind != metadataProvenanceType {
			continue
		}
		ingestID, err := auditUUIDField(record, "ingest_id")
		if err != nil {
			return fmt.Errorf("reading genesis provenance ingest: %w", err)
		}
		if !ingests[ingestID] {
			return fmt.Errorf("audit attachment genesis provenance references missing ingest %s", ingestID)
		}
	}
	return nil
}

func auditRecordsBySequence(
	records []storedAuditRecord, highWater int64,
) (map[int64]storedAuditRecord, error) {
	result, err := auditRecordsByOptionalSequence(records, highWater)
	if err != nil {
		return nil, err
	}
	if int64(len(result)) != highWater {
		kind := "record"
		if len(records) != 0 {
			kind = records[0].record.Kind
		}
		return nil, fmt.Errorf("audit %s sequence is incomplete", kind)
	}
	return result, nil
}

func auditRecordsByOptionalSequence(
	records []storedAuditRecord, highWater int64,
) (map[int64]storedAuditRecord, error) {
	result := make(map[int64]storedAuditRecord, len(records))
	for _, record := range records {
		if record.index.operationSequence == nil {
			return nil, fmt.Errorf("audit %s record lacks an operation sequence", record.record.Kind)
		}
		sequence := *record.index.operationSequence
		if sequence < 1 || sequence > highWater || result[sequence].record.Kind != "" {
			return nil, fmt.Errorf("audit %s has invalid or duplicate operation sequence %d",
				record.record.Kind, sequence)
		}
		result[sequence] = record
	}
	return result, nil
}

func auditScopeRecordsByCount(
	records []storedAuditRecord, scope initialAuditScope,
) (map[int64]storedAuditRecord, error) {
	result := make(map[int64]storedAuditRecord, len(records))
	for _, record := range records {
		if record.index.scopeID == nil || *record.index.scopeID != scope.scopeID ||
			record.index.entryCount == nil {
			return nil, errors.New("audit scope-chain record has invalid relational identity")
		}
		count := *record.index.entryCount
		if count < 1 || count > scope.entryCount || result[count].record.Kind != "" {
			return nil, fmt.Errorf("audit scope chain has invalid or duplicate entry %d", count)
		}
		result[count] = record
	}
	if int64(len(result)) != scope.entryCount {
		return nil, errors.New("audit scope chain is incomplete")
	}
	return result, nil
}

func auditScopeRecordsByScope(
	records []storedAuditRecord, scopes []initialAuditScope,
) (map[string]map[int64]storedAuditRecord, error) {
	projections := make(map[string]initialAuditScope, len(scopes))
	result := make(map[string]map[int64]storedAuditRecord, len(scopes))
	for _, scope := range scopes {
		if scope.scopeID == "" || scope.entryCount < 1 {
			return nil, errors.New("audit scope projection has invalid identity or entry count")
		}
		if _, exists := projections[scope.scopeID]; exists {
			return nil, errors.New("audit scope projection repeats an identity")
		}
		projections[scope.scopeID] = scope
		result[scope.scopeID] = make(map[int64]storedAuditRecord, scope.entryCount)
	}
	for _, record := range records {
		if record.index.scopeID == nil || record.index.entryCount == nil {
			return nil, errors.New("audit scope-chain record lacks relational identity")
		}
		scopeID, count := *record.index.scopeID, *record.index.entryCount
		projection, ok := projections[scopeID]
		if !ok || count < 1 || count > projection.entryCount ||
			result[scopeID][count].record.Kind != "" {
			return nil, errors.New("audit scope-chain record has invalid or duplicate identity")
		}
		result[scopeID][count] = record
	}
	for scopeID, projection := range projections {
		if int64(len(result[scopeID])) != projection.entryCount {
			return nil, fmt.Errorf("audit scope chain %s is incomplete", scopeID)
		}
	}
	return result, nil
}

func auditMutationScopeID(mutation audit.Record) (string, error) {
	var scopeID string
	consider := func(record audit.Record) error {
		candidate, err := auditUUIDField(record, auditScopeIDField)
		if err != nil {
			return err
		}
		if scopeID != "" && scopeID != candidate {
			return errors.New("audit mutation spans multiple scopes")
		}
		scopeID = candidate
		return nil
	}
	bindings, err := auditRecordListField(mutation, "baselines")
	if err != nil {
		return "", err
	}
	for _, binding := range bindings {
		if err := consider(binding); err != nil {
			return "", err
		}
	}
	events, err := auditRecordListField(mutation, "events")
	if err != nil {
		return "", err
	}
	for _, event := range events {
		if err := consider(event); err != nil {
			return "", err
		}
	}
	if scopeID == "" {
		return "", errors.New("audited mutation has no scope identity")
	}
	return scopeID, nil
}

func auditEventRecordsByID(records []storedAuditRecord) (map[string]storedAuditRecord, error) {
	result := make(map[string]storedAuditRecord, len(records))
	for _, record := range records {
		if record.index.eventID == nil || result[*record.index.eventID].record.Kind != "" {
			return nil, errors.New("audit event records contain an invalid or duplicate identity")
		}
		result[*record.index.eventID] = record
	}
	return result, nil
}

func (replay *auditedHistoryReplay) applyContentTransition(
	vaultID string, mutation, allocation, scopeEntry storedAuditRecord,
	eventRecords map[string]storedAuditRecord, usedEvents map[string]bool,
) error {
	sequence := *mutation.index.operationSequence
	auditSequence, err := positiveAuditInteger("operation sequence", sequence)
	if err != nil {
		return err
	}
	operationID, err := auditUUIDField(mutation.record, auditOperationIDField)
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
	events, err := auditRecordListField(mutation.record, "events")
	if err != nil {
		return err
	}
	if len(events) != 1 {
		return errors.New("content mutation must contain one scope event")
	}
	nodeID, postVersion, err := replay.validateContentTransitionEvent(
		operationID, mutation.record, events[0], eventRecords, usedEvents,
	)
	if err != nil {
		return err
	}
	if err := replay.validateContentTransitionStateChange(mutation.record, nodeID, postVersion); err != nil {
		return err
	}
	baselines, err := auditRecordListField(mutation.record, "baselines")
	if err != nil {
		return err
	}
	if len(baselines) != 0 {
		return errors.New("content mutation cannot bind an enrollment baseline")
	}
	if err := requireNoChangeMutationFields(mutation.record); err != nil {
		return err
	}
	if err := replay.advanceScope(vaultID, mutation, scopeEntry); err != nil {
		return err
	}
	if err := replay.advanceAllocation(
		vaultID, operationID, mutation, allocation, "", 0,
	); err != nil {
		return err
	}
	return replay.applyContentTransitionState(nodeID, postVersion, mutation.record)
}

func (replay *auditedHistoryReplay) validateContentTransitionEvent(
	operationID string, mutation, event audit.Record,
	eventRecords map[string]storedAuditRecord, usedEvents map[string]bool,
) (uint64, audit.Record, error) {
	eventID, err := auditDigestField(event, "event_id")
	if err != nil {
		return 0, audit.Record{}, err
	}
	wrapper, ok := eventRecords[eventID]
	if !ok || usedEvents[eventID] {
		return 0, audit.Record{}, errors.New("content mutation lacks one unique event wrapper")
	}
	wrapped, err := auditNestedField(wrapper.record, auditEventField)
	if err != nil || !auditRecordEqual(wrapped, event) {
		return 0, audit.Record{}, errors.New("content event wrapper does not match mutation")
	}
	usedEvents[eventID] = true
	identityOperation, err := audit.UUID(operationID)
	if err != nil {
		return 0, audit.Record{}, err
	}
	identity, err := hashAuditRecord(audit.Record{Kind: "event_identity", Fields: []audit.Field{
		{Name: auditOperationIDField, Value: identityOperation},
		{Name: auditEventOrdinalField, Value: audit.Unsigned(0)},
	}})
	if err != nil {
		return 0, audit.Record{}, err
	}
	if eventID != identity.text {
		return 0, audit.Record{}, errors.New("content event identity does not match its operation")
	}
	nodeID, err := auditUnsignedField(event, metadataNodeIDField)
	if err != nil || !replay.memberSet[nodeID] {
		return 0, audit.Record{}, fmt.Errorf("content mutation targets unaudited node %d", nodeID)
	}
	eventKind, err := auditTextField(event, "event_kind")
	if err != nil {
		return 0, audit.Record{}, err
	}
	if eventKind != "content_replace" && eventKind != "content_revert" {
		return 0, audit.Record{}, fmt.Errorf("unsupported audited content event %q", eventKind)
	}
	checks := []func() error{
		func() error { return requireAuditUUID(event, auditOperationIDField, operationID) },
		func() error { return requireAuditUUID(event, auditScopeIDField, replay.scopeID) },
		func() error { return requireAuditUnsigned(event, auditEventOrdinalField, 0) },
	}
	for _, check := range checks {
		if err := check(); err != nil {
			return 0, audit.Record{}, err
		}
	}
	if err := requireAuditAbsentFields(event, "target_node_id", "attachment_kind",
		"attachment_identity", auditTopologyDeltaField, "baseline_digest"); err != nil {
		return 0, audit.Record{}, err
	}
	if err := requireMatchingEventEnvelope(mutation, event); err != nil {
		return 0, audit.Record{}, err
	}
	state := replay.states[nodeID]
	priorRevision, err := auditUnsignedField(state, "node_revision")
	if err != nil {
		return 0, audit.Record{}, err
	}
	priorVersionID, err := auditOptionalUUIDField(state, "current_version_id")
	if err != nil || priorVersionID == nil {
		return 0, audit.Record{}, fmt.Errorf("audited file %d lacks a prior content head", nodeID)
	}
	if err := requireAuditUnsigned(event, "prior_node_revision", priorRevision); err != nil {
		return 0, audit.Record{}, err
	}
	if err := requireAuditUnsigned(event, "resulting_node_revision", priorRevision+1); err != nil {
		return 0, audit.Record{}, err
	}
	if err := requireAuditOptionalUUID(event, "prior_current_version_id", priorVersionID); err != nil {
		return 0, audit.Record{}, err
	}
	pre, err := auditNestedField(event, "pre")
	if err != nil || !auditRecordEqual(pre, replay.versions[*priorVersionID]) {
		return 0, audit.Record{}, errors.New("content pre-version does not match replayed head")
	}
	post, err := auditNestedField(event, "post")
	if err != nil {
		return 0, audit.Record{}, err
	}
	postVersionID, err := auditUUIDField(post, "version_id")
	if err != nil {
		return 0, audit.Record{}, err
	}
	if _, exists := replay.versions[postVersionID]; exists {
		return 0, audit.Record{}, errors.New("content mutation reuses a version identity")
	}
	if err := requireAuditUnsigned(post, metadataNodeIDField, nodeID); err != nil {
		return 0, audit.Record{}, err
	}
	if err := requireAuditUnsigned(post, "node_revision", priorRevision+1); err != nil {
		return 0, audit.Record{}, err
	}
	if err := requireAuditUUID(post, "introduced_operation_id", operationID); err != nil {
		return 0, audit.Record{}, err
	}
	if err := requireAuditText(post, "transition_kind", eventKind); err != nil {
		return 0, audit.Record{}, err
	}
	if err := replay.validateContentTransitionSource(
		event, post, eventKind, nodeID, *priorVersionID, priorRevision,
	); err != nil {
		return 0, audit.Record{}, err
	}
	postTime, err := auditField(post, auditRecordedAtField)
	if err != nil {
		return 0, audit.Record{}, err
	}
	mutationTime, err := auditField(mutation, auditRecordedAtField)
	if err != nil || !equalAuditEnvelopeValue(postTime, mutationTime) {
		return 0, audit.Record{}, errors.New("content version time does not match its mutation")
	}
	if err := requireAuditOptionalUUID(event, "resulting_current_version_id", &postVersionID); err != nil {
		return 0, audit.Record{}, err
	}
	return nodeID, post, nil
}

func (replay *auditedHistoryReplay) validateContentTransitionSource(
	event, post audit.Record, eventKind string,
	nodeID uint64, priorVersionID string, priorRevision uint64,
) error {
	eventSourceID, err := auditOptionalUUIDField(event, "source_version_id")
	if err != nil {
		return err
	}
	postSourceID, err := auditOptionalUUIDField(post, "source_version_id")
	if err != nil {
		return err
	}
	if eventKind == "content_replace" {
		if eventSourceID != nil || postSourceID != nil {
			return errors.New("content replacement carries a revert source")
		}
		return nil
	}
	if eventSourceID == nil || postSourceID == nil || *eventSourceID != *postSourceID {
		return errors.New("content revert event and version do not share one source")
	}
	if *eventSourceID == priorVersionID {
		return errors.New("content revert source is already the current head")
	}
	source, ok := replay.versions[*eventSourceID]
	if !ok {
		return fmt.Errorf("content revert references missing source version %s", *eventSourceID)
	}
	if err := requireAuditUnsigned(source, metadataNodeIDField, nodeID); err != nil {
		return errors.New("content revert source belongs to another node")
	}
	sourceRevision, err := auditUnsignedField(source, "node_revision")
	if err != nil {
		return err
	}
	if sourceRevision >= priorRevision+1 {
		return errors.New("content revert source is not older than the resulting revision")
	}
	sourceHash, err := auditDigestField(source, "blob_hash")
	if err != nil {
		return err
	}
	if err := requireAuditDigest(post, "blob_hash", sourceHash); err != nil {
		return errors.New("content revert bytes do not match its source")
	}
	sourceSize, err := auditUnsignedField(source, "size")
	if err != nil {
		return err
	}
	if err := requireAuditUnsigned(post, "size", sourceSize); err != nil {
		return errors.New("content revert size does not match its source")
	}
	sourceMIME, err := auditField(source, "media_type")
	if err != nil {
		return err
	}
	postMIME, err := auditField(post, "media_type")
	if err != nil {
		return err
	}
	if !equalAuditEnvelopeValue(sourceMIME, postMIME) {
		return errors.New("content revert MIME type does not match its source")
	}
	return nil
}

func (replay *auditedHistoryReplay) validateContentTransitionStateChange(
	mutation audit.Record, nodeID uint64, postVersion audit.Record,
) error {
	changes, err := auditRecordListField(mutation, "member_state_changes")
	if err != nil {
		return err
	}
	if len(changes) != 1 {
		return errors.New("content mutation must contain one member-state change")
	}
	state := replay.states[nodeID]
	priorRevision, err := auditUnsignedField(state, "node_revision")
	if err != nil {
		return err
	}
	priorVersionID, err := auditOptionalUUIDField(state, "current_version_id")
	if err != nil {
		return err
	}
	postVersionID, err := auditUUIDField(postVersion, "version_id")
	if err != nil {
		return err
	}
	change := changes[0]
	if err := requireAuditUnsigned(change, metadataNodeIDField, nodeID); err != nil {
		return err
	}
	if err := requireAuditUnsigned(change, "prior_revision", priorRevision); err != nil {
		return err
	}
	if err := requireAuditUnsigned(change, "resulting_revision", priorRevision+1); err != nil {
		return err
	}
	if err := requireAuditOptionalUUID(change, "prior_current_version_id", priorVersionID); err != nil {
		return err
	}
	return requireAuditOptionalUUID(change, "resulting_current_version_id", &postVersionID)
}

func (replay *auditedHistoryReplay) advanceScope(
	vaultID string, mutation, entry storedAuditRecord,
) error {
	nextCount := replay.scopeEntryCount + 1
	if entry.index.entryCount == nil || *entry.index.entryCount != nextCount {
		return errors.New("audited mutation scope entry is out of order")
	}
	if err := requireAuditUUID(entry.record, auditVaultIDField, vaultID); err != nil {
		return err
	}
	if err := requireAuditUUID(entry.record, auditScopeIDField, replay.scopeID); err != nil {
		return err
	}
	if err := requireAuditDigest(entry.record, "previous_head", replay.scopeHead); err != nil {
		return err
	}
	mutationDigest, err := hashAuditRecord(mutation.record)
	if err != nil {
		return err
	}
	if err := requireAuditDigest(entry.record, "mutation_hash", mutationDigest.text); err != nil {
		return err
	}
	replay.scopeEntryCount, replay.scopeHead = nextCount, entry.digest
	return nil
}

func (replay *auditedHistoryReplay) advanceAllocation(
	vaultID string, operationID string,
	mutation, entry storedAuditRecord, attachedDigest string, attachedCount uint64,
) error {
	nextCount, err := replay.validateAllocationBase(vaultID, operationID, entry)
	if err != nil {
		return err
	}
	if err := requireAuditBool(entry.record, "has_audited_mutation", true); err != nil {
		return err
	}
	mutationDigest, err := hashAuditRecord(mutation.record)
	if err != nil {
		return err
	}
	if err := requireAuditDigest(entry.record, "mutation_hash", mutationDigest.text); err != nil {
		return err
	}
	if attachedDigest == "" {
		if err := requireAuditBool(entry.record, "has_attached_metadata_change", false); err != nil {
			return err
		}
		if err := requireAuditUnsigned(
			entry.record, auditAttachedMetadataChangeCountField, 0,
		); err != nil {
			return err
		}
		if err := requireAuditAbsent(entry.record, "attached_metadata_change_digest"); err != nil {
			return err
		}
	} else {
		if attachedCount == 0 {
			return errors.New("audit allocation has a digest without attached-metadata changes")
		}
		if err := requireAuditBool(entry.record, "has_attached_metadata_change", true); err != nil {
			return err
		}
		if err := requireAuditUnsigned(
			entry.record, auditAttachedMetadataChangeCountField, attachedCount,
		); err != nil {
			return err
		}
		if err := requireAuditDigest(
			entry.record, "attached_metadata_change_digest", attachedDigest,
		); err != nil {
			return err
		}
	}
	replay.allocationCount, replay.allocationHead = nextCount, entry.digest
	return nil
}

func (replay *auditedHistoryReplay) validateAllocationBase(
	vaultID, operationID string, entry storedAuditRecord,
) (int64, error) {
	nextCount := replay.allocationCount + 1
	auditCount, err := positiveAuditInteger("allocation entry count", nextCount)
	if err != nil {
		return 0, err
	}
	if entry.index.operationSequence == nil || *entry.index.operationSequence != nextCount {
		return 0, errors.New("audit allocation entry is out of order")
	}
	checks := []func() error{
		func() error { return requireAuditUUID(entry.record, auditVaultIDField, vaultID) },
		func() error { return requireAuditUUID(entry.record, "lineage_id", replay.lineageID) },
		func() error { return requireAuditUUID(entry.record, auditOperationIDField, operationID) },
		func() error { return requireAuditDigest(entry.record, "previous_head", replay.allocationHead) },
		func() error { return requireAuditUnsigned(entry.record, "operation_sequence", auditCount) },
		func() error {
			return requireAuditUnsigned(entry.record, "operation_sequence_high_water", auditCount)
		},
		func() error { return requireAuditUnsigned(entry.record, "node_id_high_water", replay.nodeHighWater) },
		func() error { return requireAuditBool(entry.record, "has_topology_change", false) },
		func() error { return requireAuditBool(entry.record, "has_witness_change", false) },
		func() error { return requireAuditUnsigned(entry.record, auditWitnessChangeCountField, 0) },
		func() error {
			return requireAuditAbsentFields(
				entry.record, auditTopologyDeltaField, "witness_change_digest",
			)
		},
	}
	for _, check := range checks {
		if err := check(); err != nil {
			return 0, err
		}
	}
	allocated, err := auditUnsignedListField(entry.record, "allocated_node_ids")
	if err != nil {
		return 0, err
	}
	if len(allocated) != 0 {
		return 0, errors.New("audit allocation cannot allocate node IDs")
	}
	return nextCount, nil
}

func (replay *auditedHistoryReplay) applyContentTransitionState(
	nodeID uint64, postVersion, mutation audit.Record,
) error {
	state := replay.states[nodeID]
	priorRevision, err := auditUnsignedField(state, "node_revision")
	if err != nil {
		return err
	}
	postVersionID, err := auditUUIDField(postVersion, "version_id")
	if err != nil {
		return err
	}
	postVersionValue, err := audit.UUID(postVersionID)
	if err != nil {
		return err
	}
	replay.states[nodeID] = audit.Record{Kind: "member_state", Fields: []audit.Field{
		{Name: metadataNodeIDField, Value: audit.Unsigned(nodeID)},
		{Name: "node_revision", Value: audit.Unsigned(priorRevision + 1)},
		{Name: "current_version_id", Value: postVersionValue},
	}}
	replay.versions[postVersionID] = postVersion
	index, ok := replay.topologyIndex[nodeID]
	if !ok {
		return fmt.Errorf("audited node %d is absent from topology replay", nodeID)
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

func replaceAuditRecordField(record audit.Record, name string, value audit.Value) (audit.Record, error) {
	result := audit.Record{Kind: record.Kind, Fields: slices.Clone(record.Fields)}
	for index := range result.Fields {
		if result.Fields[index].Name == name {
			result.Fields[index].Value = value
			return result, nil
		}
	}
	return audit.Record{}, fmt.Errorf("audit record %s lacks field %s", record.Kind, name)
}

func (replay *auditedHistoryReplay) reconcileCurrentState(
	ctx context.Context, tx metadataQuerier,
) error {
	replay.saveActiveScope()
	if err := replay.reconcileMembershipProjection(ctx, tx); err != nil {
		return err
	}
	memberSet := make(map[uint64]bool)
	for _, scope := range replay.scopes {
		for _, member := range scope.members {
			memberSet[member] = true
		}
	}
	allMembers := sortedAuditMembers(memberSet)
	currentStates, err := currentAuditMemberStates(ctx, tx, allMembers)
	if err != nil {
		return err
	}
	expectedStates := make([]audit.Record, len(allMembers))
	for index, member := range allMembers {
		expectedStates[index] = replay.states[member]
	}
	if !equalAuditRecordLists(expectedStates, currentStates) {
		return errors.New("replayed audit member states do not match current nodes")
	}
	currentVersions, err := currentAuditVersions(ctx, tx, allMembers)
	if err != nil {
		return err
	}
	expectedVersions := make([]audit.Record, 0, len(replay.versions))
	for _, version := range replay.versions {
		expectedVersions = append(expectedVersions, version)
	}
	if !equalAuditRecordSets(expectedVersions, currentVersions) {
		return errors.New("replayed audit versions do not match retained content")
	}
	currentTopology, err := currentAuditTopology(ctx, tx)
	if err != nil {
		return err
	}
	if !equalAuditRecordLists(replay.topology, currentTopology) {
		return errors.New("replayed audit topology does not match current nodes")
	}
	currentAttachments, err := currentAuditAttachments(ctx, tx)
	if err != nil {
		return err
	}
	expectedAttachments := make([]audit.Record, 0, len(replay.attachments))
	for _, record := range replay.attachments {
		expectedAttachments = append(expectedAttachments, record)
	}
	if !equalAuditRecordSets(expectedAttachments, currentAttachments) {
		return errors.New("replayed audit attachments do not match current metadata")
	}
	return nil
}

func (replay *auditedHistoryReplay) reconcileMembershipProjection(
	ctx context.Context, tx metadataQuerier,
) error {
	rows, err := tx.QueryContext(ctx, `SELECT scope_id,node_id,baseline_digest
		FROM audit_memberships ORDER BY scope_id,node_id`)
	if err != nil {
		return fmt.Errorf("reading replayed audit memberships: %w", err)
	}
	defer func() { _ = rows.Close() }()
	members := make(map[string][]uint64, len(replay.scopes))
	for rows.Next() {
		var scopeID string
		var nodeID uint64
		var baseline string
		if err := rows.Scan(&scopeID, &nodeID, &baseline); err != nil {
			return fmt.Errorf("scanning replayed audit membership: %w", err)
		}
		scope := replay.scopes[scopeID]
		if scope == nil || scope.memberBaselines[nodeID] != baseline {
			return fmt.Errorf("audit membership for node %d does not match replayed baseline", nodeID)
		}
		members[scopeID] = append(members[scopeID], nodeID)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading replayed audit memberships: %w", err)
	}
	for scopeID, scope := range replay.scopes {
		if !slices.Equal(members[scopeID], scope.members) {
			return errors.New("audit membership projection does not match replayed history")
		}
	}
	baselineRows, err := tx.QueryContext(ctx, `SELECT digest,scope_id,target_node_id,operation_id
		FROM audit_baselines ORDER BY digest`)
	if err != nil {
		return fmt.Errorf("reading replayed audit baselines: %w", err)
	}
	defer func() { _ = baselineRows.Close() }()
	seen := make(map[string]bool, len(replay.baselines))
	for baselineRows.Next() {
		var digest, scopeID, operationID string
		var targetNodeID uint64
		if err := baselineRows.Scan(&digest, &scopeID, &targetNodeID, &operationID); err != nil {
			return fmt.Errorf("scanning replayed audit baseline: %w", err)
		}
		want, ok := replay.baselines[digest]
		if !ok || seen[digest] || want.scopeID != scopeID ||
			want.targetNodeID != targetNodeID || want.operationID != operationID {
			return errors.New("audit baseline projection does not match replayed history")
		}
		seen[digest] = true
	}
	if err := baselineRows.Err(); err != nil {
		return fmt.Errorf("reading replayed audit baselines: %w", err)
	}
	if len(seen) != len(replay.baselines) {
		return errors.New("audit baseline projection is incomplete")
	}
	return nil
}

func equalAuditRecordSets(left, right []audit.Record) bool {
	if len(left) != len(right) {
		return false
	}
	counts := make(map[string]int, len(left))
	for _, record := range left {
		encoded, err := audit.Encode(record)
		if err != nil {
			return false
		}
		counts[string(encoded)]++
	}
	for _, record := range right {
		encoded, err := audit.Encode(record)
		if err != nil || counts[string(encoded)] == 0 {
			return false
		}
		counts[string(encoded)]--
	}
	return true
}
