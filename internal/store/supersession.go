package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/docbank/document"
)

// ErrInvalidSupersession marks invalid declarations and graph limits.
var ErrInvalidSupersession = errors.New("invalid supersession")

const maxSupersessionNote = 1024

// Supersession records an owner-declared replacement.
type Supersession struct {
	SuccessorID, PredecessorID int64
	Note, RecordedAt           string
}

// SupersessionChange describes a declaration mutation and the affected node.
type SupersessionChange struct {
	Node    Node
	Path    string
	Changed bool
}

// NodeValidityView is a live node and its direct, live relationships.
type NodeValidityView struct {
	AsOf                     string
	Node                     Node
	Path                     string
	Window                   document.EffectiveWindow
	Predecessors, Successors []Supersession
	State                    document.ValidityState
	Undated                  bool
}

func validSupersessionNote(note string) error {
	if len(note) > maxSupersessionNote || validateUTF8Field("supersession note", note) != nil {
		return fmt.Errorf("invalid supersession note: %w", ErrInvalidSupersession)
	}
	return nil
}
func liveFileTx(tx *sql.Tx, id int64) (Node, error) {
	n, err := nodeByIDTx(tx, id)
	if err != nil {
		return Node{}, err
	}
	if n.TrashedAt != nil {
		return Node{}, fmt.Errorf("node %d: %w", id, ErrNotFound)
	}
	if n.IsDir() {
		return Node{}, fmt.Errorf("node %d: %w", id, ErrNotFile)
	}
	return n, nil
}

// PutSupersession declares or updates a replacement.
func (s *Store) PutSupersession(ctx context.Context, successorID, predecessorID, revision int64, note string) (SupersessionChange, error) {
	return s.changeSupersession(ctx, successorID, predecessorID, revision, note, true)
}

// DeleteSupersession removes a replacement declaration.
func (s *Store) DeleteSupersession(ctx context.Context, successorID, predecessorID, revision int64) (SupersessionChange, error) {
	return s.changeSupersession(ctx, successorID, predecessorID, revision, "", false)
}
func (s *Store) changeSupersession(ctx context.Context, successorID, predecessorID, revision int64, note string, put bool) (SupersessionChange, error) {
	var result SupersessionChange
	if successorID == predecessorID {
		return result, fmt.Errorf("self link: %w", ErrInvalidSupersession)
	}
	if err := validSupersessionNote(note); err != nil {
		return result, err
	}
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		n, err := liveFileTx(tx, predecessorID)
		if err != nil {
			return err
		}
		if revision != UnconditionalRev && n.Revision != revision {
			return ErrStaleRevision
		}
		if put {
			if _, err = liveFileTx(tx, successorID); err != nil {
				return err
			}
		} else {
			successor, e := nodeByIDTx(tx, successorID)
			if e != nil {
				return e
			}
			if successor.IsDir() {
				return ErrNotFile
			}
		}
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT note FROM node_supersessions WHERE successor_node_id=? AND predecessor_node_id=?`, successorID, predecessorID).Scan(&existing)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		present := err == nil
		changed := put && (!present || existing != note) || !put && present
		if changed {
			if put {
				_, err = tx.ExecContext(ctx, `INSERT INTO node_supersessions(successor_node_id,predecessor_node_id,note,recorded_at) VALUES(?,?,?,?) ON CONFLICT(successor_node_id,predecessor_node_id) DO UPDATE SET note=excluded.note,recorded_at=excluded.recorded_at`, successorID, predecessorID, note, nowRFC3339())
			} else {
				_, err = tx.ExecContext(ctx, `DELETE FROM node_supersessions WHERE successor_node_id=? AND predecessor_node_id=?`, successorID, predecessorID)
			}
			if err != nil {
				return err
			}
		}
		if put && !present {
			if err = validateSupersessionComponent(ctx, tx, predecessorID, nil); err != nil {
				return err
			}
		}
		if changed {
			if err = bumpRevisionTx(tx, n.ID, nowRFC3339()); err != nil {
				return err
			}
			n, err = nodeByIDTx(tx, n.ID)
			if err != nil {
				return err
			}
		}
		result = SupersessionChange{Node: n, Changed: changed}
		result.Path, err = pathOf(ctx, tx, n.ID)
		return err
	})
	if err != nil {
		return SupersessionChange{}, err
	}
	return result, nil
}

// PutEffectiveWindow sets a node's inclusive effective dates.
func (s *Store) PutEffectiveWindow(ctx context.Context, id, revision int64, from, to string) (SupersessionChange, error) {
	return s.changeWindow(ctx, id, revision, document.EffectiveWindow{From: from, To: to}, true)
}

// DeleteEffectiveWindow clears a node's effective dates.
func (s *Store) DeleteEffectiveWindow(ctx context.Context, id, revision int64) (SupersessionChange, error) {
	return s.changeWindow(ctx, id, revision, document.EffectiveWindow{}, false)
}
func (s *Store) changeWindow(ctx context.Context, id, revision int64, w document.EffectiveWindow, put bool) (SupersessionChange, error) {
	var result SupersessionChange
	if put && w.From == "" && w.To == "" {
		return result, fmt.Errorf("empty effective window: %w", ErrInvalidSupersession)
	}
	if err := document.ValidateEffectiveWindow(w); err != nil {
		return result, fmt.Errorf("%w: %w", ErrInvalidSupersession, err)
	}
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		n, err := liveFileTx(tx, id)
		if err != nil {
			return err
		}
		if revision != UnconditionalRev && n.Revision != revision {
			return ErrStaleRevision
		}
		var from, to sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT effective_from,effective_to FROM node_effective_windows WHERE node_id=?`, id).Scan(&from, &to)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		present := err == nil
		changed := put && (!present || from.String != w.From || to.String != w.To) || !put && present
		if changed {
			if put {
				_, err = tx.ExecContext(ctx, `INSERT INTO node_effective_windows(node_id,effective_from,effective_to) VALUES(?,NULLIF(?,''),NULLIF(?,'')) ON CONFLICT(node_id) DO UPDATE SET effective_from=excluded.effective_from,effective_to=excluded.effective_to`, id, w.From, w.To)
			} else {
				_, err = tx.ExecContext(ctx, `DELETE FROM node_effective_windows WHERE node_id=?`, id)
			}
			if err != nil {
				return err
			}
			if err = bumpRevisionTx(tx, id, nowRFC3339()); err != nil {
				return err
			}
			n, err = nodeByIDTx(tx, id)
			if err != nil {
				return err
			}
		}
		result = SupersessionChange{Node: n, Changed: changed}
		result.Path, err = pathOf(ctx, tx, n.ID)
		return err
	})
	if err != nil {
		return SupersessionChange{}, err
	}
	return result, nil
}

