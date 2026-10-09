package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/docbank/internal/audit"
)

const (
	auditPriorCurrentVersionIDField     = "prior_current_version_id"
	auditResultingCurrentVersionIDField = "resulting_current_version_id"
)

func photoAuthoredAuditRecord(s PhotoAuthoredSnapshot) (audit.Record, error) {
	if err := ValidatePhotoAuthored(s.Values); err != nil {
		return audit.Record{}, err
	}
	file, err := audit.UUID(s.FileID)
	if err != nil {
		return audit.Record{}, err
	}
	node, err := positiveAuditNodeID(s.NodeID)
	if err != nil {
		return audit.Record{}, err
	}
	revision, err := positiveAuditRevision(s.Revision)
	if err != nil {
		return audit.Record{}, err
	}
	v := s.Values
	rating, rotation := v.Rating, v.Rotation
	fields := []audit.Field{{Name: "file_id", Value: file}, {Name: "node_id", Value: audit.Unsigned(node)}, {Name: "revision", Value: audit.Unsigned(revision)}, {Name: "rating", Value: audit.Unsigned(uint64(rating))}} //nolint:gosec // ValidatePhotoAuthored bounds rating to 0 through 5.
	for _, f := range []struct{ name, value string }{{"flag", v.Flag}, {"label", v.Label}, {"caption", v.Caption}, {"creator", v.Creator}, {"copyright", v.Copyright}} {
		text, err := audit.Text(f.value)
		if err != nil {
			return audit.Record{}, err
		}
		fields = append(fields, audit.Field{Name: f.name, Value: text})
	}
	fields = append(fields, audit.Field{Name: "rotation", Value: audit.Unsigned(uint64(rotation))}) //nolint:gosec // ValidatePhotoAuthored bounds rotation to 0 through 270.
	return audit.Record{Kind: "photo_authored", Fields: fields}, nil
}

func photoAuthoredFromAudit(r audit.Record) (PhotoAuthoredSnapshot, error) {
	var s PhotoAuthoredSnapshot
	var err error
	if r.Kind != "photo_authored" {
		return s, ErrInvalidPhotoAsset
	}
	if s.FileID, err = auditUUIDField(r, "file_id"); err != nil {
		return s, err
	}
	if s.NodeID, err = auditInt64UnsignedField(r, "node_id"); err != nil {
		return s, err
	}
	if s.Revision, err = auditInt64UnsignedField(r, "revision"); err != nil {
		return s, err
	}
	rating, err := auditUnsignedField(r, "rating")
	if err != nil || rating > 5 {
		return s, ErrInvalidPhotoAsset
	}
	s.Values.Rating = int(rating)
	rotation, err := auditUnsignedField(r, "rotation")
	if err != nil || rotation > 270 {
		return s, ErrInvalidPhotoAsset
	}
	s.Values.Rotation = int(rotation)
	for _, f := range []struct {
		name  string
		value *string
	}{{"flag", &s.Values.Flag}, {"label", &s.Values.Label}, {"caption", &s.Values.Caption}, {"creator", &s.Values.Creator}, {"copyright", &s.Values.Copyright}} {
		*f.value, err = auditTextField(r, f.name)
		if err != nil {
			return s, err
		}
	}
	if s.NodeID < 1 || s.Revision < 1 {
		return s, ErrInvalidPhotoAsset
	}
	return s, ValidatePhotoAuthored(s.Values)
}

