package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"

	"go.kenn.io/docbank/document"
)

// SearchValidityNode holds declarations relevant to one search result.
type SearchValidityNode struct {
	Window     document.EffectiveWindow
	Declared   bool
	Successors []SearchValiditySuccessor
}

type SearchValiditySuccessor struct {
	NodeID              int64
	Path, EffectiveFrom string
}

// SearchResultValidity reads only candidates and their direct live successors
// within the caller's scope. Writes already validate the declarations.
func (s *Store) SearchResultValidity(ctx context.Context, scope SearchOptions, nodeIDs []int64) (map[int64]SearchValidityNode, error) {
	result := make(map[int64]SearchValidityNode, len(nodeIDs))
	if len(nodeIDs) == 0 {
		return result, nil
	}
	encoded, err := json.Marshal(nodeIDs)
	if err != nil {
		return nil, err
	}
	scopeSQL, args := liveSearchScopeSQLForBackend(scope, s.postgresSchema != "", 1)
	// Inline scope probes so an all-live request reads candidates and direct
	// successors rather than materializing every live file.
	query := `WITH requested AS (` + s.jsonArrayRowsSQL() + `), eligible AS NOT MATERIALIZED (SELECT n.id ` + scopeSQL + `)
 SELECT n.id,w.node_id,w.effective_from,w.effective_to,edge.successor_node_id,sw.effective_from
 FROM nodes n JOIN (SELECT DISTINCT CAST(value AS BIGINT) AS id FROM requested) r ON n.id=r.id
 JOIN eligible e ON e.id=n.id
 LEFT JOIN node_effective_windows w ON w.node_id=n.id
 LEFT JOIN node_supersessions edge ON edge.predecessor_node_id=n.id AND EXISTS (SELECT 1 FROM eligible successor WHERE successor.id=edge.successor_node_id)
 LEFT JOIN node_effective_windows sw ON sw.node_id=edge.successor_node_id
 ORDER BY n.id,edge.successor_node_id`
	args = append([]any{string(encoded)}, args...)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var successorIDs []int64
	for rows.Next() {
		var id int64
		var windowID, next sql.NullInt64
		var from, to, start sql.NullString
		if err := rows.Scan(&id, &windowID, &from, &to, &next, &start); err != nil {
			return nil, err
		}
		n := result[id]
		n.Window = document.EffectiveWindow{From: from.String, To: to.String}
		n.Declared = windowID.Valid || next.Valid
		if next.Valid {
			n.Successors = append(n.Successors, SearchValiditySuccessor{NodeID: next.Int64, EffectiveFrom: start.String})
			successorIDs = append(successorIDs, next.Int64)
		}
		result[id] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	paths, err := s.pathsOf(ctx, tx, successorIDs)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]string, len(paths))
	for i, id := range successorIDs {
		byID[id] = paths[i]
	}
	for id, n := range result {
		for i := range n.Successors {
			n.Successors[i].Path = byID[n.Successors[i].NodeID]
		}
		result[id] = n
	}
	return result, tx.Commit()
}
