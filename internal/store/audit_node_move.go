package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/docbank/internal/audit"
)

type auditedMoveSnapshot struct {
	authority                auditAuthorityState
	scope                    auditScopeState
	nodeSequence             int64
	operationID, recordedAt  string
	priorNodes, priorSubtree map[int64]Node
	priorPaths               map[int64]string
	subtreeIDs               []int64
}

func (s *Store) moveAuditedTx(
	ctx context.Context, tx *sql.Tx, id, newParentID int64, newName string, ifRev int64,
) (Node, error) {
	snapshot, normalizedName, noChange, err := s.prepareAuditedMove(
		ctx, tx, id, newParentID, newName, ifRev,
	)
	if err != nil {
		return Node{}, err
	}
	if noChange {
		return snapshot.priorSubtree[id], nil
	}
	moved, err := s.moveAtTx(
		tx, id, newParentID, normalizedName, ifRev, snapshot.recordedAt,
	)
	if err != nil {
		return Node{}, err
	}
	resultingNodes := make(map[int64]Node, len(snapshot.priorNodes))
	for nodeID := range snapshot.priorNodes {
		node, err := nodeByIDTx(tx, nodeID)
		if err != nil {
			return Node{}, err
		}
		resultingNodes[nodeID] = node
	}
	resultingSubtree := make(map[int64]Node, len(snapshot.subtreeIDs))
	resultingPaths := make(map[int64]string, len(snapshot.subtreeIDs))
	for _, nodeID := range snapshot.subtreeIDs {
		node, err := nodeByIDTx(tx, nodeID)
		if err != nil {
			return Node{}, err
		}
		path, err := pathOf(ctx, tx, nodeID)
		if err != nil {
			return Node{}, err
		}
		resultingSubtree[nodeID], resultingPaths[nodeID] = node, path
	}
	if err := persistAuditedMove(
		ctx, tx, s.vaultID, snapshot, resultingNodes, resultingSubtree, resultingPaths,
	); err != nil {
		return Node{}, err
	}
	return moved, nil
}