func appendAuditPhotoAuthored(ctx context.Context, q metadataQuerier, records *[]audit.Record) error {
	rows, err := q.QueryContext(ctx, `SELECT file_id,node_id,revision,rating,flag,label,caption,creator,copyright,rotation FROM photo_files WHERE role<>'sidecar' ORDER BY file_id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var s PhotoAuthoredSnapshot
		v := &s.Values
		if err := rows.Scan(&s.FileID, &s.NodeID, &s.Revision, &v.Rating, &v.Flag, &v.Label, &v.Caption, &v.Creator, &v.Copyright, &v.Rotation); err != nil {
			return err
		}
		r, err := photoAuthoredAuditRecord(s)
		if err != nil {
			return err
		}
		*records = append(*records, r)
	}
	return rows.Err()
}

func (s *Store) persistPhotoAuthoredAuditTx(ctx context.Context, tx *sql.Tx, prior Node, before, after PhotoAuthoredSnapshot, now string) error {
	authority, scopes, sequence, err := loadAuditedNodeAuthority(ctx, tx, prior.ID)
	if err != nil {
		return err
	}
	operationID, err := newUUIDv4()
	if err != nil {
		return err
	}
	pre, err := photoAuthoredAuditRecord(before)
	if err != nil {
		return err
	}
	post, err := photoAuthoredAuditRecord(after)
	if err != nil {
		return err
	}
	result := prior
	result.Revision++
	result.ModifiedAt = now
	return persistAuditedAttachmentChange(ctx, tx, s.vaultID, operationID, now, sequence, authority, scopes, prior, result, pre, post, "photo_authored")
}

func (replay *auditedHistoryReplay) applyPhotoAuthored(vaultID string, mutation, allocation storedAuditRecord, scopeIDs []string, scopeEntries map[string]storedAuditRecord, deltas, events map[string]storedAuditRecord, usedDeltas, usedEvents map[string]bool) error {
	operationID, err := auditUUIDField(mutation.record, auditOperationIDField)
	if err != nil {
		return err
	}
	if err := requireAuditUUID(mutation.record, auditVaultIDField, vaultID); err != nil {
		return err
	}
	sequence, err := positiveAuditInteger("operation sequence", replay.allocationCount+1)
	if err != nil {
		return err
	}
	if err := requireAuditUnsigned(mutation.record, "operation_sequence", sequence); err != nil {
		return err
	}
	if err := requireAuditAbsentFields(mutation.record, "grouping_id", auditTopologyDeltaField, "path_effect_digest", "witness_change_digest"); err != nil {
		return err
	}
	for _, f := range []string{auditPathEffectCountField, auditWitnessChangeCountField} {
		if err := requireAuditUnsigned(mutation.record, f, 0); err != nil {
			return err
		}
	}
	bindings, err := auditRecordListField(mutation.record, "baselines")
	if err != nil || len(bindings) != 0 {
		return errors.New("photo edit cannot enroll an audit scope")
	}
	digest, err := auditDigestField(mutation.record, "attached_metadata_change_digest")
	if err != nil {
		return err
	}
	delta, ok := deltas[digest]
	if !ok || usedDeltas[digest] {
		return errors.New("photo edit lacks a unique delta")
	}
	if err := requireAuditUUID(delta.record, auditOperationIDField, operationID); err != nil {
		return err
	}
	changes, err := auditRecordListField(delta.record, "changes")
	if err != nil || len(changes) != 1 {
		return errors.New("photo edit must have one attachment change")
	}
	change := changes[0]
	if err := requireAuditText(change, "record_kind", "photo_authored"); err != nil {
		return err
	}
	pre, err := auditNestedField(change, auditPreField)
	if err != nil {
		return err
	}
	post, err := auditNestedField(change, auditPostField)
	if err != nil {
		return err
	}
	b, err := photoAuthoredFromAudit(pre)
	if err != nil {
		return err
	}
	a, err := photoAuthoredFromAudit(post)
	if err != nil {
		return err
	}
	if b.FileID != a.FileID || b.NodeID != a.NodeID || a.Revision != b.Revision+1 {
		return errors.New("photo audit alters identity or skips a revision")
	}
	key, err := attachedAuditKey(pre)
	if err != nil {
		return err
	}
	current, exists := replay.attachments[key]
	if !exists && b.Revision == 1 && b.Values == (PhotoAuthored{}) {
		// Schema 28 enrolled files before authored decisions existed.
		current = pre
	}
	if !auditRecordEqual(current, pre) {
		return errors.New("photo audit prior values differ from replay")
	}
	identity, err := attachedAuditIdentity(pre)
	if err != nil {
		return err
	}
	stored, err := auditNestedField(change, "stable_identity")
	if err != nil || !auditRecordEqual(identity, stored) {
		return errors.New("photo audit identity differs from delta")
	}
	nodeID, err := positiveAuditNodeID(b.NodeID)
	if err != nil {
		return err
	}
	expectedScopes := []replayedAuditedTagCandidate{}
	for id, scope := range replay.scopes {
		if scope.memberSet[nodeID] {
			expectedScopes = append(expectedScopes, replayedAuditedTagCandidate{nodeID: nodeID, scopeID: id})
		}
	}
	if err := requireAuditedTagScopeFanout("photo edit", scopeIDs, expectedScopes); err != nil {
		return err
	}
	list, err := auditRecordListField(mutation.record, "events")
	if err != nil || len(list) != len(scopeIDs) {
		return errors.New("photo audit event count differs from scope count")
	}
	ordinal := uint64(0)
	for i, event := range list {
		if err := validateAuditEventWrapper(operationID, ordinal, event, events, usedEvents); err != nil {
			return err
		}
		scopeID, err := auditUUIDField(event, auditScopeIDField)
		if err != nil {
			return err
		}
		if scopeID != scopeIDs[i] {
			return errors.New("photo audit scope ordering differs")
		}
		if err := requireMatchingEventEnvelope(mutation.record, event); err != nil {
			return err
		}
		if err := requireAuditUnsigned(event, metadataNodeIDField, nodeID); err != nil {
			return err
		}
		if err := requireAuditText(event, "event_kind", "photo_authored"); err != nil {
			return err
		}
		if err := requireAuditText(event, "attachment_kind", "photo_authored"); err != nil {
			return err
		}
		eid, err := auditNestedField(event, "attachment_identity")
		if err != nil || !auditRecordEqual(eid, identity) {
			return errors.New("photo event identity differs")
		}
		for _, field := range []string{auditPreField, auditPostField} {
			want, err := auditNestedField(change, field)
			if err != nil {
				return err
			}
			got, err := auditNestedField(event, field)
			if err != nil || !auditRecordEqual(want, got) {
				return errors.New("photo event values differ from delta")
			}
		}
		ordinal++
		state := replay.states[nodeID]
		rev, err := auditUnsignedField(state, "node_revision")
		if err != nil {
			return err
		}
		current, err := auditOptionalUUIDField(state, "current_version_id")
		if err != nil {
			return err
		}
		if err := requireAuditUnsigned(event, "prior_node_revision", rev); err != nil {
			return err
		}
		if err := requireAuditUnsigned(event, "resulting_node_revision", rev+1); err != nil {
			return err
		}
		for _, f := range []string{auditPriorCurrentVersionIDField, auditResultingCurrentVersionIDField} {
			if err := requireAuditOptionalUUID(event, f, current); err != nil {
				return err
			}
		}
		if err := requireAuditAbsentFields(event, "target_node_id", "source_version_id", auditTopologyDeltaField, "baseline_digest"); err != nil {
			return err
		}
	}
	if err := replay.validateMemberStateChanges(mutation.record, []uint64{nodeID}); err != nil {
		return err
	}
	if err := requireAuditUnsigned(mutation.record, auditAttachedMetadataChangeCountField, 1); err != nil {
		return err
	}
	if err := replay.advanceScopes(vaultID, mutation, scopeIDs, scopeEntries); err != nil {
		return err
	}
	if err := replay.advanceAllocation(vaultID, operationID, mutation, allocation, digest, 1); err != nil {
		return err
	}
	usedDeltas[digest] = true
	if err := replay.applyTagAssignmentState(replayedTagAssignment{nodeID: nodeID, assignment: post, assign: true}, mutation.record); err != nil {
		return fmt.Errorf("applying photo audit: %w", err)
	}
	return nil
}
