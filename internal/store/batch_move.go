package store

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// MaxBatchMoves bounds one atomic reorganization and its response.
const MaxBatchMoves = 1000

// BatchMoveRequest identifies a live source either by SourcePath or by the
// stable NodeID plus IfRevision, and gives its absolute final destination.
type BatchMoveRequest struct {
	SourcePath      string
	NodeID          int64
	IfRevision      int64
	DestinationPath string
}

// BatchMoveResult is the transactionally captured receipt for one request.
type BatchMoveResult struct {
	Node     Node
	FromPath string
	Path     string
}

type batchMoveNode struct {
	id       int64
	parentID *int64
	name     string
	kind     string
	revision int64
}

type plannedBatchMove struct {
	requestIndex int
	nodeID       int64
	oldParentID  int64
	newParentID  int64
	newName      string
	changed      bool
}

type batchMovePlan struct {
	nodes      map[int64]batchMoveNode
	priorPaths map[int64]string
	finalPaths map[int64]string
	moves      []plannedBatchMove
	changedIDs []int64
	movedIDs   []int64
	parentIDs  []int64
	recordedAt string
}

// BatchMove applies the requested reorganization as one metadata transaction.
// Every selector and destination is resolved against the same pre-state; only
// the final topology is checked for cycles and sibling collisions.
func (s *Store) BatchMove(ctx context.Context, requests []BatchMoveRequest) ([]BatchMoveResult, error) {
	if len(requests) == 0 || len(requests) > MaxBatchMoves {
		return nil, fmt.Errorf("batch move requires 1-%d items: %w", MaxBatchMoves, ErrInvalidBatchMove)
	}
	var results []BatchMoveResult
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		plan, err := s.planBatchMove(ctx, tx, requests)
		if err != nil {
			return err
		}
		if len(plan.movedIDs) == 0 {
			results, err = s.batchMoveResults(ctx, tx, plan)
			return err
		}
		active, err := auditAuthorityActiveTx(ctx, tx)
		if err != nil {
			return err
		}
		var audited *auditedBatchMoveSnapshot
		if active {
			audited, err = s.prepareAuditedBatchMove(ctx, tx, &plan)
			if err != nil {
				return err
			}
			plan.recordedAt = audited.recordedAt
		}
		if err := s.applyBatchMove(ctx, tx, plan); err != nil {
			return err
		}
		if audited != nil {
			if err := s.persistAuditedBatchMove(ctx, tx, *audited, plan); err != nil {
				return err
			}
		}
		results, err = s.batchMoveResults(ctx, tx, plan)
		return err
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

func (s *Store) planBatchMove(
	ctx context.Context, tx *sql.Tx, requests []BatchMoveRequest,
) (batchMovePlan, error) {
	nodes, err := loadBatchMoveTopology(ctx, tx)
	if err != nil {
		return batchMovePlan{}, err
	}
	priorPaths, err := batchMovePaths(nodes)
	if err != nil {
		return batchMovePlan{}, err
	}
	pathIDs := make(map[string]int64, len(priorPaths))
	for id, path := range priorPaths {
		pathIDs[path] = id
	}
	initialNodes := make(map[int64]batchMoveNode, len(nodes))
	maps.Copy(initialNodes, nodes)
	plan := batchMovePlan{nodes: nodes, priorPaths: priorPaths, recordedAt: nowRFC3339()}
	seen := make(map[int64]bool, len(requests))
	for index, request := range requests {
		source, err := resolveBatchMoveSource(request, initialNodes, pathIDs)
		if err != nil {
			return batchMovePlan{}, fmt.Errorf("batch move item %d: %w", index, err)
		}
		if source.id == s.rootID {
			return batchMovePlan{}, fmt.Errorf("batch move item %d: %w", index, ErrIsRoot)
		}
		if seen[source.id] {
			return batchMovePlan{}, fmt.Errorf(
				"batch move item %d repeats node %d: %w", index, source.id, ErrInvalidBatchMove,
			)
		}
		seen[source.id] = true
		newParentID, newName, err := resolveBatchMoveDestination(
			request.DestinationPath, initialNodes, pathIDs,
		)
		if err != nil {
			return batchMovePlan{}, fmt.Errorf("batch move item %d: %w", index, err)
		}
		move := plannedBatchMove{
			requestIndex: index, nodeID: source.id, oldParentID: *source.parentID,
			newParentID: newParentID, newName: newName,
		}
		move.changed = move.oldParentID != move.newParentID || source.name != move.newName
		plan.moves = append(plan.moves, move)
		if move.changed {
			updated := nodes[source.id]
			updated.parentID, updated.name = new(move.newParentID), move.newName
			nodes[source.id] = updated
		}
	}
	if err := validateBatchMoveTopology(nodes); err != nil {
		return batchMovePlan{}, err
	}
	plan.finalPaths, err = batchMovePaths(nodes)
	if err != nil {
		return batchMovePlan{}, err
	}
	changed, moved, parents := map[int64]bool{}, map[int64]bool{}, map[int64]bool{}
	for _, move := range plan.moves {
		if !move.changed {
			continue
		}
		changed[move.nodeID], moved[move.nodeID] = true, true
		changed[move.oldParentID], parents[move.oldParentID] = true, true
		changed[move.newParentID], parents[move.newParentID] = true, true
	}
	plan.changedIDs, plan.movedIDs, plan.parentIDs = sortedBatchMoveIDs(changed),
		sortedBatchMoveIDs(moved), sortedBatchMoveIDs(parents)
	return plan, nil
}

func loadBatchMoveTopology(ctx context.Context, tx *sql.Tx) (map[int64]batchMoveNode, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id,parent_id,name,kind,revision FROM nodes WHERE trashed_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("loading batch move topology: %w", err)
	}
	defer func() { _ = rows.Close() }()
	nodes := make(map[int64]batchMoveNode)
	for rows.Next() {
		var node batchMoveNode
		var parent sql.NullInt64
		if err := rows.Scan(&node.id, &parent, &node.name, &node.kind, &node.revision); err != nil {
			return nil, fmt.Errorf("scanning batch move topology: %w", err)
		}
		if parent.Valid {
			node.parentID = new(parent.Int64)
		}
		nodes[node.id] = node
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("loading batch move topology: %w", err)
	}
	return nodes, nil
}