func (s *Store) prepareAuditedMove(
	ctx context.Context, tx *sql.Tx, id, newParentID int64, newName string, ifRev int64,
) (auditedMoveSnapshot, string, bool, error) {
	var snapshot auditedMoveSnapshot
	if id == s.rootID {
		return snapshot, "", false, ErrIsRoot
	}
	normalizedName, err := NormalizeName(newName)
	if err != nil {
		return snapshot, "", false, err
	}
	moved, err := nodeByIDTx(tx, id)
	if err != nil {
		return snapshot, "", false, err
	}
	if moved.TrashedAt != nil {
		return snapshot, "", false, fmt.Errorf("node %d is trashed: %w", id, ErrNotFound)
	}
	if ifRev != UnconditionalRev && moved.Revision != ifRev {
		return snapshot, "", false, fmt.Errorf(
			"node %d at revision %d, expected %d: %w", id, moved.Revision, ifRev, ErrStaleRevision,
		)
	}
	if _, err := liveDirTx(tx, newParentID); err != nil {
		return snapshot, "", false, err
	}
	inCycle, err := isAncestorTx(tx, id, newParentID)
	if err != nil {
		return snapshot, "", false, err
	}
	if inCycle {
		return snapshot, "", false, fmt.Errorf("moving node %d under %d: %w", id, newParentID, ErrCycle)
	}
	authority, scopes, nodeSequence, err := loadAuditedNodeAuthority(ctx, tx, id)
	if err != nil {
		return snapshot, "", false, err
	}
	if len(scopes) != 1 {
		return snapshot, "", false, unsupportedAuditedNodeMutation(id)
	}
	oldParentID := *moved.ParentID
	if oldParentID == newParentID && moved.Name == normalizedName {
		snapshot.priorSubtree = map[int64]Node{id: moved}
		return snapshot, normalizedName, true, nil
	}
	scopeMembers, err := auditScopeMemberSetTx(ctx, tx, scopes[0].scopeID)
	if err != nil {
		return snapshot, "", false, err
	}
	topology, err := currentAuditTopology(ctx, tx)
	if err != nil {
		return snapshot, "", false, err
	}
	auditNodeID, err := positiveAuditNodeID(id)
	if err != nil {
		return snapshot, "", false, err
	}
	affectsTrashOrigin, err := auditMoveAffectsTrashOrigin(
		topology, scopeMembers, auditNodeID,
	)
	if err != nil {
		return snapshot, "", false, err
	}
	if affectsTrashOrigin {
		return snapshot, "", false, fmt.Errorf(
			"moving node %d would change retained trash-origin paths: %w",
			id, ErrAuditMutationUnsupported,
		)
	}
	for _, parentID := range []int64{oldParentID, newParentID} {
		member, err := nodeInAuditScopeTx(ctx, tx, parentID, scopes[0].scopeID)
		if err != nil {
			return snapshot, "", false, err
		}
		if !member {
			return snapshot, "", false, unsupportedAuditedNodeMutation(parentID)
		}
	}
	subtreeIDs, err := liveSubtreeIDsTx(ctx, tx, id)
	if err != nil {
		return snapshot, "", false, err
	}
	priorSubtree := make(map[int64]Node, len(subtreeIDs))
	priorPaths := make(map[int64]string, len(subtreeIDs))
	for _, nodeID := range subtreeIDs {
		member, err := nodeInAuditScopeTx(ctx, tx, nodeID, scopes[0].scopeID)
		if err != nil {
			return snapshot, "", false, err
		}
		if !member {
			return snapshot, "", false, unsupportedAuditedNodeMutation(nodeID)
		}
		node, err := nodeByIDTx(tx, nodeID)
		if err != nil {
			return snapshot, "", false, err
		}
		path, err := pathOf(ctx, tx, nodeID)
		if err != nil {
			return snapshot, "", false, err
		}
		priorSubtree[nodeID], priorPaths[nodeID] = node, path
	}
	directIDs := []int64{id, oldParentID}
	if newParentID != oldParentID {
		directIDs = append(directIDs, newParentID)
	}
	slices.Sort(directIDs)
	priorNodes := make(map[int64]Node, len(directIDs))
	for _, nodeID := range directIDs {
		node, err := nodeByIDTx(tx, nodeID)
		if err != nil {
			return snapshot, "", false, err
		}
		priorNodes[nodeID] = node
	}
	operationID, err := newUUIDv4()
	if err != nil {
		return snapshot, "", false, err
	}
	snapshot = auditedMoveSnapshot{
		authority: authority, scope: scopes[0], nodeSequence: nodeSequence,
		operationID: operationID, recordedAt: nowRFC3339(),
		priorNodes: priorNodes, priorSubtree: priorSubtree,
		priorPaths: priorPaths, subtreeIDs: subtreeIDs,
	}
	return snapshot, normalizedName, false, nil
}

func nodeInAuditScopeTx(
	ctx context.Context, tx *sql.Tx, nodeID int64, scopeID string,
) (bool, error) {
	var member bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM audit_memberships WHERE scope_id=? AND node_id=?
	)`, scopeID, nodeID).Scan(&member); err != nil {
		return false, fmt.Errorf("checking audit membership for node %d: %w", nodeID, err)
	}
	return member, nil
}

func auditScopeMemberSetTx(
	ctx context.Context, tx *sql.Tx, scopeID string,
) (map[uint64]bool, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT node_id FROM audit_memberships WHERE scope_id=? ORDER BY node_id`, scopeID)
	if err != nil {
		return nil, fmt.Errorf("listing audit scope %s members: %w", scopeID, err)
	}
	defer func() { _ = rows.Close() }()
	result := make(map[uint64]bool)
	for rows.Next() {
		var rawID int64
		if err := rows.Scan(&rawID); err != nil {
			return nil, fmt.Errorf("scanning audit scope %s member: %w", scopeID, err)
		}
		nodeID, err := positiveAuditNodeID(rawID)
		if err != nil {
			return nil, err
		}
		result[nodeID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing audit scope %s members: %w", scopeID, err)
	}
	return result, nil
}

func liveSubtreeIDsTx(ctx context.Context, tx *sql.Tx, rootID int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `WITH RECURSIVE subtree(id) AS (
		SELECT id FROM nodes WHERE id=? AND trashed_at IS NULL
		UNION ALL
		SELECT node.id FROM nodes node JOIN subtree ON node.parent_id=subtree.id
		WHERE node.trashed_at IS NULL
	) SELECT id FROM subtree ORDER BY id`, rootID)
	if err != nil {
		return nil, fmt.Errorf("listing live subtree of node %d: %w", rootID, err)
	}
	defer func() { _ = rows.Close() }()
	var result []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning live subtree of node %d: %w", rootID, err)
		}
		result = append(result, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing live subtree of node %d: %w", rootID, err)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("node %d: %w", rootID, ErrNotFound)
	}
	return result, nil
}

