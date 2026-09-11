package store

import (
	"fmt"
	"strings"
)

// matchedPopulation renders the exact current node/version membership selected
// by a compiled query. Required shared relations remain explicit on the
// returned fragment and must be bound in the caller's read snapshot.
func matchedPopulation(
	compiled CompiledQuery, generationID, omittedFacet string,
) (compiledQueryFragment, error) {
	predicate, err := compiled.bind(generationID, omittedFacet)
	if err != nil {
		return compiledQueryFragment{}, err
	}
	return selectCompiledPopulation(predicate, compiled.Query.Filters.CollapseDuplicates), nil
}

func selectCompiledPopulation(predicate compiledQueryFragment, collapseDuplicates bool) compiledQueryFragment {
	predicate = joinCompiledFragments([]compiledQueryFragment{
		compiledLiveCurrentPredicate(), predicate,
	}, ` AND `)
	matched := `SELECT n.id AS node_id,cv.version_id AS content_version_id,
		cv.blob_hash,n.modified_at
		FROM nodes n JOIN content_versions cv ON cv.node_id=n.id
		WHERE ` + predicate.sql
	if !collapseDuplicates {
		return compiledQueryFragment{
			sql:  `SELECT DISTINCT node_id,content_version_id FROM (` + matched + `) matched_members`,
			args: predicate.args, relations: predicate.relations,
		}
	}
	return compiledQueryFragment{
		sql: `SELECT node_id,content_version_id FROM (
			SELECT node_id,content_version_id,
				ROW_NUMBER() OVER (PARTITION BY blob_hash ORDER BY ` + DuplicateRepresentativeOrder + `) duplicate_rank
			FROM (` + matched + `) matched_members
		) ranked_members WHERE duplicate_rank=1`,
		args: predicate.args, relations: predicate.relations,
	}
}

// bindQueryPopulation supplies every shared relation requested by a population
// fragment. Processing coverage has a distinct configured-profile binding;
// generation arguments inside compiled predicates are already bound by
// matchedPopulation.
func bindQueryPopulation(
	population compiledQueryFragment, selection CoverageSelection, generationID string,
) (string, []any, error) {
	var ctes []string
	args := make([]any, 0, len(population.args)+2)
	if population.relations&(compiledRelationCurrentContent|compiledRelationProcessingCoverage) != 0 {
		ctes = append(ctes, CurrentContentMembershipCTE)
	}
	if population.relations&compiledRelationProcessingCoverage != 0 {
		normalized, err := normalizeCoverageSelection([]CoverageSelection{selection})
		if err != nil {
			return "", nil, err
		}
		if normalized.Configuration != "configured" {
			return "", nil, fmt.Errorf("%w: text coverage requires a configured profile", ErrInvalidCoverageSelection)
		}
		ctes = append(ctes,
			`coverage_members AS (SELECT node_id FROM current_content_members)`,
			processingCoverageCTE(),
		)
		args = append(args, normalized.ProfileFingerprint, generationID)
	}
	args = append(args, population.args...)
	if len(ctes) == 0 {
		return population.sql, args, nil
	}
	return `WITH ` + strings.Join(ctes, ",") + ` ` + population.sql, args, nil
}
