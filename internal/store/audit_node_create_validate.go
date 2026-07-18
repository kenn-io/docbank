package store

import (
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/docbank/internal/audit"
)

type replayedNodeCreation struct {
	childID, parentID   uint64
	childTopology       audit.Record
	postTopology        []audit.Record
	childState          audit.Record
	version             *audit.Record
	baselineAttachments []audit.Record
	attachmentChanges   []audit.Record
	attachmentDigest    string
	provenance          *audit.Record
	ingestID            string
	baselineDigest      string
	topologyDigest      string
	baseline            storedAuditRecord
}

func (replay *auditedHistoryReplay) applyNodeCreation(
	vaultID string, mutation, allocation, scopeEntry storedAuditRecord,
	baselineRecords, topologyRecords, attachmentRecords,
	eventRecords map[string]storedAuditRecord,
	usedBaselines, usedTopology, usedAttachments, usedEvents map[string]bool,
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
	creation, err := replay.validateNodeCreationAuthority(
		vaultID, mutation.record, operationID, baselineRecords, topologyRecords,
		usedBaselines, usedTopology,
	)
	if err != nil {
		return err
	}
	if err := replay.validateNodeCreationAttachedMetadata(
		mutation.record, operationID, &creation, attachmentRecords, usedAttachments,
	); err != nil {
		return err
	}
	if err := replay.validateNodeCreationEvents(
		mutation.record, operationID, creation, eventRecords, usedEvents,
	); err != nil {
		return err
	}
	if err := replay.validateNodeCreationParentChange(mutation.record, creation.parentID); err != nil {
		return err
	}
	if err := validateNodeCreationMutationDigests(mutation.record, creation); err != nil {
		return err
	}
	if err := replay.advanceScope(vaultID, mutation, scopeEntry); err != nil {
		return err
	}
	if err := replay.advanceNodeCreationAllocation(
		vaultID, operationID, mutation, allocation, creation,
	); err != nil {
		return err
	}
	return replay.applyNodeCreationState(operationID, creation)
}

func (replay *auditedHistoryReplay) validateNodeCreationAuthority(
	vaultID string, mutation audit.Record, operationID string,
	baselineRecords, topologyRecords map[string]storedAuditRecord,
	usedBaselines, usedTopology map[string]bool,
) (replayedNodeCreation, error) {
	bindings, err := auditRecordListField(mutation, "baselines")
	if err != nil {
		return replayedNodeCreation{}, err
	}
	if len(bindings) != 1 {
		return replayedNodeCreation{}, errors.New("node creation must bind one inherited baseline")
	}
	binding := bindings[0]
	if err := requireAuditUUID(binding, auditScopeIDField, replay.scopeID); err != nil {
		return replayedNodeCreation{}, err
	}
	childID, err := auditUnsignedField(binding, "target_node_id")
	if err != nil {
		return replayedNodeCreation{}, err
	}
	if replay.memberSet[childID] {
		return replayedNodeCreation{}, fmt.Errorf("node creation re-enrolls audited node %d", childID)
	}
	baselineDigest, err := auditDigestField(binding, "baseline_digest")
	if err != nil {
		return replayedNodeCreation{}, err
	}
	baseline, ok := baselineRecords[baselineDigest]
	if !ok || usedBaselines[baselineDigest] {
		return replayedNodeCreation{}, errors.New("node creation lacks one unique enrollment baseline")
	}
	topologyDigest, err := auditDigestField(mutation, auditTopologyDeltaField)
	if err != nil {
		return replayedNodeCreation{}, err
	}
	topology, ok := topologyRecords[topologyDigest]
	if !ok || usedTopology[topologyDigest] {
		return replayedNodeCreation{}, errors.New("node creation lacks one unique topology delta")
	}
	creation, err := replay.validateNodeCreationTopology(
		mutation, operationID, childID, topology.record,
	)
	if err != nil {
		return replayedNodeCreation{}, err
	}
	creation.baselineDigest, creation.baseline = baselineDigest, baseline
	creation.topologyDigest = topologyDigest
	if err := replay.validateNodeCreationBaseline(
		vaultID, mutation, operationID, &creation,
	); err != nil {
		return replayedNodeCreation{}, err
	}
	usedBaselines[baselineDigest], usedTopology[topologyDigest] = true, true
	return creation, nil
}

