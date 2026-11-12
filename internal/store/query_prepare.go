package store

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"
)

// documentSnapshotPopulation hands person-edge populations to row, facet and
// annotation SQL as exact identities. A supported 64-person/11-role query
// failed pure-Go SQLite row materialization ("VIEWs and/or subqueries nested
// too deep") when its predicate stayed inline. Person-edge-free queries stay
// inline. The engine guard's exact site and threshold are unmeasured.
func documentSnapshotPopulation(ctx context.Context, q metadataQuerier, compiled CompiledQuery, generation string, coverage CoverageSelection) (compiledQueryFragment, error) {
	if !compiled.predicate.personEdges {
		return matchedPopulation(compiled, generation, nil)
	}
	return materializeDocumentPopulation(ctx, q, compiled, generation, coverage)
}

// materializeDocumentPopulation executes the selection once and hands
// its exact identities to downstream projections on the same read snapshot.
func materializeDocumentPopulation(ctx context.Context, q metadataQuerier, compiled CompiledQuery, generation string, coverage CoverageSelection) (compiledQueryFragment, error) {
	population, err := preparedPopulation(ctx, q, compiled, generation, nil, coverage)
	if err != nil {
		return compiledQueryFragment{}, err
	}
	statement, args, err := bindQueryPopulation(population, coverage, generation)
	if err != nil {
		return compiledQueryFragment{}, err
	}
	encoded, err := readPopulationIdentities(ctx, q, statement, args)
	if err != nil {
		return compiledQueryFragment{}, fmt.Errorf("materializing document membership: %w", err)
	}
	return compiledQueryFragment{sql: `SELECT json_extract(value,'$.node_id') AS node_id,json_extract(value,'$.content_version_id') AS content_version_id FROM json_each(?)`, args: []any{encoded}}, nil
}

// preparedPopulation is matchedPopulation with a person-edge predicate's
// CTE-bearing leaves prepared on the caller's read snapshot first.
func preparedPopulation(ctx context.Context, q metadataQuerier, compiled CompiledQuery, generation string, profile *string, coverage CoverageSelection) (compiledQueryFragment, error) {
	predicate, err := compiled.bind(generation, profile)
	if err != nil {
		return compiledQueryFragment{}, err
	}
	predicate, err = prepareCompiledLeaves(ctx, q, predicate, generation, coverage)
	if err != nil {
		return compiledQueryFragment{}, err
	}
	return selectCompiledPopulation(predicate, compiled.Query.Filters.CollapseDuplicates), nil
}

// prepareCompiledLeaves replaces each path, collection, text_coverage and
// has_duplicates occurrence of a bound person-edge predicate with its exact
// live current node/version set. Pure-Go SQLite rejected a supported saved
// role query when a leaf's CTE followed its person subqueries. Each leaf is
// its own SQL over all live files, so AND/OR/NOT, duplicate false and saved
// collapse keep their meaning. Admission already happened in bind; the
// prepared fragment is not bound again and may carry more arguments.
func prepareCompiledLeaves(ctx context.Context, q metadataQuerier, predicate compiledQueryFragment, generation string, coverage CoverageSelection) (compiledQueryFragment, error) {
	if !predicate.personEdges || len(predicate.leaves) == 0 {
		return predicate, nil
	}
	payloads := make([]string, len(predicate.leaves))
	for i, leaf := range predicate.leaves {
		population := selectCompiledPopulation(compiledQueryFragment{
			sql:       predicate.sql[leaf.sqlStart:leaf.sqlEnd],
			args:      predicate.args[leaf.argStart:leaf.argEnd],
			relations: leaf.relations,
		}, false)
		statement, args, err := bindQueryPopulation(population, coverage, generation)
		if err != nil {
			return compiledQueryFragment{}, err
		}
		if payloads[i], err = readPopulationIdentities(ctx, q, statement, args); err != nil {
			return compiledQueryFragment{}, fmt.Errorf("preparing query leaf: %w", err)
		}
	}
	const membership = `(n.id,cv.version_id) IN (SELECT json_extract(value,'$.node_id'),json_extract(value,'$.content_version_id') FROM json_each(?))`
	var rebuilt strings.Builder
	args := make([]any, 0, len(predicate.args)+len(payloads))
	sqlAt, argAt := 0, 0
	for i, leaf := range predicate.leaves {
		rebuilt.WriteString(predicate.sql[sqlAt:leaf.sqlStart])
		rebuilt.WriteString(membership)
		args = append(args, predicate.args[argAt:leaf.argStart]...)
		args = append(args, payloads[i])
		sqlAt, argAt = leaf.sqlEnd, leaf.argEnd
	}
	rebuilt.WriteString(predicate.sql[sqlAt:])
	args = append(args, predicate.args[argAt:]...)
	// Every shared relation reference in document predicates is a recorded
	// coverage or duplicate leaf, all of which were just replaced.
	predicate.sql, predicate.args, predicate.relations, predicate.leaves = rebuilt.String(), args, 0, nil
	return predicate, nil
}

// readPopulationIdentities executes a node/current-version selection and
// encodes its rows as a non-null JSON array of exact identities.
func readPopulationIdentities(ctx context.Context, q metadataQuerier, statement string, args []any) (string, error) {
	type identity struct {
		NodeID    int64  `json:"node_id"`
		VersionID string `json:"content_version_id"`
	}
	members := make([]identity, 0)
	err := func() (err error) {
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
		return "", err
	}
	encoded, err := json.Marshal(members)
	members = nil
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return string(encoded), nil
}
