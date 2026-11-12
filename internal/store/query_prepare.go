package store

import (
	"context"
	"encoding/json/v2"
	"fmt"
)

// documentSnapshotPopulation hands person-edge populations to row, facet and
// annotation SQL as exact identities. A supported 64-person/11-role query
// failed pure-Go SQLite row materialization ("VIEWs and/or subqueries nested
// too deep") when its predicate stayed inline. Person-edge-free queries stay
// inline. The engine guard's exact site and threshold are unmeasured.
func documentSnapshotPopulation(ctx context.Context, q metadataQuerier, compiled CompiledQuery, generation string, profile *string, coverage CoverageSelection) (compiledQueryFragment, error) {
	if !compiled.predicate.personEdges {
		return matchedPopulation(compiled, generation, profile)
	}
	return materializeDocumentPopulation(ctx, q, compiled, generation, profile, coverage)
}

// materializeDocumentPopulation executes the selection once and hands
// its exact identities to downstream projections on the same read snapshot.
func materializeDocumentPopulation(ctx context.Context, q metadataQuerier, compiled CompiledQuery, generation string, profile *string, coverage CoverageSelection) (compiledQueryFragment, error) {
	population, err := matchedPopulation(compiled, generation, profile)
	if err != nil {
		return compiledQueryFragment{}, err
	}
	statement, args, err := bindQueryPopulation(population, coverage, generation)
	if err != nil {
		return compiledQueryFragment{}, err
	}
	type identity struct {
		NodeID    int64  `json:"node_id"`
		VersionID string `json:"content_version_id"`
	}
	members := make([]identity, 0)
	err = func() (err error) {
		rows, err := q.QueryContext(ctx, statement, args...)
		if err != nil {
			return err
		}
		defer func() {
			if closeErr := rows.Close(); err == nil {
				err = closeErr
			}
		}()
		for rows.Next() {
			var member identity
			if err := rows.Scan(&member.NodeID, &member.VersionID); err != nil {
				return err
			}
			members = append(members, member)
		}
		return rows.Err()
	}()
	if err != nil {
		return compiledQueryFragment{}, fmt.Errorf("materializing document membership: %w", err)
	}
	encoded, err := json.Marshal(members)
	members = nil
	if err != nil {
		return compiledQueryFragment{}, err
	}
	if err := ctx.Err(); err != nil {
		return compiledQueryFragment{}, err
	}
	return compiledQueryFragment{sql: `SELECT json_extract(value,'$.node_id') AS node_id,json_extract(value,'$.content_version_id') AS content_version_id FROM json_each(?)`, args: []any{string(encoded)}}, nil
}