func resolveBatchMoveSource(
	request BatchMoveRequest, nodes map[int64]batchMoveNode, pathIDs map[string]int64,
) (batchMoveNode, error) {
	byPath, byID := request.SourcePath != "", request.NodeID != 0
	if byPath == byID {
		return batchMoveNode{}, fmt.Errorf("source must use exactly one of path or node ID: %w", ErrInvalidBatchMove)
	}
	var id int64
	if byPath {
		if !strings.HasPrefix(request.SourcePath, "/") {
			return batchMoveNode{}, fmt.Errorf("source path must be absolute: %w", ErrInvalidBatchMove)
		}
		canonical, err := canonicalBatchMovePath(request.SourcePath)
		if err != nil {
			return batchMoveNode{}, err
		}
		id = pathIDs[canonical]
		if id == 0 {
			return batchMoveNode{}, fmt.Errorf("source path %q: %w", request.SourcePath, ErrNotFound)
		}
		if request.IfRevision != 0 {
			return batchMoveNode{}, fmt.Errorf("path source cannot carry a revision: %w", ErrInvalidBatchMove)
		}
	} else {
		if request.NodeID < 1 || request.IfRevision < 1 {
			return batchMoveNode{}, fmt.Errorf("node source requires positive ID and revision: %w", ErrInvalidBatchMove)
		}
		id = request.NodeID
	}
	node, ok := nodes[id]
	if !ok {
		return batchMoveNode{}, fmt.Errorf("node %d: %w", id, ErrNotFound)
	}
	if byID && node.revision != request.IfRevision {
		return batchMoveNode{}, fmt.Errorf(
			"node %d at revision %d, expected %d: %w",
			id, node.revision, request.IfRevision, ErrStaleRevision,
		)
	}
	return node, nil
}

func resolveBatchMoveDestination(
	destination string, nodes map[int64]batchMoveNode, pathIDs map[string]int64,
) (int64, string, error) {
	if !strings.HasPrefix(destination, "/") {
		return 0, "", fmt.Errorf("destination path must be absolute: %w", ErrInvalidBatchMove)
	}
	canonical, err := canonicalBatchMovePath(destination)
	if err != nil {
		return 0, "", fmt.Errorf("destination %q: %w", destination, err)
	}
	segments := splitPath(canonical)
	if id := pathIDs[canonical]; id != 0 {
		destinationNode := nodes[id]
		if destinationNode.parentID == nil {
			return 0, "", ErrIsRoot
		}
		return *destinationNode.parentID, destinationNode.name, nil
	}
	if len(segments) == 0 {
		return 0, "", fmt.Errorf("destination %q: %w", destination, ErrExists)
	}
	parentPath := "/" + strings.Join(segments[:len(segments)-1], "/")
	parentID := pathIDs[parentPath]
	parent, ok := nodes[parentID]
	if !ok {
		return 0, "", fmt.Errorf("destination parent %q: %w", parentPath, ErrNotFound)
	}
	if parent.kind != nodeKindDir {
		return 0, "", fmt.Errorf("destination parent %q: %w", parentPath, ErrNotDir)
	}
	return parentID, segments[len(segments)-1], nil
}

func canonicalBatchMovePath(value string) (string, error) {
	segments := splitPath(value)
	for index, segment := range segments {
		normalized, err := NormalizeName(segment)
		if err != nil {
			return "", err
		}
		segments[index] = normalized
	}
	return "/" + strings.Join(segments, "/"), nil
}

