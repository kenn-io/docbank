package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/audit"
)

func persistAuditedIngestObservation(
	ctx context.Context, tx *sql.Tx, vaultID, operationID, recordedAt string,
	nodeSequence int64, authority auditAuthorityState, scopes []auditScopeState,
	priorNode, resultingNode Node, ingest metadataIngest, provenance metadataProvenance,
	binding *ProvenanceVersionBinding, ingestAdded bool,
) error {
	if sourceKindIsEmbedded(ingest.SourceKind) {
		return errors.New("audited ingest observation requires an operational source kind")
	}
	if provenance.Supersedes != nil {
		return errors.New("audited ingest observation cannot supersede provenance")
	}
	if priorNode.ID != resultingNode.ID || priorNode.CurrentVersionID != resultingNode.CurrentVersionID {
		return errors.New("audited ingest observation changes content identity")
	}
	return persistAuditedProvenanceMutation(
		ctx, tx, vaultID, operationID, recordedAt, nodeSequence, authority, scopes,
		priorNode, resultingNode, ingest, provenance, binding, "ingest_observe", ingestAdded,
	)
}

func auditedMutationFirstEventKind(mutation audit.Record) (string, error) {
	events, err := auditRecordListField(mutation, "events")
	if err != nil || len(events) == 0 {
		return "", errors.New("attached-metadata mutation must contain an event")
	}
	return auditTextField(events[0], "event_kind")
}

