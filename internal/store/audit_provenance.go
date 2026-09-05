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
	// The ingest row and its provenance row are one logical assertion. Both
	// become auditable attachments in the same mutation delta.
	ingestChange, err := makeAttachedMetadataAddition(ingestRecord)
	if err != nil {
		return err
	}
	delta, deltaDigest, err := makeAttachedMetadataDelta(
		values.operationID, []audit.Record{ingestChange, provenanceChange},
	)
	if err != nil {
		return err
	}
	events := make([]audit.Record, len(scopes))
	eventKind := "provenance_add"
	if provenance.Supersedes != nil {
		eventKind = "provenance_supersede"
	}
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
		mutation, auditAttachedMetadataChangeCountField, audit.Unsigned(2),
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
	allocation, err = addAttachedMetadataToAllocation(allocation, 2, deltaDigest.value)
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

type replayedProvenanceAppend struct {
	nodeID     uint64
	ingest     audit.Record
	provenance audit.Record
	change     audit.Record
	digest     string
}

func (replay *auditedHistoryReplay) applyProvenanceAppend(
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
	transition, err := replay.validateProvenanceAppendDelta(
		mutation.record, operationID, deltaRecords, usedDeltas,
	)
	if err != nil {
		return err
	}
	if err := replay.validateProvenanceAppendEvent(
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
	if err := requireAuditUnsigned(mutation.record, auditAttachedMetadataChangeCountField, 2); err != nil {
		return err
	}
	if err := requireAuditDigest(mutation.record, "attached_metadata_change_digest", transition.digest); err != nil {
		return err
	}
	if err := replay.advanceScope(vaultID, mutation, scopeEntry); err != nil {
		return err
	}
	if err := replay.advanceAllocation(vaultID, operationID, mutation, allocation, transition.digest, 2); err != nil {
		return err
	}
	return replay.applyProvenanceAppendState(transition, mutation.record)
}

func (replay *auditedHistoryReplay) validateProvenanceAppendDelta(
	mutation audit.Record, operationID string,
	deltaRecords map[string]storedAuditRecord, usedDeltas map[string]bool,
) (replayedProvenanceAppend, error) {
	digest, err := auditDigestField(mutation, "attached_metadata_change_digest")
	if err != nil {
		return replayedProvenanceAppend{}, err
	}
	delta, ok := deltaRecords[digest]
	if !ok || usedDeltas[digest] {
		return replayedProvenanceAppend{}, errors.New("provenance mutation lacks one unique attached-metadata delta")
	}
	if err := requireAuditUUID(delta.record, auditOperationIDField, operationID); err != nil {
		return replayedProvenanceAppend{}, err
	}
	changes, err := auditRecordListField(delta.record, "changes")
	if err != nil || len(changes) != 2 {
		return replayedProvenanceAppend{}, errors.New("provenance mutation must contain ingest and provenance changes")
	}
	var change, ingestChange audit.Record
	for _, candidate := range changes {
		kind, kindErr := auditTextField(candidate, "record_kind")
		if kindErr != nil {
			return replayedProvenanceAppend{}, kindErr
		}
		_, hasPre, preErr := optionalNestedAuditRecord(candidate, auditPreField)
		if preErr != nil || hasPre {
			return replayedProvenanceAppend{}, errors.New("provenance mutation changes an existing attachment")
		}
		_, hasPost, postErr := optionalNestedAuditRecord(candidate, auditPostField)
		if postErr != nil || !hasPost {
			return replayedProvenanceAppend{}, errors.New("provenance mutation lacks an added attachment")
		}
		switch kind {
		case metadataProvenanceType:
			if change.Kind != "" {
				return replayedProvenanceAppend{}, errors.New("provenance mutation repeats its fact change")
			}
			change = candidate
		case metadataIngestType:
			if ingestChange.Kind != "" {
				return replayedProvenanceAppend{}, errors.New("provenance mutation repeats its ingest change")
			}
			ingestChange = candidate
		default:
			return replayedProvenanceAppend{}, fmt.Errorf("provenance mutation carries unsupported attachment %q", kind)
		}
	}
	if change.Kind == "" || ingestChange.Kind == "" {
		return replayedProvenanceAppend{}, errors.New("provenance mutation must add one ingest and one fact")
	}
	ingestPost, err := validateAuditedIngestAddition(ingestChange)
	if err != nil {
		return replayedProvenanceAppend{}, err
	}
	ingestKey, err := attachedAuditKey(ingestPost)
	if err != nil {
		return replayedProvenanceAppend{}, err
	}
	if _, exists := replay.attachments[ingestKey]; exists {
		return replayedProvenanceAppend{}, errors.New("provenance mutation reuses ingest identity")
	}
	post, _, err := optionalNestedAuditRecord(change, auditPostField)
	if err != nil {
		return replayedProvenanceAppend{}, err
	}
	ingestID, err := auditUUIDField(ingestPost, "ingest_id")
	if err != nil {
		return replayedProvenanceAppend{}, err
	}
	if err := validateReplayedIngest(ingestPost); err != nil {
		return replayedProvenanceAppend{}, err
	}
	if err := validateReplayedProvenance(post, replay, ingestID); err != nil {
		return replayedProvenanceAppend{}, err
	}
	provenanceIngestID, err := auditUUIDField(post, "ingest_id")
	if err != nil || provenanceIngestID != ingestID {
		return replayedProvenanceAppend{}, errors.New("provenance fact does not bind its new ingest")
	}
	identity, err := attachedAuditIdentity(post)
	if err != nil {
		return replayedProvenanceAppend{}, err
	}
	storedIdentity, err := auditNestedField(change, "stable_identity")
	if err != nil || !auditRecordEqual(storedIdentity, identity) {
		return replayedProvenanceAppend{}, errors.New("provenance delta identity does not match its record")
	}
	nodeID, err := auditUnsignedField(post, metadataNodeIDField)
	if err != nil || !replay.memberSet[nodeID] {
		return replayedProvenanceAppend{}, fmt.Errorf("provenance mutation targets unaudited node %d", nodeID)
	}
	provenanceID, err := auditDigestField(post, "identity")
	if err != nil {
		return replayedProvenanceAppend{}, err
	}
	key, err := attachedAuditKey(post)
	if err != nil {
		return replayedProvenanceAppend{}, err
	}
	if _, exists := replay.attachments[key]; exists {
		return replayedProvenanceAppend{}, fmt.Errorf("provenance mutation reuses identity %s", provenanceID)
	}
	supersedes, err := auditOptionalDigestField(post, "supersedes")
	if err != nil {
		return replayedProvenanceAppend{}, err
	}
	if supersedes != nil {
		if err := replay.requireActiveProvenance(nodeID, *supersedes); err != nil {
			return replayedProvenanceAppend{}, err
		}
	}
	usedDeltas[digest] = true
	return replayedProvenanceAppend{
		nodeID: nodeID, provenance: post, change: change, digest: digest,
		ingest: ingestPost,
	}, nil
}

func validateReplayedProvenance(record audit.Record, replay *auditedHistoryReplay, additionalIngest string) error {
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
	if !replay.hasIngest(ingestID) && ingestID != additionalIngest {
		return fmt.Errorf("provenance attachment references missing ingest %s", ingestID)
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

func (replay *auditedHistoryReplay) hasIngest(id string) bool {
	for _, record := range replay.attachments {
		if record.Kind != metadataIngestType {
			continue
		}
		candidate, err := auditUUIDField(record, "ingest_id")
		if err == nil && candidate == id {
			return true
		}
	}
	return false
}

func (replay *auditedHistoryReplay) requireActiveProvenance(nodeID uint64, identity string) error {
	_, err := replay.activeProvenanceRecord(nodeID, identity)
	return err
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

func (replay *auditedHistoryReplay) validateProvenanceAppendEvent(
	operationID string, mutation audit.Record, transition replayedProvenanceAppend,
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
	eventKind := "provenance_add"
	if supersedes, err := auditOptionalDigestField(transition.provenance, "supersedes"); err != nil {
		return err
	} else if supersedes != nil {
		eventKind = "provenance_supersede"
	}
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
				return errors.New("provenance-add event unexpectedly has a pre-state")
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

func (replay *auditedHistoryReplay) applyProvenanceAppendState(
	transition replayedProvenanceAppend, mutation audit.Record,
) error {
	key, err := attachedAuditKey(transition.provenance)
	if err != nil {
		return err
	}
	replay.attachments[key] = transition.provenance
	ingestKey, err := attachedAuditKey(transition.ingest)
	if err != nil {
		return err
	}
	replay.attachments[ingestKey] = transition.ingest
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
	if _, err := auditTextField(record, "source_kind"); err != nil {
		return err
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