func validateBatchMoveTopology(nodes map[int64]batchMoveNode) error {
	siblings := make(map[string]int64, len(nodes))
	for id, node := range nodes {
		if node.parentID == nil {
			continue
		}
		parent, ok := nodes[*node.parentID]
		if !ok {
			return fmt.Errorf("batch move parent %d: %w", *node.parentID, ErrNotFound)
		}
		if parent.kind != nodeKindDir {
			return fmt.Errorf("batch move parent %d: %w", *node.parentID, ErrNotDir)
		}
		key := fmt.Sprintf("%d\x00%s", *node.parentID, node.name)
		if prior := siblings[key]; prior != 0 && prior != id {
			return fmt.Errorf("batch move nodes %d and %d collide at %q: %w", prior, id, node.name, ErrExists)
		}
		siblings[key] = id
	}
	states := make(map[int64]uint8, len(nodes))
	var visit func(int64) error
	visit = func(id int64) error {
		switch states[id] {
		case 1:
			return fmt.Errorf("batch move at node %d: %w", id, ErrCycle)
		case 2:
			return nil
		}
		states[id] = 1
		if parent := nodes[id].parentID; parent != nil {
			if err := visit(*parent); err != nil {
				return err
			}
		}
		states[id] = 2
		return nil
	}
	for id := range nodes {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func batchMovePaths(nodes map[int64]batchMoveNode) (map[int64]string, error) {
	paths := make(map[int64]string, len(nodes))
	var pathFor func(int64, map[int64]bool) (string, error)
	pathFor = func(id int64, visiting map[int64]bool) (string, error) {
		if path, ok := paths[id]; ok {
			return path, nil
		}
		node, ok := nodes[id]
		if !ok {
			return "", fmt.Errorf("batch move path node %d: %w", id, ErrNotFound)
		}
		if visiting[id] {
			return "", ErrCycle
		}
		visiting[id] = true
		defer delete(visiting, id)
		if node.parentID == nil {
			paths[id] = "/"
			return "/", nil
		}
		parent, err := pathFor(*node.parentID, visiting)
		if err != nil {
			return "", err
		}
		paths[id] = strings.TrimSuffix(parent, "/") + "/" + node.name
		return paths[id], nil
	}
	for id := range nodes {
		if _, err := pathFor(id, make(map[int64]bool)); err != nil {
			return nil, err
		}
	}
	return paths, nil
}

func (s *Store) applyBatchMove(ctx context.Context, tx *sql.Tx, plan batchMovePlan) error {
	for _, id := range plan.movedIDs {
		temporaryName := fmt.Sprintf("\x00docbank-batch-%d", id)
		if _, err := tx.ExecContext(ctx,
			`UPDATE nodes SET name=? WHERE id=?`, temporaryName, id); err != nil {
			return fmt.Errorf("staging batch move node %d: %w", id, err)
		}
	}
	moved := make(map[int64]bool, len(plan.movedIDs))
	for _, move := range plan.moves {
		if !move.changed {
			continue
		}
		moved[move.nodeID] = true
		if _, err := tx.ExecContext(ctx,
			`UPDATE nodes SET parent_id=?,name=?,revision=revision+1,modified_at=? WHERE id=?`,
			move.newParentID, move.newName, plan.recordedAt, move.nodeID); err != nil {
			return fmt.Errorf("applying batch move node %d: %w", move.nodeID, err)
		}
	}
	for _, id := range plan.parentIDs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if moved[id] {
			continue
		}
		if err := bumpRevisionTx(tx, id, plan.recordedAt); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) batchMoveResults(
	ctx context.Context, tx *sql.Tx, plan batchMovePlan,
) ([]BatchMoveResult, error) {
	results := make([]BatchMoveResult, len(plan.moves))
	for _, move := range plan.moves {
		node, err := nodeByIDTx(tx, move.nodeID)
		if err != nil {
			return nil, err
		}
		path, err := pathOf(ctx, tx, move.nodeID)
		if err != nil {
			return nil, err
		}
		results[move.requestIndex] = BatchMoveResult{
			Node: node, FromPath: plan.priorPaths[move.nodeID], Path: path,
		}
	}
	return results, nil
}

func sortedBatchMoveIDs(set map[int64]bool) []int64 {
	result := make([]int64, 0, len(set))
	for id := range set {
		result = append(result, id)
	}
	slices.Sort(result)
	return result
}

func batchMoveChangedPathIDs(
	plan batchMovePlan, members map[uint64]bool,
) ([]int64, error) {
	var result []int64
	for id, priorPath := range plan.priorPaths {
		auditNodeID, err := positiveAuditNodeID(id)
		if err != nil {
			return nil, err
		}
		if members[auditNodeID] && priorPath != plan.finalPaths[id] {
			result = append(result, id)
		}
	}
	slices.Sort(result)
	return result, nil
}
