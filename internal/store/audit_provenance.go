package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/audit"
)

const (
	auditCurrentVersionIDField = "current_version_id"
	auditNodeRevisionField     = "node_revision"
	auditPathEffectCountField  = "path_effect_count"
)

func persistAuditedProvenanceAppend(
	ctx context.Context, tx *sql.Tx, vaultID, operationID, recordedAt string,
	nodeSequence int64, authority auditAuthorityState, scopes []auditScopeState,
	priorNode, resultingNode Node, ingest metadataIngest, provenance metadataProvenance,
) error {
	eventKind := "provenance_add"
	if provenance.Supersedes != nil {
		eventKind = "provenance_supersede"
	}
	return persistAuditedProvenanceMutation(
		ctx, tx, vaultID, operationID, recordedAt, nodeSequence, authority, scopes,
		priorNode, resultingNode, ingest, provenance, nil, eventKind, true,
	)
}

func persistAuditedProvenanceMutation(
	ctx context.Context, tx *sql.Tx, vaultID, operationID, recordedAt string,
	nodeSequence int64, authority auditAuthorityState, scopes []auditScopeState,
	priorNode, resultingNode Node, ingest metadataIngest, provenance metadataProvenance,
	binding *ProvenanceVersionBinding, eventKind string, ingestAdded bool,
) error {
	sequence, err := nextAuditInteger("operation sequence", authority.sequence)
	if err != nil {
		return err
	}
	values, err := makeAuditedMutationValues(vaultID, authority.lineageID, operationID, recordedAt)
	if err != nil {
		return err
	}
	provenanceRecord, err := provenanceAuditRecord(
		provenance.Identity, provenance.NodeID, provenance.IngestID,
		provenance.OriginalPath, nullString(provenance.OriginalMTime), nullString(provenance.Supersedes),
	)
	if err != nil {
		return err
	}
	var priorProvenance *audit.Record
	if provenance.Supersedes != nil {
		prior, err := provenanceAuditRecordByIdentity(ctx, tx, *provenance.Supersedes)
		if err != nil {
			return err
		}
		priorProvenance = &prior
	}
	ingestRecord, err := ingestAuditRecord(ingest)
	if err != nil {
		return err
	}
	provenanceChange, err := makeAttachedMetadataAddition(provenanceRecord)
	if err != nil {
		return err
	}
	changes := make([]audit.Record, 0, 3)
	if ingestAdded {
		ingestChange, err := makeAttachedMetadataAddition(ingestRecord)
		if err != nil {
			return err
		}
		changes = append(changes, ingestChange)
	}
	changes = append(changes, provenanceChange)
	if binding != nil {
		bindingRecord, err := provenanceVersionBindingAuditRecord(*binding)
		if err != nil {
			return err
		}
		bindingChange, err := makeAttachedMetadataAddition(bindingRecord)
		if err != nil {
			return err
		}
		changes = append(changes, bindingChange)
	}
	delta, deltaDigest, err := makeAttachedMetadataDelta(values.operationID, changes)
	if err != nil {
		return err
	}
	events := make([]audit.Record, len(scopes))
	for index, scope := range scopes {
		events[index], err = makeAuditedProvenanceEvent(
			values, scope.scopeID, uint64(index), priorNode, resultingNode,
			priorProvenance, provenanceRecord, eventKind,
		)
		if err != nil {
			return err
		}
	}
	stateChange, err := makeAuditMemberStateChange(priorNode, resultingNode)
	if err != nil {
		return err
	}
	mutation, err := makeAuditedMemberStateMutation(values, sequence, events, stateChange)
	if err != nil {
		return err
	}
	mutation, err = replaceAuditRecordField(
		mutation, auditAttachedMetadataChangeCountField, audit.Unsigned(uint64(len(changes))),
	)
	if err != nil {
		return err
	}
	mutation, err = replaceAuditRecordField(mutation, "attached_metadata_change_digest", deltaDigest.value)
	if err != nil {
		return err
	}
	mutationDigest, err := hashAuditRecord(mutation)
	if err != nil {
		return err
	}
	if err := insertAuditRecord(ctx, tx, delta); err != nil {
		return err
	}
	for _, event := range events {
		if err := insertAuditRecord(ctx, tx, audit.Record{
			Kind: auditEventField, Fields: []audit.Field{{Name: auditEventField, Value: audit.Nested(event)}},
		}); err != nil {
			return err
		}
	}
	if err := insertAuditRecord(ctx, tx, mutation); err != nil {
		return err
	}
	if err := advanceAuditedMutationScopes(ctx, tx, values, scopes, mutationDigest.value); err != nil {
		return err
	}
	allocation, err := makeAuditAllocationEntry(
		values, sequence, nodeSequence, authority.allocationHead, mutationDigest.value,
	)
	if err != nil {
		return err
	}
	allocation, err = addAttachedMetadataToAllocation(allocation, uint64(len(changes)), deltaDigest.value)
	if err != nil {
		return err
	}
	return advanceAuditAuthority(ctx, tx, authority, sequence, allocation)
}

