package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"go.kenn.io/docbank/internal/audit"
)

type auditAuthorityState struct {
	lineageID       string
	sequence        int64
	allocationCount int64
	allocationHead  string
}

type auditScopeState struct {
	scopeID    string
	entryCount int64
	chainHead  string
}

func auditAuthorityActiveTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	var active bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM audit_authority WHERE singleton=1)`,
	).Scan(&active); err != nil {
		return false, fmt.Errorf("checking audit authority: %w", err)
	}
	return active, nil
}

func replaceAuditedContentTx(
	ctx context.Context, tx *sql.Tx, store *Store, node Node,
	blobHash string, size int64, mimeType string,
) (Node, ContentVersion, error) {
	authority, scopes, nodeSequence, err := loadAuditedNodeAuthority(ctx, tx, node.ID)
	if err != nil {
		return Node{}, ContentVersion{}, err
	}
	operation, err := newContentVersionOperation()
	if err != nil {
		return Node{}, ContentVersion{}, err
	}
	if err := store.EnsureBlobTx(tx, blobHash, size); err != nil {
		return Node{}, ContentVersion{}, err
	}
	prior, err := scanContentVersion(tx.QueryRowContext(ctx,
		`SELECT `+contentVersionCols+` FROM content_versions WHERE version_id=?`,
		node.CurrentVersionID,
	))
	if err != nil {
		return Node{}, ContentVersion{}, fmt.Errorf("reading audited prior content: %w", err)
	}
	updated, version, err := installContentVersionWithOperationTx(
		tx, node, blobHash, size, mimeType, "content_replace", nil, operation,
	)
	if err != nil {
		return Node{}, ContentVersion{}, err
	}
	if err := persistAuditedContentReplacement(
		ctx, tx, store.vaultID, nodeSequence, authority, scopes,
		node, updated, prior, version,
	); err != nil {
		return Node{}, ContentVersion{}, err
	}
	return updated, version, nil
}

func loadAuditedNodeAuthority(
	ctx context.Context, tx *sql.Tx, nodeID int64,
) (auditAuthorityState, []auditScopeState, int64, error) {
	var authority auditAuthorityState
	err := tx.QueryRowContext(ctx, `SELECT lineage_id,operation_sequence_high_water,
		allocation_entry_count,allocation_head FROM audit_authority WHERE singleton=1`).Scan(
		&authority.lineageID, &authority.sequence,
		&authority.allocationCount, &authority.allocationHead,
	)
	if err != nil {
		return authority, nil, 0, fmt.Errorf("reading audit authority: %w", err)
	}
	if authority.sequence != authority.allocationCount {
		return authority, nil, 0, errors.New("audit allocation count disagrees with operation sequence")
	}
	rows, err := tx.QueryContext(ctx, `SELECT scope.scope_id,scope.entry_count,scope.chain_head
		FROM audit_scopes scope JOIN audit_memberships membership USING(scope_id)
		WHERE membership.node_id=? ORDER BY scope.scope_id`, nodeID)
	if err != nil {
		return authority, nil, 0, fmt.Errorf("reading audit memberships for node %d: %w", nodeID, err)
	}
	defer func() { _ = rows.Close() }()
	var scopes []auditScopeState
	for rows.Next() {
		var scope auditScopeState
		if err := rows.Scan(&scope.scopeID, &scope.entryCount, &scope.chainHead); err != nil {
			return authority, nil, 0, fmt.Errorf("scanning audit membership for node %d: %w", nodeID, err)
		}
		scopes = append(scopes, scope)
	}
	if err := rows.Err(); err != nil {
		return authority, nil, 0, fmt.Errorf("reading audit memberships for node %d: %w", nodeID, err)
	}
	if len(scopes) == 0 {
		return authority, nil, 0, unsupportedAuditedNodeMutation(nodeID)
	}
	var nodeSequence int64
	if err := tx.QueryRowContext(ctx,
		`SELECT seq FROM sqlite_sequence WHERE name='nodes'`,
	).Scan(&nodeSequence); err != nil {
		return authority, nil, 0, fmt.Errorf("reading node ID high-water mark: %w", err)
	}
	return authority, scopes, nodeSequence, nil
}

func unsupportedAuditedNodeMutation(nodeID int64) error {
	return fmt.Errorf("node %d is not in an audit scope: %w", nodeID, ErrAuditMutationUnsupported)
}

func nextAuditInteger(name string, value int64) (int64, error) {
	if value < 1 || value == math.MaxInt64 {
		return 0, fmt.Errorf("audit %s cannot advance beyond %d", name, value)
	}
	return value + 1, nil
}

func persistAuditedContentReplacement(
	ctx context.Context, tx *sql.Tx, vaultID string, nodeSequence int64,
	authority auditAuthorityState, scopes []auditScopeState,
	priorNode, resultingNode Node, priorVersion, resultingVersion ContentVersion,
) error {
	operationSequence, err := nextAuditInteger("operation sequence", authority.sequence)
	if err != nil {
		return err
	}
	values, err := makeAuditedMutationValues(
		vaultID, authority.lineageID, resultingVersion.IntroducedOperationID,
		resultingVersion.RecordedAt,
	)
	if err != nil {
		return err
	}
	priorRecord, err := auditRecordForContentVersion(priorVersion)
	if err != nil {
		return err
	}
	resultingRecord, err := auditRecordForContentVersion(resultingVersion)
	if err != nil {
		return err
	}
	events := make([]audit.Record, len(scopes))
	for index, scope := range scopes {
		events[index], err = makeAuditedContentReplacementEvent(
			values, scope.scopeID, uint64(index), priorNode, resultingNode,
			priorRecord, resultingRecord,
		)
		if err != nil {
			return err
		}
	}
	change, err := makeAuditMemberStateChange(priorNode, resultingNode)
	if err != nil {
		return err
	}
	mutation, err := makeAuditedContentReplacementMutation(
		values, operationSequence, events, change,
	)
	if err != nil {
		return err
	}
	mutationDigest, err := hashAuditRecord(mutation)
	if err != nil {
		return err
	}
	for _, event := range events {
		if err := insertAuditRecord(ctx, tx, audit.Record{Kind: "event", Fields: []audit.Field{
			{Name: "event", Value: audit.Nested(event)},
		}}); err != nil {
			return err
		}
	}
	if err := insertAuditRecord(ctx, tx, mutation); err != nil {
		return err
	}
	for _, scope := range scopes {
		entryCount, err := nextAuditInteger("scope entry count", scope.entryCount)
		if err != nil {
			return err
		}
		entry, err := makeAuditScopeChainEntry(
			values, scope.scopeID, entryCount, scope.chainHead, mutationDigest.value,
		)
		if err != nil {
			return err
		}
		head, err := hashAuditRecord(entry)
		if err != nil {
			return err
		}
		if err := insertAuditRecord(ctx, tx, entry); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE audit_scopes
			SET entry_count=?,chain_head=? WHERE scope_id=?`,
			entryCount, head.text, scope.scopeID); err != nil {
			return fmt.Errorf("advancing audit scope %s: %w", scope.scopeID, err)
		}
	}
	allocation, err := makeAuditedContentAllocationEntry(
		values, operationSequence, nodeSequence, authority.allocationHead,
		mutationDigest.value,
	)
	if err != nil {
		return err
	}
	allocationHead, err := hashAuditRecord(allocation)
	if err != nil {
		return err
	}
	if err := insertAuditRecord(ctx, tx, allocation); err != nil {
		return err
	}
	allocationCount, err := nextAuditInteger("allocation entry count", authority.allocationCount)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE audit_authority
		SET operation_sequence_high_water=?,allocation_entry_count=?,allocation_head=?
		WHERE singleton=1`, operationSequence, allocationCount, allocationHead.text); err != nil {
		return fmt.Errorf("advancing audit allocation authority: %w", err)
	}
	return nil
}

type auditedMutationValues struct {
	vaultID, lineageID, operationID audit.Value
	recordedAt, origin              audit.Value
}

func makeAuditedMutationValues(
	vaultID, lineageID, operationID, recordedAt string,
) (auditedMutationValues, error) {
	var values auditedMutationValues
	constructors := []struct {
		name  string
		value string
		make  func(string) (audit.Value, error)
		out   *audit.Value
	}{
		{"vault ID", vaultID, audit.UUID, &values.vaultID},
		{"lineage ID", lineageID, audit.UUID, &values.lineageID},
		{"operation ID", operationID, audit.UUID, &values.operationID},
		{"recorded time", recordedAt, audit.Timestamp, &values.recordedAt},
		{auditOriginField, "api", audit.Text, &values.origin},
	}
	for _, item := range constructors {
		value, err := item.make(item.value)
		if err != nil {
			return values, fmt.Errorf("encoding audited content replacement %s: %w", item.name, err)
		}
		*item.out = value
	}
	return values, nil
}

func auditRecordForContentVersion(version ContentVersion) (audit.Record, error) {
	mediaType := sql.NullString{String: version.MimeType, Valid: version.MimeType != ""}
	source := sql.NullString{}
	if version.SourceVersionID != nil {
		source = sql.NullString{String: *version.SourceVersionID, Valid: true}
	}
	if version.NodeID <= 0 || version.Size < 0 || version.NodeRevision <= 0 {
		return audit.Record{}, fmt.Errorf("content version %s has invalid audit fields", version.ID)
	}
	return contentVersionAuditRecord(
		version.ID, version.BlobHash, version.RecordedAt,
		version.IntroducedOperationID, version.TransitionKind,
		uint64(version.NodeID), uint64(version.Size), uint64(version.NodeRevision),
		mediaType, source,
	)
}

func makeAuditedContentReplacementEvent(
	values auditedMutationValues, scopeID string, ordinal uint64,
	priorNode, resultingNode Node, priorVersion, resultingVersion audit.Record,
) (audit.Record, error) {
	nodeID, err := positiveAuditNodeID(priorNode.ID)
	if err != nil || resultingNode.ID != priorNode.ID {
		return audit.Record{}, errors.New("audited content replacement changes node identity")
	}
	priorRevision, err := positiveAuditRevision(priorNode.Revision)
	if err != nil {
		return audit.Record{}, err
	}
	resultingRevision, err := positiveAuditRevision(resultingNode.Revision)
	if err != nil || resultingRevision != priorRevision+1 {
		return audit.Record{}, errors.New("audited content replacement has an invalid revision transition")
	}
	scopeValue, err := audit.UUID(scopeID)
	if err != nil {
		return audit.Record{}, err
	}
	eventKind, err := audit.Text("content_replace")
	if err != nil {
		return audit.Record{}, err
	}
	identity, err := hashAuditRecord(audit.Record{Kind: "event_identity", Fields: []audit.Field{
		{Name: auditOperationIDField, Value: values.operationID},
		{Name: "event_ordinal", Value: audit.Unsigned(ordinal)},
	}})
	if err != nil {
		return audit.Record{}, err
	}
	priorVersionID, err := audit.UUID(priorNode.CurrentVersionID)
	if err != nil {
		return audit.Record{}, err
	}
	resultingVersionID, err := audit.UUID(resultingNode.CurrentVersionID)
	if err != nil {
		return audit.Record{}, err
	}
	return audit.Record{Kind: "audit_event", Fields: []audit.Field{
		{Name: "event_id", Value: identity.value},
		{Name: auditOperationIDField, Value: values.operationID},
		{Name: metadataNodeIDField, Value: audit.Unsigned(nodeID)},
		{Name: "event_kind", Value: eventKind},
		{Name: auditScopeIDField, Value: scopeValue},
		{Name: "target_node_id", Value: audit.Absent()},
		{Name: "attachment_kind", Value: audit.Absent()},
		{Name: "attachment_identity", Value: audit.Absent()},
		{Name: "source_version_id", Value: audit.Absent()},
		{Name: "event_ordinal", Value: audit.Unsigned(ordinal)},
		{Name: "recorded_at", Value: values.recordedAt},
		{Name: "prior_node_revision", Value: audit.Unsigned(priorRevision)},
		{Name: "resulting_node_revision", Value: audit.Unsigned(resultingRevision)},
		{Name: "prior_current_version_id", Value: priorVersionID},
		{Name: "resulting_current_version_id", Value: resultingVersionID},
		{Name: auditOriginField, Value: values.origin},
		{Name: "agent_label", Value: audit.Absent()},
		{Name: "pre", Value: audit.Nested(priorVersion)},
		{Name: "post", Value: audit.Nested(resultingVersion)},
		{Name: auditTopologyDeltaField, Value: audit.Absent()},
		{Name: "baseline_digest", Value: audit.Absent()},
	}}, nil
}

func makeAuditMemberStateChange(prior, resulting Node) (audit.Record, error) {
	nodeID, err := positiveAuditNodeID(prior.ID)
	if err != nil || resulting.ID != prior.ID {
		return audit.Record{}, errors.New("audited member-state change changes node identity")
	}
	priorRevision, err := positiveAuditRevision(prior.Revision)
	if err != nil {
		return audit.Record{}, err
	}
	resultingRevision, err := positiveAuditRevision(resulting.Revision)
	if err != nil || resultingRevision != priorRevision+1 {
		return audit.Record{}, errors.New("audited member-state change has an invalid revision transition")
	}
	priorVersion, err := auditNodeCurrentVersion(prior)
	if err != nil {
		return audit.Record{}, err
	}
	resultingVersion, err := auditNodeCurrentVersion(resulting)
	if err != nil {
		return audit.Record{}, err
	}
	return audit.Record{Kind: "member_state_change", Fields: []audit.Field{
		{Name: metadataNodeIDField, Value: audit.Unsigned(nodeID)},
		{Name: "prior_revision", Value: audit.Unsigned(priorRevision)},
		{Name: "resulting_revision", Value: audit.Unsigned(resultingRevision)},
		{Name: "prior_current_version_id", Value: priorVersion},
		{Name: "resulting_current_version_id", Value: resultingVersion},
	}}, nil
}

func auditNodeCurrentVersion(node Node) (audit.Value, error) {
	if node.CurrentVersionID == "" {
		return audit.Absent(), nil
	}
	return audit.UUID(node.CurrentVersionID)
}

func positiveAuditRevision(value int64) (uint64, error) {
	return positiveAuditInteger("content revision", value)
}

func makeAuditedContentReplacementMutation(
	values auditedMutationValues, sequence int64,
	events []audit.Record, change audit.Record,
) (audit.Record, error) {
	auditSequence, err := positiveAuditInteger("operation sequence", sequence)
	if err != nil {
		return audit.Record{}, err
	}
	eventValues := make([]audit.Value, len(events))
	for index := range events {
		eventValues[index] = audit.Nested(events[index])
	}
	return audit.Record{Kind: "canonical_mutation", Fields: []audit.Field{
		{Name: auditVaultIDField, Value: values.vaultID},
		{Name: "operation_sequence", Value: audit.Unsigned(auditSequence)},
		{Name: auditOperationIDField, Value: values.operationID},
		{Name: "grouping_id", Value: audit.Absent()},
		{Name: "recorded_at", Value: values.recordedAt},
		{Name: auditOriginField, Value: values.origin},
		{Name: "agent_label", Value: audit.Absent()},
		{Name: "events", Value: audit.List(eventValues...)},
		{Name: "member_state_changes", Value: audit.List(audit.Nested(change))},
		{Name: "baselines", Value: audit.List()},
		{Name: auditTopologyDeltaField, Value: audit.Absent()},
		{Name: "path_effect_count", Value: audit.Unsigned(0)},
		{Name: "path_effect_digest", Value: audit.Absent()},
		{Name: auditWitnessChangeCountField, Value: audit.Unsigned(0)},
		{Name: "witness_change_digest", Value: audit.Absent()},
		{Name: auditAttachedMetadataChangeCountField, Value: audit.Unsigned(0)},
		{Name: "attached_metadata_change_digest", Value: audit.Absent()},
	}}, nil
}

func makeAuditScopeChainEntry(
	values auditedMutationValues, scopeID string, entryCount int64,
	previousHead string, mutationHash audit.Value,
) (audit.Record, error) {
	auditEntryCount, err := positiveAuditInteger("scope entry count", entryCount)
	if err != nil {
		return audit.Record{}, err
	}
	scopeValue, err := audit.UUID(scopeID)
	if err != nil {
		return audit.Record{}, err
	}
	previousValue, err := audit.DigestHex(previousHead)
	if err != nil {
		return audit.Record{}, err
	}
	return audit.Record{Kind: "scope_chain_entry", Fields: []audit.Field{
		{Name: auditVaultIDField, Value: values.vaultID},
		{Name: auditScopeIDField, Value: scopeValue},
		{Name: "entry_count", Value: audit.Unsigned(auditEntryCount)},
		{Name: "previous_head", Value: previousValue},
		{Name: "mutation_hash", Value: mutationHash},
	}}, nil
}

func makeAuditedContentAllocationEntry(
	values auditedMutationValues, sequence, nodeSequence int64,
	previousHead string, mutationHash audit.Value,
) (audit.Record, error) {
	auditSequence, err := positiveAuditInteger("operation sequence", sequence)
	if err != nil {
		return audit.Record{}, err
	}
	auditNodeSequence, err := positiveAuditInteger("node ID high-water mark", nodeSequence)
	if err != nil {
		return audit.Record{}, err
	}
	previousValue, err := audit.DigestHex(previousHead)
	if err != nil {
		return audit.Record{}, err
	}
	return audit.Record{Kind: "allocation_entry", Fields: []audit.Field{
		{Name: auditVaultIDField, Value: values.vaultID},
		{Name: "lineage_id", Value: values.lineageID},
		{Name: "previous_head", Value: previousValue},
		{Name: "operation_sequence", Value: audit.Unsigned(auditSequence)},
		{Name: auditOperationIDField, Value: values.operationID},
		{Name: "allocated_node_ids", Value: audit.List()},
		{Name: "node_id_high_water", Value: audit.Unsigned(auditNodeSequence)},
		{Name: "operation_sequence_high_water", Value: audit.Unsigned(auditSequence)},
		{Name: "has_audited_mutation", Value: audit.Bool(true)},
		{Name: "mutation_hash", Value: mutationHash},
		{Name: "has_topology_change", Value: audit.Bool(false)},
		{Name: auditTopologyDeltaField, Value: audit.Absent()},
		{Name: "has_witness_change", Value: audit.Bool(false)},
		{Name: auditWitnessChangeCountField, Value: audit.Unsigned(0)},
		{Name: "witness_change_digest", Value: audit.Absent()},
		{Name: "has_attached_metadata_change", Value: audit.Bool(false)},
		{Name: auditAttachedMetadataChangeCountField, Value: audit.Unsigned(0)},
		{Name: "attached_metadata_change_digest", Value: audit.Absent()},
	}}, nil
}

func positiveAuditInteger(name string, value int64) (uint64, error) {
	if value <= 0 {
		return 0, fmt.Errorf("audit %s must be positive, got %d", name, value)
	}
	return uint64(value), nil
}
