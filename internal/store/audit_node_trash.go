package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/docbank/internal/audit"
)

type auditedTrashSnapshot struct {
	authority                auditAuthorityState
	scope                    auditScopeState
	nodeSequence             int64
	operationID, recordedAt  string
	priorNodes, priorSubtree map[int64]Node
	priorPaths               map[int64]string
	subtreeIDs               []int64
}

func (s *Store) trashAuditedTx(
	ctx context.Context, tx *sql.Tx, node Node, ifRev int64,
) (Node, string, error) {
	snapshot, err := s.prepareAuditedTrash(ctx, tx, node, ifRev)
	if err != nil {
		return Node{}, "", err
	}
	if err := s.trashNodeTx(tx, node, snapshot.recordedAt); err != nil {
		return Node{}, "", err
	}
	resultingNodes := make(map[int64]Node, len(snapshot.priorNodes))
	resultingTopology := make(map[int64]audit.Record, len(snapshot.priorNodes))
	for nodeID := range snapshot.priorNodes {
		resulting, err := nodeByIDTx(tx, nodeID)
		if err != nil {
			return Node{}, "", err
		}
		topology, err := auditTopologyForNodeTx(ctx, tx, nodeID)
		if err != nil {
			return Node{}, "", err
		}
		resultingNodes[nodeID], resultingTopology[nodeID] = resulting, topology
	}
	resultingSubtree := make(map[int64]Node, len(snapshot.subtreeIDs))
	for _, nodeID := range snapshot.subtreeIDs {
		resultingSubtree[nodeID] = resultingNodes[nodeID]
	}
	values, err := makeAuditedMutationValues(
		s.vaultID, snapshot.authority.lineageID, snapshot.operationID, snapshot.recordedAt,
	)
	if err != nil {
		return Node{}, "", err
	}
	topology, err := makeAuditedTrashTopologyDelta(
		values.operationID, snapshot.priorNodes, resultingTopology,
	)
	if err != nil {
		return Node{}, "", err
	}
	effects, err := makeAuditedTrashPathEffects(snapshot)
	if err != nil {
		return Node{}, "", err
	}
	if err := persistAuditedTopologyMutation(
		ctx, tx, values, snapshot.authority, snapshot.scope, snapshot.nodeSequence,
		topology, effects, snapshot.priorNodes, resultingNodes,
		snapshot.priorSubtree, resultingSubtree,
	); err != nil {
		return Node{}, "", err
	}
	return resultingSubtree[node.ID], snapshot.priorPaths[node.ID], nil
}

func (s *Store) prepareAuditedTrash(
	ctx context.Context, tx *sql.Tx, node Node, ifRev int64,
) (auditedTrashSnapshot, error) {
	var snapshot auditedTrashSnapshot
	if node.ID == s.rootID {
		return snapshot, ErrIsRoot
	}
	if node.TrashedAt != nil {
		return snapshot, fmt.Errorf("node %d already trashed: %w", node.ID, ErrNotFound)
	}
	if ifRev != UnconditionalRev && node.Revision != ifRev {
		return snapshot, fmt.Errorf(
			"node %d at revision %d, expected %d: %w",
			node.ID, node.Revision, ifRev, ErrStaleRevision,
		)
	}
	if node.ParentID == nil {
		return snapshot, ErrIsRoot
	}
	authority, scopes, nodeSequence, err := loadAuditedNodeAuthority(ctx, tx, node.ID)
	if err != nil {
		return snapshot, err
	}
	if len(scopes) != 1 {
		return snapshot, unsupportedAuditedNodeMutation(node.ID)
	}
	scope := scopes[0]
	parentMember, err := nodeInAuditScopeTx(ctx, tx, *node.ParentID, scope.scopeID)
	if err != nil {
		return snapshot, err
	}
	if !parentMember {
		return snapshot, unsupportedAuditedNodeMutation(*node.ParentID)
	}
	subtreeIDs, err := liveSubtreeIDsTx(ctx, tx, node.ID)
	if err != nil {
		return snapshot, err
	}
	priorSubtree := make(map[int64]Node, len(subtreeIDs))
	priorPaths := make(map[int64]string, len(subtreeIDs))
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
		path, err := pathOf(ctx, tx, nodeID)
		if err != nil {
			return snapshot, err
		}
		priorSubtree[nodeID], priorPaths[nodeID] = prior, path
	}
	changedIDs := append(slices.Clone(subtreeIDs), *node.ParentID)
	slices.Sort(changedIDs)
	priorNodes := make(map[int64]Node, len(changedIDs))
	for _, nodeID := range changedIDs {
		prior, err := nodeByIDTx(tx, nodeID)
		if err != nil {
			return snapshot, err
		}
		priorNodes[nodeID] = prior
	}
	operationID, err := newUUIDv4()
	if err != nil {
		return snapshot, err
	}
	snapshot = auditedTrashSnapshot{
		authority: authority, scope: scope, nodeSequence: nodeSequence,
		operationID: operationID, recordedAt: nowRFC3339(),
		priorNodes: priorNodes, priorSubtree: priorSubtree,
		priorPaths: priorPaths, subtreeIDs: subtreeIDs,
	}
	return snapshot, nil
}

func makeAuditedTrashTopologyDelta(
	operationID audit.Value, prior map[int64]Node, resulting map[int64]audit.Record,
) (audit.Record, error) {
	priorRecords := make(map[int64]audit.Record, len(prior))
	for nodeID, priorNode := range prior {
		pre, err := auditTopologyForLiveNode(priorNode)
		if err != nil {
			return audit.Record{}, err
		}
		priorRecords[nodeID] = pre
	}
	return makeAuditedTopologyRecordDelta(operationID, priorRecords, resulting)
}

func makeAuditedTrashPathEffects(snapshot auditedTrashSnapshot) ([]audit.Record, error) {
	scopeID, err := audit.UUID(snapshot.scope.scopeID)
	if err != nil {
		return nil, err
	}
	live, err := audit.Text(auditNodeStateLive)
	if err != nil {
		return nil, err
	}
	trash, err := audit.Text(auditNodeStateTrash)
	if err != nil {
		return nil, err
	}
	effects := make([]audit.Record, 0, len(snapshot.subtreeIDs))
	for _, nodeID := range snapshot.subtreeIDs {
		auditNodeID, err := positiveAuditNodeID(nodeID)
		if err != nil {
			return nil, err
		}
		priorPath := snapshot.priorPaths[nodeID]
		if len(priorPath) == 0 || priorPath[0] != '/' {
			return nil, errors.New("audited trash lacks a canonical prior path")
		}
		trashPath := append([]byte("@trash/known"), []byte(priorPath)...)
		effects = append(effects, audit.Record{Kind: "path_effect", Fields: []audit.Field{
			{Name: auditScopeIDField, Value: scopeID},
			{Name: "member_node_id", Value: audit.Unsigned(auditNodeID)},
			{Name: "old", Value: audit.Nested(audit.Record{Kind: auditPathStateKind, Fields: []audit.Field{
				{Name: auditPathField, Value: audit.Bytes([]byte(priorPath))}, {Name: auditStateField, Value: live},
			}})},
			{Name: "new", Value: audit.Nested(audit.Record{Kind: auditPathStateKind, Fields: []audit.Field{
				{Name: auditPathField, Value: audit.Bytes(trashPath)}, {Name: auditStateField, Value: trash},
			}})},
		}})
	}
	return effects, nil
}