func (replay *auditedHistoryReplay) validateNodeCreationTopology(
	mutation audit.Record, operationID string, childID uint64, delta audit.Record,
) (replayedNodeCreation, error) {
	if err := requireAuditUUID(delta, auditOperationIDField, operationID); err != nil {
		return replayedNodeCreation{}, err
	}
	changes, err := auditRecordListField(delta, "changes")
	if err != nil {
		return replayedNodeCreation{}, err
	}
	if len(changes) != 2 {
		return replayedNodeCreation{}, errors.New("node creation topology must contain parent and child changes")
	}
	var childPost, parentPre, parentPost audit.Record
	var parentID uint64
	for _, change := range changes {
		nodeID, err := auditUnsignedField(change, metadataNodeIDField)
		if err != nil {
			return replayedNodeCreation{}, err
		}
		pre, preErr := optionalAuditNestedField(change, "pre")
		post, postErr := optionalAuditNestedField(change, "post")
		if preErr != nil || postErr != nil {
			return replayedNodeCreation{}, errors.Join(preErr, postErr)
		}
		if nodeID == childID {
			if pre != nil || post == nil {
				return replayedNodeCreation{}, errors.New("created node topology must have absent pre-state")
			}
			childPost = *post
			continue
		}
		if pre == nil || post == nil || parentPre.Kind != "" {
			return replayedNodeCreation{}, errors.New("node creation has an invalid parent topology change")
		}
		parentID, parentPre, parentPost = nodeID, *pre, *post
	}
	if childPost.Kind == "" || parentPre.Kind == "" {
		return replayedNodeCreation{}, errors.New("node creation topology is incomplete")
	}
	if !replay.memberSet[parentID] {
		return replayedNodeCreation{}, fmt.Errorf("created node parent %d is not audited", parentID)
	}
	parentIndex, ok := replay.topologyIndex[parentID]
	if !ok || !auditRecordEqual(replay.topology[parentIndex], parentPre) {
		return replayedNodeCreation{}, errors.New("node creation parent pre-state does not match replayed topology")
	}
	modifiedAt, err := auditField(mutation, auditRecordedAtField)
	if err != nil {
		return replayedNodeCreation{}, err
	}
	expectedParentPost, err := replaceAuditRecordField(parentPre, "modified_at", modifiedAt)
	if err != nil {
		return replayedNodeCreation{}, err
	}
	if !auditRecordEqual(expectedParentPost, parentPost) {
		return replayedNodeCreation{}, errors.New("node creation parent post-state is not an exact timestamp touch")
	}
	if _, exists := replay.topologyIndex[childID]; exists {
		return replayedNodeCreation{}, fmt.Errorf("created node %d already exists in replayed topology", childID)
	}
	if err := validateCreatedTopologyNode(childPost, childID, parentID, modifiedAt); err != nil {
		return replayedNodeCreation{}, err
	}
	postTopology := slices.Clone(replay.topology)
	postTopology[parentIndex] = parentPost
	postTopology = append(postTopology, childPost)
	if err := sortAuditTopologyRecords(postTopology); err != nil {
		return replayedNodeCreation{}, err
	}
	if err := validateReplayedAuditTopology(postTopology); err != nil {
		return replayedNodeCreation{}, fmt.Errorf("validating node creation topology: %w", err)
	}
	return replayedNodeCreation{
		childID: childID, parentID: parentID, childTopology: childPost,
		postTopology: postTopology,
	}, nil
}

func optionalAuditNestedField(record audit.Record, name string) (*audit.Record, error) {
	value, err := auditField(record, name)
	if err != nil {
		return nil, err
	}
	if value.IsAbsent() {
		return nil, nil //nolint:nilnil // nil is the canonical absent optional record.
	}
	nested, ok := value.RecordValue()
	if !ok {
		return nil, fmt.Errorf("audit field %s.%s is not an optional record", record.Kind, name)
	}
	return &nested, nil
}

func validateCreatedTopologyNode(
	node audit.Record, childID, parentID uint64, recordedAt audit.Value,
) error {
	checks := []func() error{
		func() error { return requireAuditUnsigned(node, metadataNodeIDField, childID) },
		func() error { return requireAuditUnsigned(node, "parent_id", parentID) },
		func() error { return requireAuditText(node, auditStateField, auditNodeStateLive) },
		func() error { return requireAuditAbsent(node, auditOriginField) },
		func() error { return requireAuditAbsent(node, "trashed_at") },
	}
	for _, check := range checks {
		if err := check(); err != nil {
			return err
		}
	}
	for _, field := range []string{"created_at", "modified_at"} {
		value, err := auditField(node, field)
		if err != nil {
			return err
		}
		if !equalAuditEnvelopeValue(value, recordedAt) {
			return fmt.Errorf("created node %s does not match its operation time", field)
		}
	}
	return nil
}