// validateSupersessionComponent walks both directions, including trashed nodes.
func validateSupersessionComponent(ctx context.Context, q metadataQuerier, start int64, visited map[int64]bool) error {
	seen := map[int64]bool{start: true}
	queue := []int64{start}
	edges := map[[2]int64]bool{}
	next := map[int64][]int64{}
	indegree := map[int64]int{}
	outdegree := map[int64]int{}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		err := func() error {
			rows, err := q.QueryContext(ctx, `SELECT predecessor_node_id,successor_node_id FROM node_supersessions WHERE predecessor_node_id=? OR successor_node_id=? ORDER BY predecessor_node_id,successor_node_id`, id, id)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var a, b int64
				if err = rows.Scan(&a, &b); err != nil {
					return err
				}
				edge := [2]int64{a, b}
				if edges[edge] {
					continue
				}
				edges[edge] = true
				outdegree[a]++
				indegree[b]++
				if outdegree[a] > 16 || indegree[b] > 16 {
					return fmt.Errorf("supersession degree: %w", ErrInvalidSupersession)
				}
				next[a] = append(next[a], b)
				for _, n := range []int64{a, b} {
					if !seen[n] {
						seen[n] = true
						queue = append(queue, n)
						if len(seen) > 4096 {
							return fmt.Errorf("supersession node budget: %w", ErrInvalidSupersession)
						}
					}
				}
			}
			if err = rows.Err(); err != nil {
				return err
			}
			return nil
		}()
		if err != nil {
			return err
		}
	}
	for id := range seen {
		if visited != nil {
			visited[id] = true
		}
	}
	queue = queue[:0]
	for n := range seen {
		if indegree[n] == 0 {
			queue = append(queue, n)
		}
	}
	count := 0
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		count++
		for _, v := range next[n] {
			indegree[v]--
			if indegree[v] == 0 {
				queue = append(queue, v)
			}
		}
	}
	if count != len(seen) {
		return fmt.Errorf("supersession cycle: %w", ErrInvalidSupersession)
	}
	return nil
}

// NodeValidity evaluates a live file against its direct live successors.
func (s *Store) NodeValidity(ctx context.Context, id int64, asOf string) (NodeValidityView, error) {
	var v NodeValidityView
	if asOf == "" {
		asOf = time.Now().Format("2006-01-02")
	}
	if err := document.ValidateDate(asOf); err != nil {
		return v, fmt.Errorf("%w: %w", ErrInvalidSupersession, err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return v, err
	}
	defer func() { _ = tx.Rollback() }()
	v.AsOf = asOf
	v.Node, err = liveFileTx(tx, id)
	if err != nil {
		return NodeValidityView{}, err
	}
	v.Path, err = pathOf(ctx, tx, id)
	if err != nil {
		return NodeValidityView{}, err
	}
	var from, to sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT effective_from,effective_to FROM node_effective_windows WHERE node_id=?`, id).Scan(&from, &to)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return NodeValidityView{}, err
	}
	v.Window = document.EffectiveWindow{From: from.String, To: to.String}
	rows, err := tx.QueryContext(ctx, `SELECT l.successor_node_id,l.predecessor_node_id,l.note,l.recorded_at,COALESCE(w.effective_from,'') FROM node_supersessions l JOIN nodes n ON n.id=CASE WHEN l.predecessor_node_id=? THEN l.successor_node_id ELSE l.predecessor_node_id END LEFT JOIN node_effective_windows w ON w.node_id=l.successor_node_id WHERE (l.predecessor_node_id=? OR l.successor_node_id=?) AND n.kind='file' AND n.trashed_at IS NULL ORDER BY l.successor_node_id,l.predecessor_node_id`, id, id, id)
	if err != nil {
		return NodeValidityView{}, err
	}
	defer func() { _ = rows.Close() }()
	var starts []document.SuccessorStart
	for rows.Next() {
		var link Supersession
		var start string
		if err = rows.Scan(&link.SuccessorID, &link.PredecessorID, &link.Note, &link.RecordedAt, &start); err != nil {
			return NodeValidityView{}, err
		}
		if link.PredecessorID == id {
			v.Successors = append(v.Successors, link)
			starts = append(starts, document.SuccessorStart{From: start})
		} else {
			v.Predecessors = append(v.Predecessors, link)
		}
	}
	if err = rows.Err(); err != nil {
		return NodeValidityView{}, err
	}
	v.State, v.Undated = document.EvaluateValidity(asOf, v.Window, starts)
	return v, nil
}
