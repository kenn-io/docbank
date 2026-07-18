package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/docbank/internal/audit"
)

type auditedRestoreSnapshot struct {
	authority                auditAuthorityState
	scope                    auditScopeState
	nodeSequence             int64
	operationID, recordedAt  string
	target                   restoreTarget
	priorNodes, priorSubtree map[int64]Node
	priorTopology            map[int64]audit.Record
	priorPaths               map[int64]string
	subtreeIDs               []int64
}

func (s *Store) restoreAuditedTx(
	ctx context.Context, tx *sql.Tx, node Node, ifRev int64,
) (Node, error) {
	snapshot, err := s.prepareAuditedRestore(ctx, tx, node, ifRev)
	if err != nil {
		return Node{}, err
	}
	restored, err := s.restoreNodeTx(tx, node, snapshot.target, snapshot.recordedAt)
	if err != nil {
		return Node{}, err
	}
	resultingNodes := make(map[int64]Node, len(snapshot.priorNodes))
	resultingTopology := make(map[int64]audit.Record, len(snapshot.priorNodes))
	for nodeID := range snapshot.priorNodes {
		resulting, err := nodeByIDTx(tx, nodeID)
		if err != nil {
			return Node{}, err
		}
		topology, err := auditTopologyForNodeTx(ctx, tx, nodeID)
		if err != nil {
			return Node{}, err
		}
		resultingNodes[nodeID], resultingTopology[nodeID] = resulting, topology
	}
	resultingSubtree := make(map[int64]Node, len(snapshot.subtreeIDs))
	resultingPaths := make(map[int64]string, len(snapshot.subtreeIDs))
	for _, nodeID := range snapshot.subtreeIDs {
		path, err := pathOf(ctx, tx, nodeID)
		if err != nil {
			return Node{}, err
		}
		resultingSubtree[nodeID], resultingPaths[nodeID] = resultingNodes[nodeID], path
	}
	values, err := makeAuditedMutationValues(
		s.vaultID, snapshot.authority.lineageID, snapshot.operationID, snapshot.recordedAt,
	)
	if err != nil {
		return Node{}, err
	}
	topology, err := makeAuditedTopologyRecordDelta(
		values.operationID, snapshot.priorTopology, resultingTopology,
	)
	if err != nil {
		return Node{}, err
	}
	effects, err := makeAuditedRestorePathEffects(snapshot, resultingPaths)
	if err != nil {
		return Node{}, err
	}
	if err := persistAuditedTopologyMutation(
		ctx, tx, values, snapshot.authority, snapshot.scope, snapshot.nodeSequence,
		topology, effects, snapshot.priorNodes, resultingNodes,
		snapshot.priorSubtree, resultingSubtree,
	); err != nil {
		return Node{}, err
	}
	return restored, nil
}

func (s *Store) prepareAuditedRestore(
	ctx context.Context, tx *sql.Tx, node Node, ifRev int64,
) (auditedRestoreSnapshot, error) {
	var snapshot auditedRestoreSnapshot
	if node.TrashedAt == nil {
		return snapshot, fmt.Errorf("node %d: %w", node.ID, ErrNotTrashed)
	}
	if ifRev != UnconditionalRev && node.Revision != ifRev {
		return snapshot, fmt.Errorf(
			"node %d at revision %d, expected %d: %w",
			node.ID, node.Revision, ifRev, ErrStaleRevision,
		)
	}
	target, err := s.restoreTargetTx(tx, node)
	if err != nil {
		return snapshot, err
	}
	if target.originParentID == nil || target.destID != *target.originParentID {
		return snapshot, fmt.Errorf(
			"restoring node %d requires a scope-boundary fallback: %w",
			node.ID, ErrAuditMutationUnsupported,
		)
	}
	authority, scopes, nodeSequence, err := loadAuditedNodeAuthority(ctx, tx, node.ID)
	if err != nil {
		return snapshot, err
	}
	if len(scopes) != 1 {
		return snapshot, unsupportedAuditedNodeMutation(node.ID)
	}
	scope := scopes[0]
	if target.finalName != target.originalName {
		scopeMembers, err := auditScopeMemberSetTx(ctx, tx, scope.scopeID)
		if err != nil {
			return snapshot, err
		}
		topology, err := currentAuditTopology(ctx, tx)
		if err != nil {
			return snapshot, err
		}
		auditNodeID, err := positiveAuditNodeID(node.ID)
		if err != nil {
			return snapshot, err
		}
		affectsTrashOrigin, err := auditMoveAffectsTrashOrigin(
			topology, scopeMembers, auditNodeID,
		)
		if err != nil {
			return snapshot, err
		}
		if affectsTrashOrigin {
			return snapshot, fmt.Errorf(
				"restoring node %d under a conflict suffix would change retained trash-origin paths: %w",
				node.ID, ErrAuditMutationUnsupported,
			)
		}
	}
	destinationMember, err := nodeInAuditScopeTx(ctx, tx, target.destID, scope.scopeID)
	if err != nil {
		return snapshot, err
	}
	if !destinationMember {
		return snapshot, unsupportedAuditedNodeMutation(target.destID)
	}
	subtreeIDs, err := trashedSubtreeIDsTx(ctx, tx, node.ID, *node.TrashedAt)
	if err != nil {
		return snapshot, err
	}
	priorSubtree := make(map[int64]Node, len(subtreeIDs))
	for _, nodeID := range subtreeIDs {
		member, err := nodeInAuditScopeTx(ctx, tx, nodeID, scope.scopeID)
		if err != nil {
			return snapshot, err
		}
		if !member {
			return snapshot, unsupportedAuditedNodeMutation(nodeID)
		}
		prior, err := nodeByIDTx(tx, nodeID)
		if err != nil {
			return snapshot, err
		}
		priorSubtree[nodeID] = prior
	}
	originParentPath, err := pathOf(ctx, tx, target.destID)
	if err != nil {
		return snapshot, err
	}
	rootTrashPath := append([]byte("@trash/known"), []byte(originParentPath)...)
	if rootTrashPath[len(rootTrashPath)-1] != '/' {
		rootTrashPath = append(rootTrashPath, '/')
	}
	rootTrashPath = append(rootTrashPath, []byte(target.originalName)...)
	priorPaths := make(map[int64]string, len(subtreeIDs))
	for _, nodeID := range subtreeIDs {
		path, err := auditedTrashSubtreePath(
			priorSubtree, node.ID, nodeID, string(rootTrashPath),
		)
		if err != nil {
			return snapshot, err
		}
		priorPaths[nodeID] = path
	}
	changedIDs := append(slices.Clone(subtreeIDs), target.destID)
	slices.Sort(changedIDs)
	priorNodes := make(map[int64]Node, len(changedIDs))
	priorTopology := make(map[int64]audit.Record, len(changedIDs))
	for _, nodeID := range changedIDs {
		prior, err := nodeByIDTx(tx, nodeID)
		if err != nil {
			return snapshot, err
		}
		topology, err := auditTopologyForNodeTx(ctx, tx, nodeID)
		if err != nil {
			return snapshot, err
		}
		priorNodes[nodeID], priorTopology[nodeID] = prior, topology
	}
	operationID, err := newUUIDv4()
	if err != nil {
		return snapshot, err
	}
	return auditedRestoreSnapshot{
		authority: authority, scope: scope, nodeSequence: nodeSequence,
		operationID: operationID, recordedAt: nowRFC3339(), target: target,
		priorNodes: priorNodes, priorSubtree: priorSubtree, priorTopology: priorTopology,
		priorPaths: priorPaths, subtreeIDs: subtreeIDs,
	}, nil
}

