package store

import (
	"context"
	"encoding/json/v2"
	"fmt"
)

// documentSnapshotPopulation retains the prepared handoff only for queries
// whose document predicates use person edges.
func documentSnapshotPopulation(ctx context.Context, q metadataQuerier, compiled CompiledQuery, generation string, profile *string, coverage CoverageSelection) (compiledQueryFragment, error) {
	if !compiled.predicate.personEdges {
		return matchedPopulation(compiled, generation, profile)
	}
	return materializeDocumentPopulation(ctx, q, compiled, generation, profile, coverage)
}

// documentReportPopulation uses the same selection without the snapshot handoff.
func documentReportPopulation(ctx context.Context, q metadataQuerier, compiled CompiledQuery, generation string, profile *string, coverage CoverageSelection) (compiledQueryFragment, error) {
	if !compiled.predicate.personEdges {
		return matchedPopulation(compiled, generation, profile)
	}
	return prepareCompiledPopulation(ctx, q, compiled, generation, profile, coverage)
}

// prepareCompiledPredicate binds and validates the original predicate before
// replacing compiler-identified boundaries. q must be the caller's pinned read
// snapshot; the returned JSON arguments belong to this preparation only.
func prepareCompiledPredicate(ctx context.Context, q metadataQuerier, compiled CompiledQuery, generation string, profile *string, coverage CoverageSelection) (compiledQueryFragment, error) {
	predicate, err := compiled.bind(generation, profile)
	if err != nil {
		return compiledQueryFragment{}, err
	}
	return prepareBoundPredicate(ctx, q, predicate, generation, coverage)
}

// prepareCompiledPopulation preserves collapse after the predicate has been
// lowered. Use the same q, generation, profile and disclosure-scoped compiled
// query for every subsequent consumer of the returned fragment.
func prepareCompiledPopulation(ctx context.Context, q metadataQuerier, compiled CompiledQuery, generation string, profile *string, coverage CoverageSelection) (compiledQueryFragment, error) {
	predicate, err := prepareCompiledPredicate(ctx, q, compiled, generation, profile, coverage)
	if err != nil {
		return compiledQueryFragment{}, err
	}
	return selectCompiledPopulation(predicate, compiled.Query.Filters.CollapseDuplicates), nil
}

// prepareBoundPredicate accepts only a fragment returned by CompiledQuery.bind,
// which checks the original SQL and argument limits. Boundaries are evaluated
// inside out: a saved collapse must rank its own matching members, not siblings
// selected by the surrounding query.
func prepareBoundPredicate(ctx context.Context, q metadataQuerier, predicate compiledQueryFragment, generation string, coverage CoverageSelection) (compiledQueryFragment, error) {
	predicate.args = append([]any(nil), predicate.args...)
	predicate.boundaries = append([]compiledBoundary(nil), predicate.boundaries...)
	for len(predicate.boundaries) > 0 {
		if err := ctx.Err(); err != nil {
			return compiledQueryFragment{}, err
		}
		// Compiler boundaries are disjoint or nested. The shortest is innermost.
		index := 0
		for i := 1; i < len(predicate.boundaries); i++ {
			if predicate.boundaries[i].sqlEnd-predicate.boundaries[i].sqlStart < predicate.boundaries[index].sqlEnd-predicate.boundaries[index].sqlStart {
				index = i
			}
		}
		b := predicate.boundaries[index]
		leaf := compiledQueryFragment{sql: `SELECT n.id,cv.version_id FROM nodes n JOIN content_versions cv ON cv.node_id=n.id WHERE ` + compiledLiveCurrentPredicate().sql + ` AND (` + predicate.sql[b.sqlStart:b.sqlEnd] + `)`, args: predicate.args[b.argStart:b.argEnd], relations: b.relations}
		statement, args, err := bindQueryPopulation(leaf, coverage, generation)
		if err != nil {
			return compiledQueryFragment{}, err
		}
		type identity struct {
			NodeID    int64  `json:"node_id"`
			VersionID string `json:"content_version_id"`
		}
		var members []identity
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
			return compiledQueryFragment{}, fmt.Errorf("preparing query membership: %w", err)
		}
		// A nil slice would encode as null; an empty JSON array has no members.
		if members == nil {
			members = []identity{}
		}
		encoded, err := json.Marshal(members)
		if err != nil {
			return compiledQueryFragment{}, err
		}
		members = nil
		if err := ctx.Err(); err != nil {
			return compiledQueryFragment{}, err
		}
		replacement := `(n.id,cv.version_id) IN (SELECT json_extract(prepared_member.value,'$.node_id'),json_extract(prepared_member.value,'$.content_version_id') FROM json_each(?) prepared_member)`
		predicate.sql = predicate.sql[:b.sqlStart] + replacement + predicate.sql[b.sqlEnd:]
		newArgs := make([]any, 0, len(predicate.args)-(b.argEnd-b.argStart)+1)
		newArgs = append(newArgs, predicate.args[:b.argStart]...)
		newArgs = append(newArgs, string(encoded))
		newArgs = append(newArgs, predicate.args[b.argEnd:]...)
		predicate.args = newArgs
		sqlDelta, argDelta := len(replacement)-(b.sqlEnd-b.sqlStart), 1-(b.argEnd-b.argStart)
		predicate.boundaries = append(predicate.boundaries[:index], predicate.boundaries[index+1:]...)
		for i := range predicate.boundaries {
			other := &predicate.boundaries[i]
			if other.sqlStart >= b.sqlEnd {
				other.sqlStart += sqlDelta
				other.sqlEnd += sqlDelta
				other.argStart += argDelta
				other.argEnd += argDelta
			} else if other.sqlStart <= b.sqlStart && other.sqlEnd >= b.sqlEnd {
				other.sqlEnd += sqlDelta
				other.argEnd += argDelta
			}
		}
	}
	// All shared relation references in document predicates belong to boundaries.
	predicate.relations = 0
	return predicate, nil
}

// materializeDocumentPopulation executes the prepared selection once and hands
// its exact identities to downstream projections on the same read snapshot.
func materializeDocumentPopulation(ctx context.Context, q metadataQuerier, compiled CompiledQuery, generation string, profile *string, coverage CoverageSelection) (compiledQueryFragment, error) {
	population, err := prepareCompiledPopulation(ctx, q, compiled, generation, profile, coverage)
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
