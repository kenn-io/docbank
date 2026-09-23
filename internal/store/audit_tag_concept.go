package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/docbank/internal/audit"
)

const auditConceptStateKind = "concept_state_v1"

func conceptStatePresent(ctx context.Context, tx metadataQuerier) (bool, error) {
	var present bool
	err := tx.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM tag_concepts) OR EXISTS(SELECT 1 FROM tag_aliases) OR
		EXISTS(SELECT 1 FROM tag_concept_edges) OR EXISTS(SELECT 1 FROM tag_redirects) OR
		EXISTS(SELECT 1 FROM tag_merge_audit) OR EXISTS(SELECT 1 FROM passage_tags)`).Scan(&present)
	return present, err
}

// conceptStateAuditRecord binds the complete deterministic concept projection
// to the audit lineage without placing potentially large rows in audit records.
func conceptStateAuditRecord(ctx context.Context, tx metadataQuerier) (audit.Record, error) {
	h := sha256.New()
	write := func(value any) error {
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		_, err = h.Write(append(encoded, '\n'))
		return err
	}
	if err := exportTagConceptMetadata(ctx, tx, write); err != nil {
		return audit.Record{}, err
	}
	if err := exportPassageTagMetadata(ctx, tx, write); err != nil {
		return audit.Record{}, err
	}
	digest, err := audit.DigestHex(hex.EncodeToString(h.Sum(nil)))
	if err != nil {
		return audit.Record{}, err
	}
	return audit.Record{Kind: auditConceptStateKind, Fields: []audit.Field{
		{Name: "digest", Value: digest},
	}}, nil
}

func appendAuditConceptState(ctx context.Context, tx metadataQuerier, records *[]audit.Record) error {
	present, err := conceptStatePresent(ctx, tx)
	if err != nil || !present {
		return err
	}
	record, err := conceptStateAuditRecord(ctx, tx)
	if err != nil {
		return err
	}
	*records = append(*records, record)
	return nil
}

func (s *Store) withConceptTx(ctx context.Context, fn func(*sql.Tx) error) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		active, err := auditAuthorityActiveTx(ctx, tx)
		if err != nil {
			return err
		}
		if !active {
			return fn(tx)
		}
		priorPresent, err := conceptStatePresent(ctx, tx)
		if err != nil {
			return err
		}
		prior, err := conceptStateAuditRecord(ctx, tx)
		if err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			return err
		}
		resultingPresent, err := conceptStatePresent(ctx, tx)
		if err != nil {
			return err
		}
		resulting, err := conceptStateAuditRecord(ctx, tx)
		if err != nil {
			return err
		}
		if priorPresent == resultingPresent && auditRecordEqual(prior, resulting) {
			return nil
		}
		if !priorPresent {
			prior = audit.Record{}
		}
		if !resultingPresent {
			resulting = audit.Record{}
		}
		return s.persistAuditedConceptChange(ctx, tx, prior, resulting)
	})
}

func (s *Store) persistAuditedConceptChange(ctx context.Context, tx *sql.Tx, prior, resulting audit.Record) error {
	authority, nodeSequence, err := loadAuditAuthorityTx(ctx, tx)
	if err != nil {
		return err
	}
	operationID, err := newUUIDv4()
	if err != nil {
		return err
	}
	values, err := makeAuditedMutationValues(s.vaultID, authority.lineageID, operationID, nowRFC3339())
	if err != nil {
		return err
	}
	change, err := makeAttachedMetadataChange(prior, resulting)
	if err != nil {
		return err
	}
	delta, deltaDigest, err := makeAttachedMetadataDelta(values.operationID, []audit.Record{change})
	if err != nil {
		return err
	}
	sequence, err := nextAuditInteger("operation sequence", authority.sequence)
	if err != nil {
		return err
	}
	allocation, err := makeAuditAllocationEntry(values, sequence, nodeSequence, authority.allocationHead, audit.Absent())
	if err != nil {
		return err
	}
	allocation, err = addAttachedMetadataToAllocation(allocation, 1, deltaDigest.value)
	if err != nil {
		return err
	}
	if err := insertAuditRecord(ctx, tx, delta); err != nil {
		return fmt.Errorf("recording audited concept change: %w", err)
	}
	return advanceAuditAuthority(ctx, tx, authority, sequence, allocation)
}

func (replay *auditedHistoryReplay) applyUnscopedConceptStateChange(
	operationID, digest string, allocation storedAuditRecord, nextCount int64,
	deltaRecords map[string]storedAuditRecord, usedDeltas map[string]bool,
) (bool, error) {
	delta, ok := deltaRecords[digest]
	if !ok || usedDeltas[digest] {
		return false, nil
	}
	changes, err := auditRecordListField(delta.record, "changes")
	if err != nil || len(changes) != 1 {
		return false, err
	}
	kind, err := auditTextField(changes[0], "record_kind")
	if err != nil || kind != auditConceptStateKind {
		return false, err
	}
	if err := requireAuditUUID(delta.record, auditOperationIDField, operationID); err != nil {
		return true, err
	}
	if err := requireAuditUnsigned(allocation.record, auditAttachedMetadataChangeCountField, 1); err != nil {
		return true, err
	}
	change := changes[0]
	pre, hasPre, err := optionalNestedAuditRecord(change, auditPreField)
	if err != nil {
		return true, err
	}
	post, hasPost, err := optionalNestedAuditRecord(change, auditPostField)
	if err != nil {
		return true, err
	}
	if !hasPre && !hasPost || hasPre && pre.Kind != auditConceptStateKind || hasPost && post.Kind != auditConceptStateKind {
		return true, errors.New("concept change has invalid record kind")
	}
	if hasPre {
		if _, err := auditDigestField(pre, "digest"); err != nil {
			return true, err
		}
	}
	if hasPost {
		if _, err := auditDigestField(post, "digest"); err != nil {
			return true, err
		}
	}
	record := pre
	if !hasPre {
		record = post
	}
	identity, _ := attachedAuditIdentity(record)
	storedIdentity, err := auditNestedField(change, "stable_identity")
	if err != nil || !auditRecordEqual(storedIdentity, identity) {
		return true, errors.New("concept change identity is invalid")
	}
	key, _ := attachedAuditKey(record)
	current, exists := replay.attachments[key]
	if hasPre && (!exists || !auditRecordEqual(current, pre)) || !hasPre && exists || hasPre && hasPost && auditRecordEqual(pre, post) {
		return true, errors.New("concept change does not extend prior authority")
	}
	if hasPost {
		replay.attachments[key] = post
	} else {
		delete(replay.attachments, key)
	}
	usedDeltas[digest] = true
	replay.allocationCount, replay.allocationHead = nextCount, allocation.digest
	return true, nil
}
