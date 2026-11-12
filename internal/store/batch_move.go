package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

type plannedBatchMove struct {
	requestIndex    int
	nodeID          int64
	oldParentID     int64
	newParentID     int64
	newName         string
	destinationPath string
	changed         bool
}

type batchMovePlan struct {
	priorPaths map[int64]string
	finalPaths map[int64]string
	moves      []plannedBatchMove
	changedIDs []int64
	movedIDs   []int64
	parentIDs  []int64
	recordedAt string
}

// BatchMove applies the requested reorganization as one metadata transaction.
// Sources resolve against one pre-state. Exact destination coordinates resolve
// against the planned final tree, which is checked before any row changes.
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
		if err := s.addBatchMoveSubtreePaths(ctx, tx, &plan); err != nil {
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
	root, err := nodeByIDTx(ctx, tx, s.rootID)
	if err != nil {
		return batchMovePlan{}, err
	}
	preState := map[string]Node{"/": root}
	plan := batchMovePlan{
		priorPaths: make(map[int64]string, len(requests)),
		finalPaths: make(map[int64]string, len(requests)), recordedAt: nowRFC3339(),
	}
	sources := make(map[int64]Node, len(requests))
	destinations := make(map[string]Node, len(requests))
	var byID []int64
	for index, request := range requests {
		source, priorPath, err := resolveBatchMoveSource(ctx, tx, request, preState)
		if err != nil {
			return batchMovePlan{}, fmt.Errorf("batch move item %d: %w", index, err)
		}
		if source.ID == s.rootID {
			return batchMovePlan{}, fmt.Errorf("batch move item %d: %w", index, ErrIsRoot)
		}
		if _, ok := sources[source.ID]; ok {
			return batchMovePlan{}, fmt.Errorf(
				"batch move item %d repeats node %d: %w", index, source.ID, ErrInvalidBatchMove,
			)
		}
		destinationPath, err := canonicalBatchMoveDestination(request.DestinationPath)
		if err != nil {
			return batchMovePlan{}, fmt.Errorf("batch move item %d: %w", index, err)
		}
		sources[source.ID] = source
		plan.priorPaths[source.ID] = priorPath
		plan.finalPaths[source.ID] = destinationPath
		if priorPath == "" {
			byID = append(byID, source.ID)
		}
		plan.moves = append(plan.moves, plannedBatchMove{
			requestIndex: index, nodeID: source.ID, oldParentID: *source.ParentID,
			destinationPath: destinationPath,
		})
	}
	paths, err := s.pathsOf(ctx, tx, byID)
	if err != nil {
		return batchMovePlan{}, err
	}
	for index, id := range byID {
		plan.priorPaths[id] = paths[index]
	}
	for _, move := range plan.moves {
		if prior, ok := destinations[move.destinationPath]; ok {
			return batchMovePlan{}, fmt.Errorf(
				"batch move nodes %d and %d collide at %q: %w",
				prior.ID, move.nodeID, move.destinationPath, ErrExists,
			)
		}
		destinations[move.destinationPath] = sources[move.nodeID]
	}
	finalState := map[string]Node{"/": root}
	var parentError error
	for index := range plan.moves {
		move := &plan.moves[index]
		segments := splitPath(move.destinationPath)
		parentPath := "/" + strings.Join(segments[:len(segments)-1], "/")
		parent, err := resolveBatchMovePath(ctx, tx, parentPath, finalState, destinations, sources)
		if err == nil && parent.Kind != nodeKindDir {
			err = ErrNotDir
		}
		if err != nil {
			if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrNotDir) {
				return batchMovePlan{}, err
			}
			if parentError == nil {
				parentError = fmt.Errorf("batch move item %d destination parent %q: %w",
					move.requestIndex, parentPath, err)
			}
			continue
		}
		move.newParentID, move.newName = parent.ID, segments[len(segments)-1]
		move.changed = move.oldParentID != move.newParentID || sources[move.nodeID].Name != move.newName
		if !move.changed {
			// The source already occupies this coordinate, so it cannot collide.
			continue
		}
		existing, err := childByName(ctx, tx, parent.ID, move.newName)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return batchMovePlan{}, err
		}
		if _, moving := sources[existing.ID]; err == nil && !moving {
			return batchMovePlan{}, fmt.Errorf("batch move nodes %d and %d collide at %q: %w",
				existing.ID, move.nodeID, move.destinationPath, ErrExists)
		}
	}
	if parentError != nil {
		return batchMovePlan{}, parentError
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

// resolveBatchMovePath resolves final coordinates through requested destinations
// or live children. Every requested destination has its parent checked separately.
func resolveBatchMovePath(
	ctx context.Context, tx *sql.Tx, path string, memo, destinations map[string]Node, sources map[int64]Node,
) (Node, error) {
	if node, ok := memo[path]; ok {
		return node, nil
	}
	if node, ok := destinations[path]; ok {
		memo[path] = node
		return node, nil
	}
	separator := strings.LastIndex(path, "/")
	parentPath := path[:separator]
	if parentPath == "" {
		parentPath = "/"
	}
	parent, err := resolveBatchMovePath(ctx, tx, parentPath, memo, destinations, sources)
	if err != nil {
		return Node{}, err
	}
	if parent.Kind != nodeKindDir {
		return Node{}, ErrNotFound
	}
	node, err := childByName(ctx, tx, parent.ID, path[separator+1:])
	if err != nil {
		return Node{}, err
	}
	if _, moving := sources[node.ID]; moving {
		return Node{}, ErrNotFound
	}
	memo[path] = node
	return node, nil
}

func resolveBatchMoveSource(
	ctx context.Context, tx *sql.Tx, request BatchMoveRequest, memo map[string]Node,
) (Node, string, error) {
	byPath, byID := request.SourcePath != "", request.NodeID != 0
	if byPath == byID {
		return Node{}, "", fmt.Errorf("source must use exactly one of path or node ID: %w", ErrInvalidBatchMove)
	}
	if byPath {
		if !strings.HasPrefix(request.SourcePath, "/") {
			return Node{}, "", fmt.Errorf("source path must be absolute: %w", ErrInvalidBatchMove)
		}
		canonical, err := canonicalBatchMovePath(request.SourcePath)
		if err != nil {
			return Node{}, "", err
		}
		node, err := resolveBatchMovePath(ctx, tx, canonical, memo, nil, nil)
		if err != nil {
			return Node{}, "", fmt.Errorf("source path %q: %w", request.SourcePath, err)
		}
		if request.IfRevision != 0 {
			return Node{}, "", fmt.Errorf("path source cannot carry a revision: %w", ErrInvalidBatchMove)
		}
		return node, canonical, nil
	}
	if request.NodeID < 1 || request.IfRevision < 1 {
		return Node{}, "", fmt.Errorf("node source requires positive ID and revision: %w", ErrInvalidBatchMove)
	}
	node, err := nodeByIDTx(ctx, tx, request.NodeID)
	if err != nil {
		return Node{}, "", err
	}
	if node.TrashedAt != nil {
		return Node{}, "", fmt.Errorf("node %d: %w", node.ID, ErrNotFound)
	}
	if node.Revision != request.IfRevision {
		return Node{}, "", fmt.Errorf("node %d at revision %d, expected %d: %w",
			node.ID, node.Revision, request.IfRevision, ErrStaleRevision)
	}
	return node, "", nil
}

func canonicalBatchMoveDestination(destination string) (string, error) {
	if !strings.HasPrefix(destination, "/") {
		return "", fmt.Errorf("destination path must be absolute: %w", ErrInvalidBatchMove)
	}
	canonical, err := canonicalBatchMovePath(destination)
	if err != nil {
		return "", fmt.Errorf("destination %q: %w", destination, err)
	}
	if canonical == "/" {
		return "", fmt.Errorf("destination %q: %w", destination, ErrIsRoot)
	}
	return canonical, nil
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

func (s *Store) addBatchMoveSubtreePaths(ctx context.Context, tx *sql.Tx, plan *batchMovePlan) error {
	ids := make(map[int64]bool)
	for _, id := range plan.movedIDs {
		subtree, err := liveSubtreeIDsTx(ctx, tx, id)
		if err != nil {
			return err
		}
		for _, child := range subtree {
			ids[child] = true
		}
	}
	subtreeIDs := sortedBatchMoveIDs(ids)
	paths, err := s.pathsOf(ctx, tx, subtreeIDs)
	if err != nil {
		return err
	}
	destinations := make(map[string]string, len(plan.moves))
	for _, move := range plan.moves {
		destinations[plan.priorPaths[move.nodeID]] = move.destinationPath
	}
	for index, id := range subtreeIDs {
		priorPath := paths[index]
		for ancestor := priorPath; ancestor != ""; ancestor = ancestor[:strings.LastIndex(ancestor, "/")] {
			if destination, ok := destinations[ancestor]; ok {
				plan.priorPaths[id] = priorPath
				plan.finalPaths[id] = destination + strings.TrimPrefix(priorPath, ancestor)
				break
			}
		}
	}
	return nil
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
		if err := bumpRevisionTx(ctx, tx, id, plan.recordedAt); err != nil {
			return err
		}
	}
	for id, priorPath := range plan.priorPaths {
		if priorPath != plan.finalPaths[id] {
			if err := invalidateMovedNoteLimitTx(ctx, tx, id); err != nil {
				return err
			}
		}
	}
	return recordVaultChangesTx(ctx, tx, "moved", sortedBatchMoveIDs(moved)...)
}

func (s *Store) batchMoveResults(
	ctx context.Context, tx *sql.Tx, plan batchMovePlan,
) ([]BatchMoveResult, error) {
	results := make([]BatchMoveResult, len(plan.moves))
	for _, move := range plan.moves {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		node, err := nodeByIDTx(ctx, tx, move.nodeID)
		if err != nil {
			return nil, err
		}
		// The validated plan supplies canonical paths for this same transaction.
		results[move.requestIndex] = BatchMoveResult{
			Node: node, FromPath: plan.priorPaths[move.nodeID], Path: plan.finalPaths[move.nodeID],
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