func (replay *auditedHistoryReplay) validateNodeCreationBaseline(
	vaultID string, mutation audit.Record, operationID string, creation *replayedNodeCreation,
) error {
	baseline := creation.baseline.record
	checks := []func() error{
		func() error { return requireAuditUUID(baseline, auditVaultIDField, vaultID) },
		func() error { return requireAuditUUID(baseline, auditScopeIDField, replay.scopeID) },
		func() error { return requireAuditUnsigned(baseline, "target_node_id", creation.childID) },
		func() error { return requireAuditUUID(baseline, auditOperationIDField, operationID) },
		func() error { return requireAuditText(baseline, "cause", "node_create") },
	}
	for _, check := range checks {
		if err := check(); err != nil {
			return err
		}
	}
	members, err := auditUnsignedListField(baseline, "members")
	if err != nil {
		return err
	}
	if !slices.Equal(members, []uint64{creation.childID}) {
		return errors.New("node creation baseline has an invalid member set")
	}
	states, err := auditRecordListField(baseline, "member_states")
	if err != nil {
		return err
	}
	if len(states) != 1 {
		return errors.New("node creation baseline must contain one member state")
	}
	state := states[0]
	if err := requireAuditUnsigned(state, metadataNodeIDField, creation.childID); err != nil {
		return err
	}
	if err := requireAuditUnsigned(state, "node_revision", 1); err != nil {
		return err
	}
	kind, err := auditTextField(creation.childTopology, "node_kind")
	if err != nil {
		return err
	}
	versions, err := auditRecordListField(baseline, "versions")
	if err != nil {
		return err
	}
	current, err := auditOptionalUUIDField(state, "current_version_id")
	if err != nil {
		return err
	}
	switch kind {
	case nodeKindDir:
		if current != nil || len(versions) != 0 {
			return errors.New("created directory baseline contains content")
		}
	case "file":
		if current == nil || len(versions) != 1 {
			return errors.New("created file baseline lacks its initial content")
		}
		if _, exists := replay.versions[*current]; exists {
			return fmt.Errorf("created file reuses protected content version %s", *current)
		}
		if err := validateCreatedContentVersion(
			versions[0], creation.childID, *current, operationID, mutation,
		); err != nil {
			return err
		}
		creation.version = &versions[0]
	default:
		return fmt.Errorf("created node has invalid kind %q", kind)
	}
	attachments, err := auditRecordListField(baseline, "attachments")
	if err != nil {
		return err
	}
	creation.baselineAttachments = attachments
	expectedNodes, expectedWitnesses, err := initialBaselineTopology(
		creation.postTopology, []uint64{creation.childID}, operationID,
	)
	if err != nil {
		return err
	}
	nodes, err := auditRecordListField(baseline, "nodes")
	if err != nil {
		return err
	}
	if !equalAuditRecordLists(nodes, expectedNodes) {
		return errors.New("node creation baseline topology does not match replayed post-state")
	}
	witnesses, err := auditRecordListField(baseline, "witnesses")
	if err != nil {
		return err
	}
	if !equalAuditRecordLists(witnesses, expectedWitnesses) {
		return errors.New("node creation baseline witnesses do not match replayed post-state")
	}
	creation.childState = state
	return nil
}

func validateCreatedContentVersion(
	version audit.Record, nodeID uint64, versionID, operationID string, mutation audit.Record,
) error {
	checks := []func() error{
		func() error { return requireAuditUUID(version, "version_id", versionID) },
		func() error { return requireAuditUnsigned(version, metadataNodeIDField, nodeID) },
		func() error { return requireAuditUnsigned(version, "node_revision", 1) },
		func() error { return requireAuditUUID(version, "introduced_operation_id", operationID) },
		func() error { return requireAuditText(version, "transition_kind", "content_create") },
		func() error { return requireAuditAbsent(version, "source_version_id") },
	}
	for _, check := range checks {
		if err := check(); err != nil {
			return err
		}
	}
	recordedAt, err := auditField(version, auditRecordedAtField)
	if err != nil {
		return err
	}
	mutationTime, err := auditField(mutation, auditRecordedAtField)
	if err != nil {
		return err
	}
	if !equalAuditEnvelopeValue(recordedAt, mutationTime) {
		return errors.New("created content version time does not match its mutation")
	}
	return nil
}