func (replay *auditedHistoryReplay) validateIngestObservationDelta(
	mutation audit.Record, operationID string,
	deltaRecords map[string]storedAuditRecord, usedDeltas map[string]bool,
) (replayedProvenanceMutation, error) {
	digest, err := auditDigestField(mutation, "attached_metadata_change_digest")
	if err != nil {
		return replayedProvenanceMutation{}, err
	}
	delta, ok := deltaRecords[digest]
	if !ok || usedDeltas[digest] {
		return replayedProvenanceMutation{}, errors.New("ingest observation lacks one unique attached-metadata delta")
	}
	if err := requireAuditUUID(delta.record, auditOperationIDField, operationID); err != nil {
		return replayedProvenanceMutation{}, err
	}
	changes, err := auditRecordListField(delta.record, "changes")
	if err != nil || len(changes) < 1 || len(changes) > 3 {
		return replayedProvenanceMutation{}, errors.New("ingest observation must contain one fact and optional ingest and binding")
	}
	var provenance, ingest, binding audit.Record
	for _, change := range changes {
		post, err := validateAuditedIngestAddition(change)
		if err != nil {
			return replayedProvenanceMutation{}, err
		}
		switch post.Kind {
		case metadataProvenanceType:
			if provenance.Kind != "" {
				return replayedProvenanceMutation{}, errors.New("ingest observation repeats its fact change")
			}
			provenance = post
		case metadataIngestType:
			if ingest.Kind != "" {
				return replayedProvenanceMutation{}, errors.New("ingest observation repeats its ingest change")
			}
			ingest = post
		case metadataProvenanceVersionBindingType:
			if binding.Kind != "" {
				return replayedProvenanceMutation{}, errors.New("ingest observation repeats its binding change")
			}
			binding = post
		default:
			return replayedProvenanceMutation{}, fmt.Errorf(
				"ingest observation carries unsupported attachment %q", post.Kind,
			)
		}
	}
	if provenance.Kind == "" {
		return replayedProvenanceMutation{}, errors.New("ingest observation lacks its fact change")
	}
	ingestID, err := auditUUIDField(provenance, "ingest_id")
	if err != nil {
		return replayedProvenanceMutation{}, err
	}
	ingestAdded := ingest.Kind != ""
	if ingestAdded {
		if err := requireAuditUUID(ingest, "ingest_id", ingestID); err != nil {
			return replayedProvenanceMutation{}, err
		}
		key, err := attachedAuditKey(ingest)
		if err != nil {
			return replayedProvenanceMutation{}, err
		}
		if _, exists := replay.attachments[key]; exists {
			return replayedProvenanceMutation{}, errors.New("ingest observation reuses ingest identity")
		}
	} else {
		ingest, err = replay.ingestAttachment(ingestID)
		if err != nil {
			return replayedProvenanceMutation{}, err
		}
	}
	if err := validateReplayedOperationalIngest(ingest); err != nil {
		return replayedProvenanceMutation{}, err
	}
	if err := validateReplayedProvenance(provenance, ingestID); err != nil {
		return replayedProvenanceMutation{}, err
	}
	if supersedes, err := auditOptionalDigestField(provenance, "supersedes"); err != nil {
		return replayedProvenanceMutation{}, err
	} else if supersedes != nil {
		return replayedProvenanceMutation{}, errors.New("ingest observation cannot supersede provenance")
	}
	nodeID, err := auditUnsignedField(provenance, metadataNodeIDField)
	if err != nil || !replay.memberSet[nodeID] {
		return replayedProvenanceMutation{}, fmt.Errorf("ingest observation targets unaudited node %d", nodeID)
	}
	topologyIndex, ok := replay.topologyIndex[nodeID]
	if !ok {
		return replayedProvenanceMutation{}, fmt.Errorf("ingest observation target %d is absent from topology", nodeID)
	}
	topology := replay.topology[topologyIndex]
	state, err := auditTextField(topology, auditStateField)
	if err != nil {
		return replayedProvenanceMutation{}, err
	}
	if state != auditNodeStateLive {
		return replayedProvenanceMutation{}, fmt.Errorf(
			"ingest observation targets non-live node %d", nodeID,
		)
	}
	nodeKind, err := auditTextField(topology, "node_kind")
	if err != nil {
		return replayedProvenanceMutation{}, err
	}
	if nodeKind != nodeKindFile {
		return replayedProvenanceMutation{}, fmt.Errorf("ingest observation targets non-file node %d", nodeID)
	}
	key, err := attachedAuditKey(provenance)
	if err != nil {
		return replayedProvenanceMutation{}, err
	}
	if _, exists := replay.attachments[key]; exists {
		return replayedProvenanceMutation{}, errors.New("ingest observation reuses provenance identity")
	}
	if binding.Kind != "" {
		bindingKey, err := attachedAuditKey(binding)
		if err != nil {
			return replayedProvenanceMutation{}, err
		}
		if _, exists := replay.attachments[bindingKey]; exists {
			return replayedProvenanceMutation{}, errors.New("ingest observation reuses binding identity")
		}
		provenanceIdentity, err := auditDigestField(provenance, "identity")
		if err != nil {
			return replayedProvenanceMutation{}, err
		}
		state := replay.states[nodeID]
		contentVersionID, err := auditUUIDField(state, auditCurrentVersionIDField)
		if err != nil {
			return replayedProvenanceMutation{}, err
		}
		observedAt, err := auditTimestampField(mutation, auditRecordedAtField)
		if err != nil {
			return replayedProvenanceMutation{}, err
		}
		if err := validateAuditedProvenanceVersionBinding(
			binding, provenanceIdentity, contentVersionID, observedAt,
		); err != nil {
			return replayedProvenanceMutation{}, err
		}
	}
	usedDeltas[digest] = true
	return replayedProvenanceMutation{
		nodeID: nodeID, ingest: ingest, provenance: provenance, binding: binding,
		digest: digest, ingestAdded: ingestAdded, eventKind: "ingest_observe",
	}, nil
}

func validateReplayedOperationalIngest(record audit.Record) error {
	if record.Kind != metadataIngestType {
		return errors.New("ingest observation has the wrong ingest record kind")
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
	if sourceKind == "" || sourceKindIsEmbedded(sourceKind) {
		return errors.New("ingest observation requires an operational source kind")
	}
	description, err := auditField(record, "source_desc")
	if err != nil {
		return err
	}
	if value, ok := description.BytesValue(); !ok || len(value) == 0 || !utf8.Valid(value) {
		return errors.New("ingest observation has invalid source description")
	}
	return nil
}
