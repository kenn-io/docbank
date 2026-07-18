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
		state, err := auditTextField(record, "state")
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
	scopeID         string
	lineageID       string
	members         []uint64
	memberSet       map[uint64]bool
	states          map[uint64]audit.Record
	versions        map[string]audit.Record
	attachments     map[string]audit.Record
	topology        []audit.Record
	topologyIndex   map[uint64]int
	scopeHead       string
	scopeEntryCount int64
	allocationHead  string
	allocationCount int64
	nodeHighWater   uint64
	baselines       map[string]auditBaselineProjection
	memberBaselines map[uint64]string
}

type auditBaselineProjection struct {
	scopeID, operationID string
	targetNodeID         uint64
}

func validateAuditedHistory(
	ctx context.Context, tx metadataQuerier, vaultID string, nodeSequence int64,
	authority initialAuditAuthority, scope initialAuditScope,
	records, initial map[string][]storedAuditRecord,
) error {
	if err := validateStoredAuditRecordSchemas(records); err != nil {
		return err
	}
	replay, err := newAuditedHistoryReplay(authority, scope, initial)
	if err != nil {
		return err
	}
	mutations, err := auditRecordsBySequence(records["canonical_mutation"], authority.sequence)
	if err != nil {
		return err
	}
	allocations, err := auditRecordsBySequence(records["allocation_entry"], authority.sequence)
	if err != nil {
		return err
	}
	entries, err := auditScopeRecordsByCount(records["scope_chain_entry"], scope)
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
		mutation := mutations[sequence]
		bindings, bindingErr := auditRecordListField(mutation.record, "baselines")
		if bindingErr != nil {
			return fmt.Errorf("validating audit operation %d: %w", sequence, bindingErr)
		}
		topologyValue, topologyErr := auditField(mutation.record, auditTopologyDeltaField)
		if topologyErr != nil {
			return fmt.Errorf("validating audit operation %d: %w", sequence, topologyErr)
		}
		if len(bindings) == 0 && topologyValue.IsAbsent() {
			err = replay.applyContentTransition(
				vaultID, mutation, allocations[sequence], entries[sequence], events, usedEvents,
			)
		} else if len(bindings) != 0 {
			err = replay.applyNodeCreation(
				vaultID, mutation, allocations[sequence], entries[sequence],
				baselines, topologyDeltas, attachmentDeltas, events, usedBaselines,
				usedTopologyDeltas, usedAttachmentDeltas, usedEvents,
			)
		} else {
			kind, kindErr := classifyAuditedTopologyMutation(mutation.record, topologyDeltas)
			if kindErr != nil {
				err = kindErr
			} else if kind == auditedTopologyTrash {
				err = replay.applyNodeTrash(
					vaultID, mutation, allocations[sequence], entries[sequence],
					topologyDeltas, pathEffectLists, events,
					usedTopologyDeltas, usedPathEffectLists, usedEvents,
				)
			} else {
				err = replay.applyNodeMove(
					vaultID, mutation, allocations[sequence], entries[sequence],
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
	if scope.entryCount != replay.scopeEntryCount || scope.chainHead != replay.scopeHead {
		return errors.New("audit scope authority does not match replayed history")
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

const (
	auditedTopologyMove  = "move"
	auditedTopologyTrash = "trash"
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
	kind := auditedTopologyMove
	for _, change := range changes {
		pre, preErr := auditNestedField(change, "pre")
		post, postErr := auditNestedField(change, "post")
		if preErr != nil || postErr != nil {
			return "", errors.New("topology mutation requires complete pre/post states")
		}
		preState, err := auditTextField(pre, "state")
		if err != nil {
			return "", err
		}
		postState, err := auditTextField(post, "state")
		if err != nil {
			return "", err
		}
		switch {
		case preState == auditNodeStateLive && postState == auditNodeStateLive:
		case preState == auditNodeStateLive && postState == auditNodeStateTrash:
			kind = auditedTopologyTrash
		default:
			return "", fmt.Errorf(
				"unsupported audited topology state transition %s to %s", preState, postState,
			)
		}
	}
	return kind, nil
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
		scopeID:         scope.scopeID,
		lineageID:       authority.lineageID,
		members:         members,
		memberSet:       auditMemberSet(members),
		states:          make(map[uint64]audit.Record, len(states)),
		versions:        make(map[string]audit.Record, len(versions)),
		attachments:     make(map[string]audit.Record, len(attachments)),
		topology:        slices.Clone(topology),
		topologyIndex:   make(map[uint64]int, len(topology)),
		scopeHead:       initial["scope_chain_entry"][0].digest,
		scopeEntryCount: 1,
		allocationHead:  initial["allocation_entry"][0].digest,
		allocationCount: 1,
		nodeHighWater:   nodeHighWater,
		baselines: map[string]auditBaselineProjection{
			initialBaseline.digest: {
				scopeID: scope.scopeID, operationID: scope.operationID,
				targetNodeID: scope.targetNodeID,
			},
		},
		memberBaselines: make(map[uint64]string, len(members)),
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
	}
	for index, node := range topology {
		nodeID, err := auditUnsignedField(node, metadataNodeIDField)
		if err != nil {
			return nil, err
		}
		replay.topologyIndex[nodeID] = index
	}
	return replay, nil
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
	if int64(len(result)) != highWater {
		return nil, fmt.Errorf("audit %s sequence is incomplete", records[0].record.Kind)
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
		vaultID, operationID, mutation, allocation,
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
		return errors.New("content mutation scope entry is out of order")
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
	mutation, entry storedAuditRecord,
) error {
	nextCount := replay.allocationCount + 1
	auditCount, err := positiveAuditInteger("allocation entry count", nextCount)
	if err != nil {
		return err
	}
	if entry.index.operationSequence == nil || *entry.index.operationSequence != nextCount {
		return errors.New("content mutation allocation entry is out of order")
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
		func() error { return requireAuditBool(entry.record, "has_audited_mutation", true) },
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
		return errors.New("content mutation allocation cannot allocate node IDs")
	}
	mutationDigest, err := hashAuditRecord(mutation.record)
	if err != nil {
		return err
	}
	if err := requireAuditDigest(entry.record, "mutation_hash", mutationDigest.text); err != nil {
		return err
	}
	for _, field := range []string{
		"has_topology_change", "has_witness_change", "has_attached_metadata_change",
	} {
		if err := requireAuditBool(entry.record, field, false); err != nil {
			return err
		}
	}
	for _, field := range []string{auditWitnessChangeCountField, auditAttachedMetadataChangeCountField} {
		if err := requireAuditUnsigned(entry.record, field, 0); err != nil {
			return err
		}
	}
	if err := requireAuditAbsentFields(entry.record, auditTopologyDeltaField,
		"witness_change_digest", "attached_metadata_change_digest"); err != nil {
		return err
	}
	replay.allocationCount, replay.allocationHead = nextCount, entry.digest
	return nil
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
	if err := replay.reconcileMembershipProjection(ctx, tx); err != nil {
		return err
	}
	currentStates, err := currentAuditMemberStates(ctx, tx, replay.members)
	if err != nil {
		return err
	}
	expectedStates := make([]audit.Record, len(replay.members))
	for index, member := range replay.members {
		expectedStates[index] = replay.states[member]
	}
	if !equalAuditRecordLists(expectedStates, currentStates) {
		return errors.New("replayed audit member states do not match current nodes")
	}
	currentVersions, err := currentAuditVersions(ctx, tx, replay.members)
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
	rows, err := tx.QueryContext(ctx, `SELECT node_id,baseline_digest FROM audit_memberships
		WHERE scope_id=? ORDER BY node_id`, replay.scopeID)
	if err != nil {
		return fmt.Errorf("reading replayed audit memberships: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var members []uint64
	for rows.Next() {
		var nodeID uint64
		var baseline string
		if err := rows.Scan(&nodeID, &baseline); err != nil {
			return fmt.Errorf("scanning replayed audit membership: %w", err)
		}
		if replay.memberBaselines[nodeID] != baseline {
			return fmt.Errorf("audit membership for node %d does not match replayed baseline", nodeID)
		}
		members = append(members, nodeID)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading replayed audit memberships: %w", err)
	}
	if !slices.Equal(members, replay.members) {
		return errors.New("audit membership projection does not match replayed history")
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