func (replay *auditedHistoryReplay) validateNodeCreationEvents(
	mutation audit.Record, operationID string, creation replayedNodeCreation,
	eventRecords map[string]storedAuditRecord, usedEvents map[string]bool,
) error {
	events, err := auditRecordListField(mutation, "events")
	if err != nil {
		return err
	}
	expectedKinds := []string{"audit_inherit", "node_create"}
	if creation.version != nil {
		expectedKinds = []string{"audit_inherit", "content_create", "node_create"}
	}
	if creation.provenance != nil {
		expectedKinds = append(expectedKinds, "provenance_add")
	}
	if len(events) != len(expectedKinds) {
		return errors.New("node creation has an invalid event set")
	}
	current, err := auditOptionalUUIDField(creation.childState, "current_version_id")
	if err != nil {
		return err
	}
	topologyDigest, err := auditDigestField(mutation, auditTopologyDeltaField)
	if err != nil {
		return err
	}
	ordinal := uint64(0)
	for index, event := range events {
		if err := validateAuditEventWrapper(
			operationID, ordinal, event, eventRecords, usedEvents,
		); err != nil {
			return err
		}
		checks := []func() error{
			func() error { return requireAuditUUID(event, auditOperationIDField, operationID) },
			func() error { return requireAuditUnsigned(event, metadataNodeIDField, creation.childID) },
			func() error { return requireAuditText(event, "event_kind", expectedKinds[index]) },
			func() error { return requireAuditUUID(event, auditScopeIDField, replay.scopeID) },
			func() error { return requireAuditUnsigned(event, auditEventOrdinalField, ordinal) },
			func() error { return requireAuditUnsigned(event, "prior_node_revision", 0) },
			func() error { return requireAuditUnsigned(event, "resulting_node_revision", 1) },
			func() error { return requireAuditAbsent(event, "prior_current_version_id") },
			func() error { return requireAuditOptionalUUID(event, "resulting_current_version_id", current) },
		}
		for _, check := range checks {
			if err := check(); err != nil {
				return err
			}
		}
		if err := requireMatchingEventEnvelope(mutation, event); err != nil {
			return err
		}
		if err := validateCreationEventPayload(
			event, expectedKinds[index], creation, topologyDigest,
		); err != nil {
			return err
		}
		ordinal++
	}
	return nil
}

func validateAuditEventWrapper(
	operationID string, ordinal uint64, event audit.Record,
	eventRecords map[string]storedAuditRecord, usedEvents map[string]bool,
) error {
	eventID, err := auditDigestField(event, "event_id")
	if err != nil {
		return err
	}
	wrapper, ok := eventRecords[eventID]
	if !ok || usedEvents[eventID] {
		return errors.New("audited mutation lacks one unique event wrapper")
	}
	wrapped, err := auditNestedField(wrapper.record, auditEventField)
	if err != nil {
		return err
	}
	if !auditRecordEqual(wrapped, event) {
		return errors.New("audit event wrapper does not match mutation")
	}
	operationValue, err := audit.UUID(operationID)
	if err != nil {
		return err
	}
	identity, err := hashAuditRecord(audit.Record{Kind: "event_identity", Fields: []audit.Field{
		{Name: auditOperationIDField, Value: operationValue},
		{Name: auditEventOrdinalField, Value: audit.Unsigned(ordinal)},
	}})
	if err != nil {
		return err
	}
	if identity.text != eventID {
		return errors.New("audit event identity does not match its operation")
	}
	usedEvents[eventID] = true
	return nil
}

