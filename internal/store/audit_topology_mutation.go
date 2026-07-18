package store

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/docbank/internal/audit"
)

func makeAuditedTopologyRecordDelta(
	operationID audit.Value, prior, resulting map[int64]audit.Record,
) (audit.Record, error) {
	if len(prior) != len(resulting) {
		return audit.Record{}, fmt.Errorf(
			"audited topology transition has %d prior nodes and %d resulting nodes",
			len(prior), len(resulting),
		)
	}
	changes := make([]audit.Record, 0, len(prior))
	for nodeID, pre := range prior {
		auditNodeID, err := positiveAuditNodeID(nodeID)
		if err != nil {
			return audit.Record{}, err
		}
		post, ok := resulting[nodeID]
		if !ok {
			return audit.Record{}, fmt.Errorf(
				"audited topology transition lacks resulting node %d", nodeID,
			)
		}
		changes = append(changes, audit.Record{Kind: "topology_change", Fields: []audit.Field{
			{Name: metadataNodeIDField, Value: audit.Unsigned(auditNodeID)},
			{Name: "pre", Value: audit.Nested(pre)},
			{Name: "post", Value: audit.Nested(post)},
		}})
	}
	if err := sortAuditTopologyRecords(changes); err != nil {
		return audit.Record{}, err
	}
	return makeAuditedTopologyDelta(operationID, changes), nil
}

// persistAuditedTopologyMutation commits the record envelope shared by
// topology changes that affect one existing audit scope. Callers remain
// responsible for deriving the operation-specific topology and path effects.
func persistAuditedTopologyMutation(
	ctx context.Context,
	tx *sql.Tx,
	values auditedMutationValues,
	authority auditAuthorityState,
	scope auditScopeState,
	nodeSequence int64,
	topology audit.Record,
	effects []audit.Record,
	priorNodes, resultingNodes, priorEventNodes, resultingEventNodes map[int64]Node,
) error {
	topologyDigest, err := hashAuditRecord(topology)
	if err != nil {
		return err
	}
	effectList := audit.Record{Kind: "path_effect_list", Fields: []audit.Field{
		{Name: auditOperationIDField, Value: values.operationID},
		{Name: auditTopologyDeltaField, Value: topologyDigest.value},
		{Name: "effects", Value: audit.List(auditNestedValues(effects)...)},
	}}
	effectDigest, err := hashAuditRecord(effectList)
	if err != nil {
		return err
	}
	stateChanges, err := makeAuditedTopologyStateChanges(priorNodes, resultingNodes)
	if err != nil {
		return err
	}
	events, err := makeAuditedPathEvents(
		values, scope.scopeID, priorEventNodes, resultingEventNodes, effects, topologyDigest,
	)
	if err != nil {
		return err
	}
	mutation, err := makeAuditedTopologyMutation(
		values, authority.sequence+1, events, stateChanges,
		topologyDigest, uint64(len(effects)), effectDigest,
	)
	if err != nil {
		return err
	}
	mutationDigest, err := hashAuditRecord(mutation)
	if err != nil {
		return err
	}
	for _, record := range []audit.Record{topology, effectList} {
		if err := insertAuditRecord(ctx, tx, record); err != nil {
			return err
		}
	}
	for _, event := range events {
		if err := insertAuditRecord(ctx, tx, audit.Record{Kind: auditEventField, Fields: []audit.Field{
			{Name: auditEventField, Value: audit.Nested(event)},
		}}); err != nil {
			return err
		}
	}
	if err := insertAuditRecord(ctx, tx, mutation); err != nil {
		return err
	}
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
	scopeHead, err := hashAuditRecord(entry)
	if err != nil {
		return err
	}
	if err := insertAuditRecord(ctx, tx, entry); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE audit_scopes SET entry_count=?,chain_head=? WHERE scope_id=?`,
		entryCount, scopeHead.text, scope.scopeID,
	); err != nil {
		return fmt.Errorf("advancing audit scope %s: %w", scope.scopeID, err)
	}
	allocation, err := makeAuditedTopologyAllocationEntry(
		values, authority.sequence+1, nodeSequence,
		authority.allocationHead, mutationDigest.value, topologyDigest.value,
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
	allocationCount, err := nextAuditInteger(
		"allocation entry count", authority.allocationCount,
	)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE audit_authority
		SET operation_sequence_high_water=?,allocation_entry_count=?,allocation_head=?
		WHERE singleton=1`, authority.sequence+1, allocationCount, allocationHead.text); err != nil {
		return fmt.Errorf("advancing audit allocation authority: %w", err)
	}
	return nil
}