func persistAuditedMove(
	ctx context.Context, tx *sql.Tx, vaultID string, snapshot auditedMoveSnapshot,
	resultingNodes, resultingSubtree map[int64]Node, resultingPaths map[int64]string,
) error {
	values, err := makeAuditedMutationValues(
		vaultID, snapshot.authority.lineageID, snapshot.operationID, snapshot.recordedAt,
	)
	if err != nil {
		return err
	}
	topology, err := makeAuditedMoveTopologyDelta(
		values.operationID, snapshot.priorNodes, resultingNodes,
	)
	if err != nil {
		return err
	}
	topologyDigest, err := hashAuditRecord(topology)
	if err != nil {
		return err
	}
	effects, effectList, effectDigest, err := makeAuditedMovePathEffects(
		values, snapshot, resultingPaths, topologyDigest,
	)
	if err != nil {
		return err
	}
	stateChanges, err := makeAuditedMoveStateChanges(snapshot.priorNodes, resultingNodes)
	if err != nil {
		return err
	}
	events, err := makeAuditedMoveEvents(
		values, snapshot.scope.scopeID, snapshot, resultingSubtree, effects, topologyDigest,
	)
	if err != nil {
		return err
	}
	mutation, err := makeAuditedMoveMutation(
		values, snapshot.authority.sequence+1, events, stateChanges,
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
	entryCount, err := nextAuditInteger("scope entry count", snapshot.scope.entryCount)
	if err != nil {
		return err
	}
	entry, err := makeAuditScopeChainEntry(
		values, snapshot.scope.scopeID, entryCount, snapshot.scope.chainHead, mutationDigest.value,
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
	if _, err := tx.ExecContext(ctx, `UPDATE audit_scopes SET entry_count=?,chain_head=? WHERE scope_id=?`,
		entryCount, scopeHead.text, snapshot.scope.scopeID); err != nil {
		return fmt.Errorf("advancing audit scope %s: %w", snapshot.scope.scopeID, err)
	}
	allocation, err := makeAuditedMoveAllocationEntry(
		values, snapshot.authority.sequence+1, snapshot.nodeSequence,
		snapshot.authority.allocationHead, mutationDigest.value, topologyDigest.value,
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
		"allocation entry count", snapshot.authority.allocationCount,
	)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE audit_authority
		SET operation_sequence_high_water=?,allocation_entry_count=?,allocation_head=?
		WHERE singleton=1`, snapshot.authority.sequence+1, allocationCount, allocationHead.text); err != nil {
		return fmt.Errorf("advancing audit allocation authority: %w", err)
	}
	return nil
}

func makeAuditedMoveTopologyDelta(
	operationID audit.Value, prior, resulting map[int64]Node,
) (audit.Record, error) {
	changes := make([]audit.Record, 0, len(prior))
	for nodeID, priorNode := range prior {
		auditNodeID, err := positiveAuditNodeID(nodeID)
		if err != nil {
			return audit.Record{}, err
		}
		resultingNode, ok := resulting[nodeID]
		if !ok {
			return audit.Record{}, fmt.Errorf("audited move lacks resulting node %d", nodeID)
		}
		pre, err := auditTopologyForLiveNode(priorNode)
		if err != nil {
			return audit.Record{}, err
		}
		post, err := auditTopologyForLiveNode(resultingNode)
		if err != nil {
			return audit.Record{}, err
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
	return audit.Record{Kind: auditTopologyDeltaField, Fields: []audit.Field{
		{Name: auditOperationIDField, Value: operationID},
		{Name: "changes", Value: audit.List(auditNestedValues(changes)...)},
	}}, nil
}

func makeAuditedMovePathEffects(
	values auditedMutationValues, snapshot auditedMoveSnapshot,
	resultingPaths map[int64]string, topologyDigest auditRecordHash,
) ([]audit.Record, audit.Record, auditRecordHash, error) {
	scopeID, err := audit.UUID(snapshot.scope.scopeID)
	if err != nil {
		return nil, audit.Record{}, auditRecordHash{}, err
	}
	live, err := audit.Text("live")
	if err != nil {
		return nil, audit.Record{}, auditRecordHash{}, err
	}
	effects := make([]audit.Record, 0, len(snapshot.subtreeIDs))
	for _, nodeID := range snapshot.subtreeIDs {
		auditNodeID, err := positiveAuditNodeID(nodeID)
		if err != nil {
			return nil, audit.Record{}, auditRecordHash{}, err
		}
		priorPath, resultingPath := snapshot.priorPaths[nodeID], resultingPaths[nodeID]
		if priorPath == resultingPath {
			return nil, audit.Record{}, auditRecordHash{}, fmt.Errorf(
				"audited move did not change path of subtree node %d", nodeID,
			)
		}
		effects = append(effects, audit.Record{Kind: "path_effect", Fields: []audit.Field{
			{Name: auditScopeIDField, Value: scopeID},
			{Name: "member_node_id", Value: audit.Unsigned(auditNodeID)},
			{Name: "old", Value: audit.Nested(audit.Record{Kind: "path_state", Fields: []audit.Field{
				{Name: "path", Value: audit.Bytes([]byte(priorPath))},
				{Name: "state", Value: live},
			}})},
			{Name: "new", Value: audit.Nested(audit.Record{Kind: "path_state", Fields: []audit.Field{
				{Name: "path", Value: audit.Bytes([]byte(resultingPath))},
				{Name: "state", Value: live},
			}})},
		}})
	}
	list := audit.Record{Kind: "path_effect_list", Fields: []audit.Field{
		{Name: auditOperationIDField, Value: values.operationID},
		{Name: auditTopologyDeltaField, Value: topologyDigest.value},
		{Name: "effects", Value: audit.List(auditNestedValues(effects)...)},
	}}
	digest, err := hashAuditRecord(list)
	return effects, list, digest, err
}

func makeAuditedMoveStateChanges(
	prior, resulting map[int64]Node,
) ([]audit.Record, error) {
	changes := make([]audit.Record, 0, len(prior))
	for nodeID, priorNode := range prior {
		resultingNode, ok := resulting[nodeID]
		if !ok {
			return nil, fmt.Errorf("audited move lacks resulting member %d", nodeID)
		}
		change, err := makeAuditMemberStateChange(priorNode, resultingNode)
		if err != nil {
			return nil, err
		}
		changes = append(changes, change)
	}
	if err := sortAuditTopologyRecords(changes); err != nil {
		return nil, err
	}
	return changes, nil
}

func makeAuditedMoveEvents(
	values auditedMutationValues, scopeID string, snapshot auditedMoveSnapshot,
	resultingSubtree map[int64]Node, effects []audit.Record, topologyDigest auditRecordHash,
) ([]audit.Record, error) {
	if len(effects) != len(snapshot.subtreeIDs) {
		return nil, errors.New("audited move path effects do not match its subtree")
	}
	events := make([]audit.Record, len(effects))
	ordinal := uint64(0)
	for index, effect := range effects {
		nodeID := snapshot.subtreeIDs[index]
		auditNodeID, err := positiveAuditNodeID(nodeID)
		if err != nil {
			return nil, err
		}
		if err := requireAuditUnsigned(effect, "member_node_id", auditNodeID); err != nil {
			return nil, err
		}
		events[index], err = makeAuditedMoveEvent(
			values, scopeID, ordinal, snapshot.priorSubtree[nodeID],
			resultingSubtree[nodeID], effect, topologyDigest,
		)
		if err != nil {
			return nil, err
		}
		ordinal++
	}
	return events, nil
}

func makeAuditedMoveEvent(
	values auditedMutationValues, scopeID string, ordinal uint64,
	prior, resulting Node, effect audit.Record, topologyDigest auditRecordHash,
) (audit.Record, error) {
	nodeID, err := positiveAuditNodeID(prior.ID)
	if err != nil || resulting.ID != prior.ID {
		return audit.Record{}, errors.New("audited move changes affected node identity")
	}
	scopeValue, err := audit.UUID(scopeID)
	if err != nil {
		return audit.Record{}, err
	}
	kind, err := audit.Text("node_path")
	if err != nil {
		return audit.Record{}, err
	}
	identity, err := hashAuditRecord(audit.Record{Kind: "event_identity", Fields: []audit.Field{
		{Name: auditOperationIDField, Value: values.operationID},
		{Name: auditEventOrdinalField, Value: audit.Unsigned(ordinal)},
	}})
	if err != nil {
		return audit.Record{}, err
	}
	pre, err := auditNestedField(effect, "old")
	if err != nil {
		return audit.Record{}, err
	}
	post, err := auditNestedField(effect, "new")
	if err != nil {
		return audit.Record{}, err
	}
	priorVersion, err := auditNodeCurrentVersion(prior)
	if err != nil {
		return audit.Record{}, err
	}
	resultingVersion, err := auditNodeCurrentVersion(resulting)
	if err != nil {
		return audit.Record{}, err
	}
	priorRevision, err := positiveAuditRevision(prior.Revision)
	if err != nil {
		return audit.Record{}, err
	}
	resultingRevision, err := positiveAuditRevision(resulting.Revision)
	if err != nil {
		return audit.Record{}, err
	}
	return audit.Record{Kind: "audit_event", Fields: []audit.Field{
		{Name: "event_id", Value: identity.value},
		{Name: auditOperationIDField, Value: values.operationID},
		{Name: metadataNodeIDField, Value: audit.Unsigned(nodeID)},
		{Name: "event_kind", Value: kind},
		{Name: auditScopeIDField, Value: scopeValue},
		{Name: "target_node_id", Value: audit.Absent()},
		{Name: "attachment_kind", Value: audit.Absent()},
		{Name: "attachment_identity", Value: audit.Absent()},
		{Name: "source_version_id", Value: audit.Absent()},
		{Name: auditEventOrdinalField, Value: audit.Unsigned(ordinal)},
		{Name: auditRecordedAtField, Value: values.recordedAt},
		{Name: "prior_node_revision", Value: audit.Unsigned(priorRevision)},
		{Name: "resulting_node_revision", Value: audit.Unsigned(resultingRevision)},
		{Name: "prior_current_version_id", Value: priorVersion},
		{Name: "resulting_current_version_id", Value: resultingVersion},
		{Name: auditOriginField, Value: values.origin},
		{Name: "agent_label", Value: audit.Absent()},
		{Name: "pre", Value: audit.Nested(pre)},
		{Name: "post", Value: audit.Nested(post)},
		{Name: auditTopologyDeltaField, Value: topologyDigest.value},
		{Name: "baseline_digest", Value: audit.Absent()},
	}}, nil
}

func makeAuditedMoveMutation(
	values auditedMutationValues, sequence int64, events, changes []audit.Record,
	topologyDigest auditRecordHash, effectCount uint64, effectDigest auditRecordHash,
) (audit.Record, error) {
	auditSequence, err := positiveAuditInteger("operation sequence", sequence)
	if err != nil {
		return audit.Record{}, err
	}
	return audit.Record{Kind: "canonical_mutation", Fields: []audit.Field{
		{Name: auditVaultIDField, Value: values.vaultID},
		{Name: "operation_sequence", Value: audit.Unsigned(auditSequence)},
		{Name: auditOperationIDField, Value: values.operationID},
		{Name: "grouping_id", Value: audit.Absent()},
		{Name: auditRecordedAtField, Value: values.recordedAt},
		{Name: auditOriginField, Value: values.origin},
		{Name: "agent_label", Value: audit.Absent()},
		{Name: "events", Value: audit.List(auditNestedValues(events)...)},
		{Name: "member_state_changes", Value: audit.List(auditNestedValues(changes)...)},
		{Name: "baselines", Value: audit.List()},
		{Name: auditTopologyDeltaField, Value: topologyDigest.value},
		{Name: "path_effect_count", Value: audit.Unsigned(effectCount)},
		{Name: "path_effect_digest", Value: effectDigest.value},
		{Name: auditWitnessChangeCountField, Value: audit.Unsigned(0)},
		{Name: "witness_change_digest", Value: audit.Absent()},
		{Name: auditAttachedMetadataChangeCountField, Value: audit.Unsigned(0)},
		{Name: "attached_metadata_change_digest", Value: audit.Absent()},
	}}, nil
}

func makeAuditedMoveAllocationEntry(
	values auditedMutationValues, sequence, nodeSequence int64,
	previousHead string, mutationHash, topologyDigest audit.Value,
) (audit.Record, error) {
	entry, err := makeAuditedContentAllocationEntry(
		values, sequence, nodeSequence, previousHead, mutationHash,
	)
	if err != nil {
		return audit.Record{}, err
	}
	entry, err = replaceAuditRecordField(entry, "has_topology_change", audit.Bool(true))
	if err != nil {
		return audit.Record{}, err
	}
	return replaceAuditRecordField(entry, auditTopologyDeltaField, topologyDigest)
}