func validateCreationEventPayload(
	event audit.Record, kind string, creation replayedNodeCreation, topologyDigest string,
) error {
	if err := requireAuditAbsent(event, "source_version_id"); err != nil {
		return err
	}
	if err := requireAuditAbsent(event, "pre"); err != nil {
		return err
	}
	if kind != "provenance_add" {
		if err := requireAuditAbsentFields(event, "attachment_kind", "attachment_identity"); err != nil {
			return err
		}
	}
	switch kind {
	case "audit_inherit":
		if err := requireAuditUnsigned(event, "target_node_id", creation.childID); err != nil {
			return err
		}
		if err := requireAuditDigest(event, "baseline_digest", creation.baselineDigest); err != nil {
			return err
		}
		return requireAuditAbsentFields(event, "post", auditTopologyDeltaField)
	case "content_create":
		post, err := auditNestedField(event, "post")
		if err != nil {
			return err
		}
		if creation.version == nil || !auditRecordEqual(post, *creation.version) {
			return errors.New("content-create event does not match its inherited baseline")
		}
		return requireAuditAbsentFields(event, "target_node_id", auditTopologyDeltaField, "baseline_digest")
	case "node_create":
		post, err := auditNestedField(event, "post")
		if err != nil {
			return err
		}
		if !auditRecordEqual(post, creation.childTopology) {
			return errors.New("node-create event does not match its inherited baseline")
		}
		if err := requireAuditDigest(event, auditTopologyDeltaField, topologyDigest); err != nil {
			return err
		}
		return requireAuditAbsentFields(event, "target_node_id", "baseline_digest")
	case "provenance_add":
		if creation.provenance == nil {
			return errors.New("provenance-add event has no replayed provenance")
		}
		post, err := auditNestedField(event, "post")
		if err != nil {
			return err
		}
		if !auditRecordEqual(post, *creation.provenance) {
			return errors.New("provenance-add event does not match its attached-metadata delta")
		}
		if err := requireAuditText(event, "attachment_kind", metadataProvenanceType); err != nil {
			return err
		}
		identity, err := attachedAuditIdentity(*creation.provenance)
		if err != nil {
			return err
		}
		storedIdentity, err := auditNestedField(event, "attachment_identity")
		if err != nil {
			return err
		}
		if !auditRecordEqual(identity, storedIdentity) {
			return errors.New("provenance-add event identity does not match its post record")
		}
		return requireAuditAbsentFields(
			event, "target_node_id", auditTopologyDeltaField, "baseline_digest",
		)
	default:
		return fmt.Errorf("invalid node creation event kind %q", kind)
	}
}

