package store

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/docbank/internal/audit"
)

type auditedBatchMoveSnapshot struct {
	authority       auditAuthorityState
	scope           auditScopeState
	nodeSequence    int64
	operationID     string
	recordedAt      string
	priorNodes      map[int64]Node
	priorEventNodes map[int64]Node
	eventNodeIDs    []int64
}

func (s *Store) prepareAuditedBatchMove(
	ctx context.Context, tx *sql.Tx, plan *batchMovePlan,
) (*auditedBatchMoveSnapshot, error) {
	authority, scopes, nodeSequence, err := loadAuditedNodeAuthority(ctx, tx, plan.movedIDs[0])
	if err != nil {
		return nil, err
	}
	if len(scopes) != 1 {
		return nil, unsupportedAuditedNodeMutation(plan.movedIDs[0])
	}
	members, err := auditScopeMemberSetTx(ctx, tx, scopes[0].scopeID)
	if err != nil {
		return nil, err
	}
	for _, nodeID := range plan.changedIDs {
		auditNodeID, err := positiveAuditNodeID(nodeID)
		if err != nil {
			return nil, err
		}
		if !members[auditNodeID] {
			return nil, unsupportedAuditedNodeMutation(nodeID)
		}
	}
	topology, err := currentAuditTopology(ctx, tx)
	if err != nil {
		return nil, err
	}
	for _, nodeID := range plan.movedIDs {
		auditNodeID, err := positiveAuditNodeID(nodeID)
		if err != nil {
			return nil, err
		}
		affectsTrashOrigin, err := auditMoveAffectsTrashOrigin(topology, members, auditNodeID)
		if err != nil {
			return nil, err
		}
		if affectsTrashOrigin {
			return nil, fmt.Errorf(
				"moving node %d would change retained trash-origin paths: %w",
				nodeID, ErrAuditMutationUnsupported,
			)
		}
	}
	operationID, err := newUUIDv4()
	if err != nil {
		return nil, err
	}
	snapshot := &auditedBatchMoveSnapshot{
		authority: authority, scope: scopes[0], nodeSequence: nodeSequence,
		operationID: operationID, recordedAt: plan.recordedAt,
		priorNodes: make(map[int64]Node, len(plan.changedIDs)),
	}
	for _, nodeID := range plan.changedIDs {
		node, err := nodeByIDTx(tx, nodeID)
		if err != nil {
			return nil, err
		}
		snapshot.priorNodes[nodeID] = node
	}
	snapshot.eventNodeIDs, err = batchMoveChangedPathIDs(*plan, members)
	if err != nil {
		return nil, err
	}
	if len(snapshot.eventNodeIDs) == 0 {
		return nil, fmt.Errorf("audited batch move changes no protected path: %w", ErrInvalidBatchMove)
	}
	snapshot.priorEventNodes = make(map[int64]Node, len(snapshot.eventNodeIDs))
	for _, nodeID := range snapshot.eventNodeIDs {
		node, err := nodeByIDTx(tx, nodeID)
		if err != nil {
			return nil, err
		}
		snapshot.priorEventNodes[nodeID] = node
	}
	return snapshot, nil
}

func (s *Store) persistAuditedBatchMove(
	ctx context.Context, tx *sql.Tx, snapshot auditedBatchMoveSnapshot, plan batchMovePlan,
) error {
	resultingNodes := make(map[int64]Node, len(snapshot.priorNodes))
	for nodeID := range snapshot.priorNodes {
		node, err := nodeByIDTx(tx, nodeID)
		if err != nil {
			return err
		}
		resultingNodes[nodeID] = node
	}
	resultingEventNodes := make(map[int64]Node, len(snapshot.eventNodeIDs))
	for _, nodeID := range snapshot.eventNodeIDs {
		node, err := nodeByIDTx(tx, nodeID)
		if err != nil {
			return err
		}
		resultingEventNodes[nodeID] = node
	}
	values, err := makeAuditedMutationValues(
		s.vaultID, snapshot.authority.lineageID, snapshot.operationID, snapshot.recordedAt,
	)
	if err != nil {
		return err
	}
	topology, err := makeAuditedMoveTopologyDelta(values.operationID, snapshot.priorNodes, resultingNodes)
	if err != nil {
		return err
	}
	effects, err := makeAuditedBatchMovePathEffects(
		snapshot.scope.scopeID, snapshot.eventNodeIDs, plan.priorPaths, plan.finalPaths,
	)
	if err != nil {
		return err
	}
	return persistAuditedTopologyMutation(
		ctx, tx, values, snapshot.authority, snapshot.scope, snapshot.nodeSequence,
		topology, effects, snapshot.priorNodes, resultingNodes,
		snapshot.priorEventNodes, resultingEventNodes,
	)
}

func makeAuditedBatchMovePathEffects(
	scopeID string, nodeIDs []int64, priorPaths, resultingPaths map[int64]string,
) ([]audit.Record, error) {
	scopeValue, err := audit.UUID(scopeID)
	if err != nil {
		return nil, err
	}
	live, err := audit.Text(auditNodeStateLive)
	if err != nil {
		return nil, err
	}
	effects := make([]audit.Record, 0, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		auditNodeID, err := positiveAuditNodeID(nodeID)
		if err != nil {
			return nil, err
		}
		priorPath, resultingPath := priorPaths[nodeID], resultingPaths[nodeID]
		if priorPath == "" || resultingPath == "" || priorPath == resultingPath {
			return nil, fmt.Errorf("audited batch move has invalid path effect for node %d", nodeID)
		}
		effects = append(effects, audit.Record{Kind: "path_effect", Fields: []audit.Field{
			{Name: auditScopeIDField, Value: scopeValue},
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