func trashedSubtreeIDsTx(
	ctx context.Context, tx *sql.Tx, rootID int64, trashedAt string,
) ([]int64, error) {
	rows, err := tx.QueryContext(
		ctx, `SELECT id,parent_id FROM nodes WHERE trashed_at=? ORDER BY id`, trashedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("listing trashed subtree of node %d: %w", rootID, err)
	}
	defer func() { _ = rows.Close() }()
	children := make(map[int64][]int64)
	foundRoot := false
	for rows.Next() {
		var id int64
		var parentID sql.NullInt64
		if err := rows.Scan(&id, &parentID); err != nil {
			return nil, fmt.Errorf("scanning trashed subtree of node %d: %w", rootID, err)
		}
		foundRoot = foundRoot || id == rootID
		if parentID.Valid {
			children[parentID.Int64] = append(children[parentID.Int64], id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing trashed subtree of node %d: %w", rootID, err)
	}
	if !foundRoot {
		return nil, fmt.Errorf("node %d is not a restorable trash root: %w", rootID, ErrNotTrashed)
	}
	visited := make(map[int64]bool)
	var result []int64
	var visit func(int64) error
	visit = func(nodeID int64) error {
		if visited[nodeID] {
			return errors.New("trashed subtree contains a parent cycle")
		}
		visited[nodeID] = true
		result = append(result, nodeID)
		for _, childID := range children[nodeID] {
			if err := visit(childID); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(rootID); err != nil {
		return nil, err
	}
	slices.Sort(result)
	return result, nil
}

func auditedTrashSubtreePath(
	nodes map[int64]Node, rootID, nodeID int64, rootPath string,
) (string, error) {
	if nodeID == rootID {
		return rootPath, nil
	}
	visited := make(map[int64]bool)
	var names []string
	currentID := nodeID
	for currentID != rootID {
		if visited[currentID] {
			return "", errors.New("audited trash subtree contains a parent cycle")
		}
		visited[currentID] = true
		current, ok := nodes[currentID]
		if !ok || current.ParentID == nil {
			return "", fmt.Errorf("audited trash subtree node %d lacks its parent", currentID)
		}
		names = append(names, current.Name)
		currentID = *current.ParentID
	}
	result := []byte(rootPath)
	for _, name := range slices.Backward(names) {
		result = append(result, '/')
		result = append(result, name...)
	}
	return string(result), nil
}

func makeAuditedRestorePathEffects(
	snapshot auditedRestoreSnapshot, resultingPaths map[int64]string,
) ([]audit.Record, error) {
	scopeID, err := audit.UUID(snapshot.scope.scopeID)
	if err != nil {
		return nil, err
	}
	trash, err := audit.Text(auditNodeStateTrash)
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
		if priorPath == "" || resultingPath == "" {
			return nil, fmt.Errorf("audited restore lacks a path for node %d", nodeID)
		}
		effects = append(effects, audit.Record{Kind: "path_effect", Fields: []audit.Field{
			{Name: auditScopeIDField, Value: scopeID},
			{Name: "member_node_id", Value: audit.Unsigned(auditNodeID)},
			{Name: "old", Value: audit.Nested(audit.Record{Kind: auditPathStateKind, Fields: []audit.Field{
				{Name: auditPathField, Value: audit.Bytes([]byte(priorPath))}, {Name: auditStateField, Value: trash},
			}})},
			{Name: "new", Value: audit.Nested(audit.Record{Kind: auditPathStateKind, Fields: []audit.Field{
				{Name: auditPathField, Value: audit.Bytes([]byte(resultingPath))}, {Name: auditStateField, Value: live},
			}})},
		}})
	}
	return effects, nil
}