func (replay *auditedHistoryReplay) validateNodeCreationParentChange(
	mutation audit.Record, parentID uint64,
) error {
	changes, err := auditRecordListField(mutation, "member_state_changes")
	if err != nil {
		return err
	}
	if len(changes) != 1 {
		return errors.New("node creation must contain one parent member-state change")
	}
	state, ok := replay.states[parentID]
	if !ok {
		return fmt.Errorf("node creation parent %d lacks replayed member state", parentID)
	}
	priorRevision, err := auditUnsignedField(state, "node_revision")
	if err != nil {
		return err
	}
	current, err := auditOptionalUUIDField(state, "current_version_id")
	if err != nil {
		return err
	}
	change := changes[0]
	checks := []func() error{
		func() error { return requireAuditUnsigned(change, metadataNodeIDField, parentID) },
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
	return nil
}

func validateNodeCreationMutationDigests(
	mutation audit.Record, creation replayedNodeCreation,
) error {
	if err := requireAuditDigest(mutation, auditTopologyDeltaField, creation.topologyDigest); err != nil {
		return err
	}
	if err := requireAuditAbsentFields(
		mutation, "path_effect_digest", "witness_change_digest",
	); err != nil {
		return err
	}
	for _, field := range []string{"path_effect_count", auditWitnessChangeCountField} {
		if err := requireAuditUnsigned(mutation, field, 0); err != nil {
			return err
		}
	}
	if creation.provenance == nil {
		if err := requireAuditAbsentFields(
			mutation, "grouping_id", "attached_metadata_change_digest",
		); err != nil {
			return err
		}
		return requireAuditUnsigned(mutation, auditAttachedMetadataChangeCountField, 0)
	}
	if err := requireAuditUUID(mutation, "grouping_id", creation.ingestID); err != nil {
		return err
	}
	if err := requireAuditDigest(
		mutation, "attached_metadata_change_digest", creation.attachmentDigest,
	); err != nil {
		return err
	}
	if err := requireAuditUnsigned(
		mutation, auditAttachedMetadataChangeCountField, uint64(len(creation.attachmentChanges)),
	); err != nil {
		return err
	}
	return nil
}

func (replay *auditedHistoryReplay) advanceNodeCreationAllocation(
	vaultID, operationID string, mutation, entry storedAuditRecord,
	creation replayedNodeCreation,
) error {
	nextCount := replay.allocationCount + 1
	auditCount, err := positiveAuditInteger("allocation entry count", nextCount)
	if err != nil {
		return err
	}
	if creation.childID != replay.nodeHighWater+1 {
		return fmt.Errorf("created node ID %d does not follow allocation high-water %d",
			creation.childID, replay.nodeHighWater)
	}
	checks := []func() error{
		func() error { return requireAuditUUID(entry.record, auditVaultIDField, vaultID) },
		func() error { return requireAuditUUID(entry.record, "lineage_id", replay.lineageID) },
		func() error { return requireAuditUUID(entry.record, auditOperationIDField, operationID) },
		func() error { return requireAuditDigest(entry.record, "previous_head", replay.allocationHead) },
		func() error { return requireAuditUnsigned(entry.record, "operation_sequence", auditCount) },
		func() error { return requireAuditUnsigned(entry.record, "operation_sequence_high_water", auditCount) },
		func() error { return requireAuditUnsigned(entry.record, "node_id_high_water", creation.childID) },
		func() error { return requireAuditBool(entry.record, "has_audited_mutation", true) },
		func() error { return requireAuditBool(entry.record, "has_topology_change", true) },
		func() error { return requireAuditBool(entry.record, "has_witness_change", false) },
		func() error {
			return requireAuditBool(
				entry.record, "has_attached_metadata_change", creation.provenance != nil,
			)
		},
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
	if !slices.Equal(allocated, []uint64{creation.childID}) {
		return errors.New("node creation allocation does not name the created node")
	}
	mutationDigest, err := hashAuditRecord(mutation.record)
	if err != nil {
		return err
	}
	if err := requireAuditDigest(entry.record, "mutation_hash", mutationDigest.text); err != nil {
		return err
	}
	topologyDigest, err := auditDigestField(mutation.record, auditTopologyDeltaField)
	if err != nil {
		return err
	}
	if err := requireAuditDigest(entry.record, auditTopologyDeltaField, topologyDigest); err != nil {
		return err
	}
	if err := requireAuditUnsigned(entry.record, auditWitnessChangeCountField, 0); err != nil {
		return err
	}
	if err := requireAuditAbsent(entry.record, "witness_change_digest"); err != nil {
		return err
	}
	if creation.provenance == nil {
		if err := requireAuditUnsigned(entry.record, auditAttachedMetadataChangeCountField, 0); err != nil {
			return err
		}
		if err := requireAuditAbsent(entry.record, "attached_metadata_change_digest"); err != nil {
			return err
		}
	} else {
		if err := requireAuditUnsigned(
			entry.record, auditAttachedMetadataChangeCountField,
			uint64(len(creation.attachmentChanges)),
		); err != nil {
			return err
		}
		if err := requireAuditDigest(
			entry.record, "attached_metadata_change_digest", creation.attachmentDigest,
		); err != nil {
			return err
		}
	}
	replay.allocationCount, replay.allocationHead = nextCount, entry.digest
	return nil
}

func (replay *auditedHistoryReplay) applyNodeCreationState(
	operationID string, creation replayedNodeCreation,
) error {
	parentState := replay.states[creation.parentID]
	priorRevision, err := auditUnsignedField(parentState, "node_revision")
	if err != nil {
		return err
	}
	current, err := auditField(parentState, "current_version_id")
	if err != nil {
		return err
	}
	replay.states[creation.parentID] = audit.Record{Kind: "member_state", Fields: []audit.Field{
		{Name: metadataNodeIDField, Value: audit.Unsigned(creation.parentID)},
		{Name: "node_revision", Value: audit.Unsigned(priorRevision + 1)},
		{Name: "current_version_id", Value: current},
	}}
	replay.members = append(replay.members, creation.childID)
	slices.Sort(replay.members)
	replay.memberSet[creation.childID] = true
	replay.states[creation.childID] = creation.childState
	replay.memberBaselines[creation.childID] = creation.baselineDigest
	replay.baselines[creation.baselineDigest] = auditBaselineProjection{
		scopeID: replay.scopeID, operationID: operationID, targetNodeID: creation.childID,
	}
	if creation.version != nil {
		versionID, err := auditUUIDField(*creation.version, "version_id")
		if err != nil {
			return err
		}
		if _, exists := replay.versions[versionID]; exists {
			return fmt.Errorf("created file reuses protected content version %s", versionID)
		}
		replay.versions[versionID] = *creation.version
	}
	for _, change := range creation.attachmentChanges {
		post, err := validateAuditedIngestAddition(change)
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
		replay.attachments[key] = post
	}
	replay.topology = creation.postTopology
	replay.topologyIndex = make(map[uint64]int, len(replay.topology))
	for index, node := range replay.topology {
		nodeID, err := auditUnsignedField(node, metadataNodeIDField)
		if err != nil {
			return err
		}
		replay.topologyIndex[nodeID] = index
	}
	replay.nodeHighWater = creation.childID
	return nil
}
