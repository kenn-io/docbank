package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
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
	effects, err := makeAuditedMovePathEffects(snapshot, resultingPaths)
	if err != nil {
		return err
	}
	return persistAuditedTopologyMutation(
		ctx, tx, values, snapshot.authority, snapshot.scope, snapshot.nodeSequence,
		topology, effects, snapshot.priorNodes, resultingNodes,
		snapshot.priorSubtree, resultingSubtree,
	)
}

func makeAuditedMoveTopologyDelta(
	operationID audit.Value, prior, resulting map[int64]Node,
) (audit.Record, error) {
	priorRecords := make(map[int64]audit.Record, len(prior))
	resultingRecords := make(map[int64]audit.Record, len(resulting))
	for nodeID, priorNode := range prior {
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
		priorRecords[nodeID], resultingRecords[nodeID] = pre, post
	}
	return makeAuditedTopologyRecordDelta(operationID, priorRecords, resultingRecords)
}

func makeAuditedTopologyDelta(operationID audit.Value, changes []audit.Record) audit.Record {
	return audit.Record{Kind: auditTopologyDeltaField, Fields: []audit.Field{
		{Name: auditOperationIDField, Value: operationID},
		{Name: "changes", Value: audit.List(auditNestedValues(changes)...)},
	}}
}

func makeAuditedMovePathEffects(
	snapshot auditedMoveSnapshot, resultingPaths map[int64]string,
) ([]audit.Record, error) {
	scopeID, err := audit.UUID(snapshot.scope.scopeID)
	if err != nil {
		return nil, err
	}
	live, err := audit.Text(auditNodeStateLive)
	if err != nil {
		return nil, err
	}
	effects := make([]audit.Record, 0, len(snapshot.subtreeIDs))
	for _, nodeID := range snapshot.subtreeIDs {
		auditNodeID, err := positiveAuditNodeID(nodeID)
		if err != nil {
			return nil, err
		}
		priorPath, resultingPath := snapshot.priorPaths[nodeID], resultingPaths[nodeID]
		if priorPath == resultingPath {
			return nil, fmt.Errorf(
				"audited move did not change path of subtree node %d", nodeID,
			)
		}
		effects = append(effects, audit.Record{Kind: "path_effect", Fields: []audit.Field{
			{Name: auditScopeIDField, Value: scopeID},
			{Name: "member_node_id", Value: audit.Unsigned(auditNodeID)},
			{Name: "old", Value: audit.Nested(audit.Record{Kind: auditPathStateKind, Fields: []audit.Field{
				{Name: auditPathField, Value: audit.Bytes([]byte(priorPath))},
				{Name: auditStateField, Value: live},
			}})},
			{Name: "new", Value: audit.Nested(audit.Record{Kind: auditPathStateKind, Fields: []audit.Field{
				{Name: auditPathField, Value: audit.Bytes([]byte(resultingPath))},
				{Name: auditStateField, Value: live},
			}})},
		}})
	}
	return effects, nil
}

func makeAuditedTopologyStateChanges(
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

func makeAuditedPathEvents(
	values auditedMutationValues, scopeID string, prior, resulting map[int64]Node,
	effects []audit.Record, topologyDigest auditRecordHash,
) ([]audit.Record, error) {
	events := make([]audit.Record, len(effects))
	ordinal := uint64(0)
	for index, effect := range effects {
		auditNodeID, err := auditUnsignedField(effect, "member_node_id")
		if err != nil {
			return nil, err
		}
		if auditNodeID > math.MaxInt64 {
			return nil, fmt.Errorf("audited path effect node %d exceeds signed node range", auditNodeID)
		}
		nodeID := int64(auditNodeID)
		priorNode, priorOK := prior[nodeID]
		resultingNode, resultingOK := resulting[nodeID]
		if !priorOK || !resultingOK {
			return nil, fmt.Errorf("audited path effect lacks node state for %d", nodeID)
		}
		events[index], err = makeAuditedPathEvent(
			values, scopeID, ordinal, priorNode, resultingNode, effect, topologyDigest,
		)
		if err != nil {
			return nil, err
		}
		ordinal++
	}
	return events, nil
}

func makeAuditedPathEvent(
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

func makeAuditedTopologyMutation(
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

func makeAuditedTopologyAllocationEntry(
	values auditedMutationValues, sequence, nodeSequence int64,
	previousHead string, mutationHash, topologyDigest audit.Value,
) (audit.Record, error) {
	entry, err := makeAuditedMutationAllocationEntry(
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