func provenanceAuditRecordByIdentity(
	ctx context.Context, tx *sql.Tx, identity string,
) (audit.Record, error) {
	var nodeID int64
	var ingestID, originalPath string
	var originalMTime, supersedes sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT node_id,ingest_id,original_path,
		original_mtime,supersedes FROM provenance WHERE identity=?`, identity).Scan(
		&nodeID, &ingestID, &originalPath, &originalMTime, &supersedes,
	); err != nil {
		return audit.Record{}, fmt.Errorf("reading superseded provenance %s: %w", identity, err)
	}
	return provenanceAuditRecord(identity, nodeID, ingestID, originalPath, originalMTime, supersedes)
}

func makeAuditedProvenanceEvent(
	values auditedMutationValues, scopeID string, ordinal uint64,
	priorNode, resultingNode Node, priorProvenance *audit.Record,
	provenance audit.Record, eventKind string,
) (audit.Record, error) {
	if priorNode.ID != resultingNode.ID {
		return audit.Record{}, errors.New("audited provenance changes node identity")
	}
	nodeID, err := positiveAuditNodeID(priorNode.ID)
	if err != nil {
		return audit.Record{}, err
	}
	priorRevision, err := positiveAuditRevision(priorNode.Revision)
	if err != nil {
		return audit.Record{}, err
	}
	resultingRevision, err := positiveAuditRevision(resultingNode.Revision)
	if err != nil || resultingRevision != priorRevision+1 {
		return audit.Record{}, errors.New("audited provenance has an invalid revision transition")
	}
	scopeValue, err := audit.UUID(scopeID)
	if err != nil {
		return audit.Record{}, err
	}
	identity, err := attachedAuditIdentity(provenance)
	if err != nil {
		return audit.Record{}, err
	}
	eventID, err := hashAuditRecord(audit.Record{Kind: auditEventIdentityKind, Fields: []audit.Field{
		{Name: auditOperationIDField, Value: values.operationID},
		{Name: auditEventOrdinalField, Value: audit.Unsigned(ordinal)},
	}})
	if err != nil {
		return audit.Record{}, err
	}
	eventKindValue, err := audit.Text(eventKind)
	if err != nil {
		return audit.Record{}, err
	}
	attachmentKind, err := audit.Text(metadataProvenanceType)
	if err != nil {
		return audit.Record{}, err
	}
	priorVersion, err := auditNodeCurrentVersion(priorNode)
	if err != nil {
		return audit.Record{}, err
	}
	resultingVersion, err := auditNodeCurrentVersion(resultingNode)
	if err != nil {
		return audit.Record{}, err
	}
	pre := audit.Absent()
	if priorProvenance != nil {
		pre = audit.Nested(*priorProvenance)
	}
	return audit.Record{Kind: "audit_event", Fields: []audit.Field{
		{Name: "event_id", Value: eventID.value},
		{Name: auditOperationIDField, Value: values.operationID},
		{Name: metadataNodeIDField, Value: audit.Unsigned(nodeID)},
		{Name: "event_kind", Value: eventKindValue},
		{Name: auditScopeIDField, Value: scopeValue},
		{Name: auditTargetNodeIDField, Value: audit.Absent()},
		{Name: "attachment_kind", Value: attachmentKind},
		{Name: "attachment_identity", Value: audit.Nested(identity)},
		{Name: auditSourceVersionIDField, Value: audit.Absent()},
		{Name: auditEventOrdinalField, Value: audit.Unsigned(ordinal)},
		{Name: auditRecordedAtField, Value: values.recordedAt},
		{Name: "prior_node_revision", Value: audit.Unsigned(priorRevision)},
		{Name: "resulting_node_revision", Value: audit.Unsigned(resultingRevision)},
		{Name: "prior_current_version_id", Value: priorVersion},
		{Name: "resulting_current_version_id", Value: resultingVersion},
		{Name: auditOriginField, Value: values.origin},
		{Name: auditAgentLabelField, Value: audit.Absent()},
		{Name: auditPreField, Value: pre},
		{Name: auditPostField, Value: audit.Nested(provenance)},
		{Name: auditTopologyDeltaField, Value: audit.Absent()},
		{Name: auditBaselineDigestField, Value: audit.Absent()},
	}}, nil
}

type replayedProvenanceMutation struct {
	nodeID      uint64
	ingest      audit.Record
	provenance  audit.Record
	binding     audit.Record
	digest      string
	eventKind   string
	ingestAdded bool
}

func (replay *auditedHistoryReplay) applyProvenanceMutation(
	vaultID string, mutation, allocation, scopeEntry storedAuditRecord,
	deltaRecords, eventRecords map[string]storedAuditRecord,
	usedDeltas, usedEvents map[string]bool,
) error {
	operationID, err := auditUUIDField(mutation.record, auditOperationIDField)
	if err != nil {
		return err
	}
	sequence := replay.allocationCount + 1
	if err := requireAuditUUID(mutation.record, auditVaultIDField, vaultID); err != nil {
		return err
	}
	auditSequence, err := positiveAuditInteger("operation sequence", sequence)
	if err != nil {
		return err
	}
	if err := requireAuditUnsigned(mutation.record, "operation_sequence", auditSequence); err != nil {
		return err
	}
	if err := requireAuditAbsent(mutation.record, "grouping_id"); err != nil {
		return err
	}
	eventKind, err := auditedMutationFirstEventKind(mutation.record)
	if err != nil {
		return err
	}
	var transition replayedProvenanceMutation
	if eventKind == "ingest_observe" {
		transition, err = replay.validateIngestObservationDelta(
			mutation.record, operationID, deltaRecords, usedDeltas,
		)
	} else {
		transition, err = replay.validateProvenanceAppendDelta(
			mutation.record, operationID, deltaRecords, usedDeltas,
		)
	}
	if err != nil {
		return err
	}
	if err := replay.validateProvenanceMutationEvent(
		operationID, mutation.record, transition, eventRecords, usedEvents,
	); err != nil {
		return err
	}
	if err := replay.validateMemberStateChanges(mutation.record, []uint64{transition.nodeID}); err != nil {
		return err
	}
	bindings, err := auditRecordListField(mutation.record, "baselines")
	if err != nil {
		return err
	}
	if len(bindings) != 0 {
		return errors.New("provenance mutation cannot bind an enrollment baseline")
	}
	if err := requireAuditAbsentFields(
		mutation.record, auditTopologyDeltaField, "path_effect_digest", "witness_change_digest",
	); err != nil {
		return err
	}
	for _, field := range []string{auditPathEffectCountField, auditWitnessChangeCountField} {
		if err := requireAuditUnsigned(mutation.record, field, 0); err != nil {
			return err
		}
	}
	changeCount := uint64(1)
	if transition.ingestAdded {
		changeCount = 2
	}
	if transition.binding.Kind != "" {
		changeCount++
	}
	if err := requireAuditUnsigned(mutation.record, auditAttachedMetadataChangeCountField, changeCount); err != nil {
		return err
	}
	if err := requireAuditDigest(mutation.record, "attached_metadata_change_digest", transition.digest); err != nil {
		return err
	}
	if err := replay.advanceScope(vaultID, mutation, scopeEntry); err != nil {
		return err
	}
	if err := replay.advanceAllocation(vaultID, operationID, mutation, allocation, transition.digest, changeCount); err != nil {
		return err
	}
	return replay.applyProvenanceMutationState(transition, mutation.record)
}

func (replay *auditedHistoryReplay) validateProvenanceAppendDelta(
	mutation audit.Record, operationID string,
	deltaRecords map[string]storedAuditRecord, usedDeltas map[string]bool,
) (replayedProvenanceMutation, error) {
	digest, err := auditDigestField(mutation, "attached_metadata_change_digest")
	if err != nil {
		return replayedProvenanceMutation{}, err
	}
	delta, ok := deltaRecords[digest]
	if !ok || usedDeltas[digest] {
		return replayedProvenanceMutation{}, errors.New("provenance mutation lacks one unique attached-metadata delta")
	}
	if err := requireAuditUUID(delta.record, auditOperationIDField, operationID); err != nil {
		return replayedProvenanceMutation{}, err
	}
	changes, err := auditRecordListField(delta.record, "changes")
	if err != nil || len(changes) != 2 {
		return replayedProvenanceMutation{}, errors.New("provenance mutation must contain ingest and provenance changes")
	}
	var change, ingestChange audit.Record
	for _, candidate := range changes {
		kind, kindErr := auditTextField(candidate, "record_kind")
		if kindErr != nil {
			return replayedProvenanceMutation{}, kindErr
		}
		_, hasPre, preErr := optionalNestedAuditRecord(candidate, auditPreField)
		if preErr != nil || hasPre {
			return replayedProvenanceMutation{}, errors.New("provenance mutation changes an existing attachment")
		}
		_, hasPost, postErr := optionalNestedAuditRecord(candidate, auditPostField)
		if postErr != nil || !hasPost {
			return replayedProvenanceMutation{}, errors.New("provenance mutation lacks an added attachment")
		}
		switch kind {
		case metadataProvenanceType:
			if change.Kind != "" {
				return replayedProvenanceMutation{}, errors.New("provenance mutation repeats its fact change")
			}
			change = candidate
		case metadataIngestType:
			if ingestChange.Kind != "" {
				return replayedProvenanceMutation{}, errors.New("provenance mutation repeats its ingest change")
			}
			ingestChange = candidate
		default:
			return replayedProvenanceMutation{}, fmt.Errorf("provenance mutation carries unsupported attachment %q", kind)
		}
	}
	if change.Kind == "" || ingestChange.Kind == "" {
		return replayedProvenanceMutation{}, errors.New("provenance mutation must add one ingest and one fact")
	}
	ingestPost, err := validateAuditedIngestAddition(ingestChange)
	if err != nil {
		return replayedProvenanceMutation{}, err
	}
	ingestKey, err := attachedAuditKey(ingestPost)
	if err != nil {
		return replayedProvenanceMutation{}, err
	}
	if _, exists := replay.attachments[ingestKey]; exists {
		return replayedProvenanceMutation{}, errors.New("provenance mutation reuses ingest identity")
	}
	post, _, err := optionalNestedAuditRecord(change, auditPostField)
	if err != nil {
		return replayedProvenanceMutation{}, err
	}
	ingestID, err := auditUUIDField(ingestPost, "ingest_id")
	if err != nil {
		return replayedProvenanceMutation{}, err
	}
	if err := validateReplayedIngest(ingestPost); err != nil {
		return replayedProvenanceMutation{}, err
	}
	if err := validateReplayedProvenance(post, ingestID); err != nil {
		return replayedProvenanceMutation{}, err
	}
	identity, err := attachedAuditIdentity(post)
	if err != nil {
		return replayedProvenanceMutation{}, err
	}
	storedIdentity, err := auditNestedField(change, "stable_identity")
	if err != nil || !auditRecordEqual(storedIdentity, identity) {
		return replayedProvenanceMutation{}, errors.New("provenance delta identity does not match its record")
	}
	nodeID, err := auditUnsignedField(post, metadataNodeIDField)
	if err != nil || !replay.memberSet[nodeID] {
		return replayedProvenanceMutation{}, fmt.Errorf("provenance mutation targets unaudited node %d", nodeID)
	}
	topologyIndex, ok := replay.topologyIndex[nodeID]
	if !ok {
		return replayedProvenanceMutation{}, fmt.Errorf("provenance mutation target %d is absent from topology", nodeID)
	}
	nodeKind, err := auditTextField(replay.topology[topologyIndex], "node_kind")
	if err != nil {
		return replayedProvenanceMutation{}, err
	}
	if nodeKind != nodeKindFile {
		return replayedProvenanceMutation{}, fmt.Errorf("provenance mutation targets non-file node %d", nodeID)
	}
	provenanceID, err := auditDigestField(post, "identity")
	if err != nil {
		return replayedProvenanceMutation{}, err
	}
	key, err := attachedAuditKey(post)
	if err != nil {
		return replayedProvenanceMutation{}, err
	}
	if _, exists := replay.attachments[key]; exists {
		return replayedProvenanceMutation{}, fmt.Errorf("provenance mutation reuses identity %s", provenanceID)
	}
	supersedes, err := auditOptionalDigestField(post, "supersedes")
	if err != nil {
		return replayedProvenanceMutation{}, err
	}
	if supersedes != nil {
		if err := replay.requireSupersedableProvenance(nodeID, *supersedes); err != nil {
			return replayedProvenanceMutation{}, err
		}
	}
	eventKind := "provenance_add"
	if supersedes != nil {
		eventKind = "provenance_supersede"
	}
	usedDeltas[digest] = true
	return replayedProvenanceMutation{
		nodeID: nodeID, provenance: post, digest: digest,
		ingest: ingestPost, ingestAdded: true, eventKind: eventKind,
	}, nil
}

func validateReplayedProvenance(record audit.Record, newIngest string) error {
	if record.Kind != metadataProvenanceType {
		return errors.New("provenance attachment has the wrong record kind")
	}
	nodeID, err := auditUnsignedField(record, metadataNodeIDField)
	if err != nil {
		return err
	}
	ingestID, err := auditUUIDField(record, "ingest_id")
	if err != nil {
		return err
	}
	if ingestID != newIngest {
		return errors.New("provenance fact does not bind its new ingest")
	}
	path, ok := auditFieldBytes(record, "original_path")
	if !ok || len(path) == 0 || !utf8.Valid(path) {
		return errors.New("provenance attachment has invalid original path")
	}
	mtime, err := auditOptionalTimestampField(record, "original_mtime")
	if err != nil {
		return err
	}
	supersedes, err := auditOptionalDigestField(record, "supersedes")
	if err != nil {
		return err
	}
	identity, err := auditDigestField(record, "identity")
	if err != nil {
		return err
	}
	ingestValue, err := audit.UUID(ingestID)
	if err != nil {
		return err
	}
	identityRecord := audit.Record{Kind: "provenance_identity", Fields: []audit.Field{
		{Name: "node_id", Value: audit.Unsigned(nodeID)},
		{Name: "ingest_id", Value: ingestValue},
		{Name: "original_path", Value: audit.Bytes(path)},
		{Name: "original_mtime", Value: audit.Absent()},
		{Name: "supersedes", Value: audit.Absent()},
	}}
	if mtime != nil {
		identityRecord.Fields[3].Value, err = audit.Timestamp(*mtime)
		if err != nil {
			return err
		}
	}
	if supersedes != nil {
		identityRecord.Fields[4].Value, err = audit.DigestHex(*supersedes)
		if err != nil {
			return err
		}
	}
	digest, err := hashAuditRecord(identityRecord)
	if err != nil {
		return err
	}
	if digest.text != identity {
		return errors.New("provenance attachment identity does not match its immutable fields")
	}
	return nil
}

func auditFieldBytes(record audit.Record, name string) ([]byte, bool) {
	value, err := auditField(record, name)
	if err != nil {
		return nil, false
	}
	bytes, ok := value.BytesValue()
	return bytes, ok
}

func (replay *auditedHistoryReplay) requireSupersedableProvenance(nodeID uint64, identity string) error {
	predecessor, err := replay.activeProvenanceRecord(nodeID, identity)
	if err != nil {
		return err
	}
	ingestID, err := auditField(predecessor, "ingest_id")
	if err != nil {
		return err
	}
	key, err := attachedAuditKey(audit.Record{Kind: metadataIngestType, Fields: []audit.Field{
		{Name: "ingest_id", Value: ingestID},
	}})
	if err != nil {
		return err
	}
	ingest, ok := replay.attachments[key]
	if !ok {
		return errors.New("provenance predecessor references missing ingest")
	}
	sourceKind, err := auditTextField(ingest, "source_kind")
	if err != nil {
		return err
	}
	if !sourceKindIsEmbedded(sourceKind) {
		return errors.New("operational ingest provenance cannot be superseded")
	}
	return nil
}

func (replay *auditedHistoryReplay) activeProvenanceRecord(
	nodeID uint64, identity string,
) (audit.Record, error) {
	var found bool
	var result audit.Record
	for _, record := range replay.attachments {
		if record.Kind != metadataProvenanceType {
			continue
		}
		candidateID, err := auditDigestField(record, "identity")
		if err != nil {
			return audit.Record{}, err
		}
		candidateNode, err := auditUnsignedField(record, metadataNodeIDField)
		if err != nil {
			return audit.Record{}, err
		}
		if candidateID == identity {
			if candidateNode != nodeID {
				return audit.Record{}, errors.New("provenance predecessor belongs to another node")
			}
			found = true
			result = record
		}
		superseded, err := auditOptionalDigestField(record, "supersedes")
		if err != nil {
			return audit.Record{}, err
		}
		if superseded != nil && *superseded == identity {
			return audit.Record{}, errors.New("provenance predecessor is already superseded")
		}
	}
	if !found {
		return audit.Record{}, errors.New("provenance predecessor is missing")
	}
	return result, nil
}

func (replay *auditedHistoryReplay) validateProvenanceMutationEvent(
	operationID string, mutation audit.Record, transition replayedProvenanceMutation,
	eventRecords map[string]storedAuditRecord, usedEvents map[string]bool,
) error {
	events, err := auditRecordListField(mutation, "events")
	if err != nil || len(events) != 1 {
		return errors.New("provenance mutation must contain one scope event")
	}
	event := events[0]
	if err := validateAuditEventWrapper(operationID, 0, event, eventRecords, usedEvents); err != nil {
		return err
	}
	nodeID := transition.nodeID
	identity, err := attachedAuditIdentity(transition.provenance)
	if err != nil {
		return err
	}
	storedIdentity, err := auditNestedField(event, "attachment_identity")
	if err != nil || !auditRecordEqual(storedIdentity, identity) {
		return errors.New("provenance event identity does not match its attachment")
	}
	eventKind := transition.eventKind
	var expectedPre audit.Record
	if eventKind == "provenance_supersede" {
		supersedes, err := auditOptionalDigestField(transition.provenance, "supersedes")
		if err != nil || supersedes == nil {
			return errors.New("provenance supersession lacks its predecessor")
		}
		expectedPre, err = replay.activeProvenanceRecord(nodeID, *supersedes)
		if err != nil {
			return err
		}
	}
	state := replay.states[nodeID]
	priorRevision, err := auditUnsignedField(state, auditNodeRevisionField)
	if err != nil {
		return err
	}
	current, err := auditOptionalUUIDField(state, auditCurrentVersionIDField)
	if err != nil {
		return err
	}
	checks := []func() error{
		func() error { return requireAuditUUID(event, auditOperationIDField, operationID) },
		func() error { return requireAuditUnsigned(event, metadataNodeIDField, nodeID) },
		func() error { return requireAuditText(event, "event_kind", eventKind) },
		func() error { return requireAuditUUID(event, auditScopeIDField, replay.scopeID) },
		func() error { return requireAuditText(event, "attachment_kind", metadataProvenanceType) },
		func() error { return requireAuditUnsigned(event, auditEventOrdinalField, 0) },
		func() error { return requireAuditUnsigned(event, "prior_node_revision", priorRevision) },
		func() error { return requireAuditUnsigned(event, "resulting_node_revision", priorRevision+1) },
		func() error { return requireAuditOptionalUUID(event, "prior_current_version_id", current) },
		func() error { return requireAuditOptionalUUID(event, "resulting_current_version_id", current) },
		func() error { return requireMatchingEventEnvelope(mutation, event) },
		func() error {
			return requireAuditAbsentFields(event, "target_node_id", "source_version_id", auditTopologyDeltaField, "baseline_digest")
		},
		func() error {
			pre, hasPre, err := optionalNestedAuditRecord(event, auditPreField)
			if err != nil {
				return err
			}
			if eventKind == "provenance_supersede" {
				if !hasPre || !auditRecordEqual(pre, expectedPre) {
					return errors.New("provenance event pre-state does not match its predecessor")
				}
				return nil
			}
			if hasPre {
				return errors.New("provenance event unexpectedly has a pre-state")
			}
			return nil
		},
	}
	for _, check := range checks {
		if err := check(); err != nil {
			return err
		}
	}
	post, err := auditNestedField(event, auditPostField)
	if err != nil || !auditRecordEqual(post, transition.provenance) {
		return errors.New("provenance event post-state does not match its delta")
	}
	return nil
}

func (replay *auditedHistoryReplay) applyProvenanceMutationState(
	transition replayedProvenanceMutation, mutation audit.Record,
) error {
	key, err := attachedAuditKey(transition.provenance)
	if err != nil {
		return err
	}
	replay.attachments[key] = transition.provenance
	if transition.binding.Kind != "" {
		bindingKey, err := attachedAuditKey(transition.binding)
		if err != nil {
			return err
		}
		replay.attachments[bindingKey] = transition.binding
	}
	if transition.ingestAdded {
		ingestKey, err := attachedAuditKey(transition.ingest)
		if err != nil {
			return err
		}
		replay.attachments[ingestKey] = transition.ingest
	}
	state := replay.states[transition.nodeID]
	revision, err := auditUnsignedField(state, auditNodeRevisionField)
	if err != nil {
		return err
	}
	current, err := auditField(state, auditCurrentVersionIDField)
	if err != nil {
		return err
	}
	replay.states[transition.nodeID] = audit.Record{Kind: "member_state", Fields: []audit.Field{
		{Name: metadataNodeIDField, Value: audit.Unsigned(transition.nodeID)},
		{Name: auditNodeRevisionField, Value: audit.Unsigned(revision + 1)},
		{Name: auditCurrentVersionIDField, Value: current},
	}}
	index, ok := replay.topologyIndex[transition.nodeID]
	if !ok {
		return fmt.Errorf("audited node %d is absent from topology replay", transition.nodeID)
	}
	modifiedAt, err := auditField(mutation, auditRecordedAtField)
	if err != nil {
		return err
	}
	replay.topology[index], err = replaceAuditRecordField(replay.topology[index], "modified_at", modifiedAt)
	return err
}

func validateReplayedIngest(record audit.Record) error {
	if record.Kind != metadataIngestType {
		return errors.New("provenance mutation ingest has the wrong record kind")
	}
	if _, err := auditUUIDField(record, "ingest_id"); err != nil {
		return err
	}
	if _, err := auditTimestampField(record, "started_at"); err != nil {
		return err
	}
	sourceKind, err := auditTextField(record, "source_kind")
	if err != nil {
		return err
	}
	if !sourceKindIsEmbedded(sourceKind) || len(sourceKind) == len(callerSuppliedSourceKindPrefix) {
		return errors.New("provenance mutation ingest requires a non-empty caller-supplied source kind")
	}
	description, err := auditField(record, "source_desc")
	if err != nil {
		return err
	}
	if value, ok := description.BytesValue(); !ok || len(value) == 0 || !utf8.Valid(value) {
		return errors.New("provenance mutation ingest has invalid source description")
	}
	return nil
}
