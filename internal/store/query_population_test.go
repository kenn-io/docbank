package store

import (
	"cmp"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/query"
)

type queryPopulationMember struct {
	NodeID           int64
	ContentVersionID string
}

type queryPopulationFixture struct {
	store *Store
	nodes map[string]Node
	tags  map[string]Tag
}

func newQueryPopulationFixture(t *testing.T) queryPopulationFixture {
	t.Helper()
	s := newTestStore(t)
	ctx := t.Context()
	shared := fakeHash("population-shared")
	files := []struct {
		key, name, hash string
	}{
		{"alpha-beta", "alpha beta.txt", shared},
		{"alpha-copy", "alpha copy.txt", shared},
		{"global-copy", "global copy.txt", shared},
		{"gamma", "gamma.txt", fakeHash("population-gamma")},
		{"delta", "delta.pdf", fakeHash("population-delta")},
		{"mercury", "mercury.txt", fakeHash("population-mercury")},
		{"venus", "venus.txt", fakeHash("population-venus")},
		{"archive-alpha", "archive alpha.txt", fakeHash("population-archive")},
		{"tagged", "tagged.txt", fakeHash("population-tagged")},
		{"replaced", "replaced.txt", fakeHash("population-old")},
	}
	nodes := make(map[string]Node, len(files))
	for _, file := range files {
		node, err := s.CreateFile(ctx, s.RootID(), file.name, file.hash, 20, "text/plain")
		require.NoError(t, err)
		nodes[file.key] = node
	}
	for key, modified := range map[string]string{
		"global-copy": "2026-01-01T00:00:00.000000000Z",
		"alpha-beta":  "2026-01-02T00:00:00.000000000Z",
		"alpha-copy":  "2026-01-03T00:00:00.000000000Z",
	} {
		_, err := s.db.ExecContext(ctx, `UPDATE nodes SET modified_at=? WHERE id=?`, modified, nodes[key].ID)
		require.NoError(t, err)
	}
	seedCompiledLegacyText(t, s, nodes["mercury"], "cobalt orbit")
	seedCompiledLegacyText(t, s, nodes["venus"], "cobalt transit")
	seedCompiledLegacyText(t, s, nodes["replaced"], "obsolete signal")
	replaced, _, err := s.ReplaceContent(ctx, nodes["replaced"].ID, nodes["replaced"].Revision,
		fakeHash("population-new"), 21, "text/plain")
	require.NoError(t, err)
	nodes["replaced"] = replaced

	outer, err := s.CreateTag(ctx, "outer")
	require.NoError(t, err)
	keep, err := s.CreateTag(ctx, "keep")
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, outer.ID, nodes["alpha-beta"].ID, nodes["alpha-beta"].Revision)
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, keep.ID, nodes["alpha-copy"].ID, nodes["alpha-copy"].Revision)
	require.NoError(t, err)

	_, err = s.CreateSavedQuery(ctx, "Collapsed alpha", "", SavedQueryKindQuery,
		[]byte(`{"syntax":"advanced","text":"name:alpha","filters":{"has_duplicates":true,"collapse_duplicates":true}}`))
	require.NoError(t, err)
	_, err = s.CreateSavedQuery(ctx, "Named alpha", "", SavedQueryKindQuery,
		[]byte(`{"syntax":"advanced","text":"name:\"alpha beta\""}`))
	require.NoError(t, err)
	return queryPopulationFixture{s, nodes, map[string]Tag{"outer": outer, "keep": keep}}
}

func (fixture queryPopulationFixture) MatchedPopulation(
	t *testing.T, value query.Query, selection CoverageSelection, omittedFacet string,
) []queryPopulationMember {
	t.Helper()
	var members []queryPopulationMember
	err := fixture.store.withLexicalGenerationRead(t.Context(), func(q metadataQuerier, generation LexicalGeneration) error {
		compiled, err := CompileQuery(t.Context(), value, queryResolver{q: q})
		if err != nil {
			return err
		}
		population, err := matchedPopulation(compiled, generation.ID, omittedFacet)
		if err != nil {
			return err
		}
		statement, args, err := bindQueryPopulation(population, selection, generation.ID)
		if err != nil {
			return err
		}
		rows, err := q.QueryContext(t.Context(), statement, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var nodeID int64
			var versionID string
			if err := rows.Scan(&nodeID, &versionID); err != nil {
				return err
			}
			members = append(members, queryPopulationMember{nodeID, versionID})
		}
		return rows.Err()
	})
	require.NoError(t, err)
	slices.SortFunc(members, func(a, b queryPopulationMember) int {
		return cmp.Compare(a.NodeID, b.NodeID)
	})
	return members
}

func queryPopulationQuery(t *testing.T, text string, filters query.Filters) query.Query {
	t.Helper()
	value := compilerQuery(t, text)
	value.Filters = filters
	return value
}

func queryPopulationNodeIDs(members []queryPopulationMember) []int64 {
	ids := make([]int64, len(members))
	for i, member := range members {
		ids[i] = member.NodeID
	}
	return ids
}

// Removing field scope, changing OR/NOT precedence, or treating NEAR as a
// document-level intersection changes these hand-selected members.
func TestQueryPopulationPreservesAdvancedASTScope(t *testing.T) {
	fixture := newQueryPopulationFixture(t)
	members := fixture.MatchedPopulation(t, queryPopulationQuery(t,
		`name:(alpha OR mercury) AND NOT name:archive`, query.Filters{}), CoverageSelection{}, "")
	require.ElementsMatch(t, []int64{
		fixture.nodes["alpha-beta"].ID,
		fixture.nodes["alpha-copy"].ID,
		fixture.nodes["mercury"].ID,
	}, queryPopulationNodeIDs(members))

	members = fixture.MatchedPopulation(t, queryPopulationQuery(t,
		`name:(alpha NEAR/0 beta)`, query.Filters{}), CoverageSelection{}, "")
	require.Equal(t, []int64{fixture.nodes["alpha-beta"].ID}, queryPopulationNodeIDs(members))

	members = fixture.MatchedPopulation(t, queryPopulationQuery(t,
		`cobalt`, query.Filters{}), CoverageSelection{}, "")
	require.Len(t, members, 2)
	require.ElementsMatch(t, []int64{
		fixture.nodes["mercury"].ID, fixture.nodes["venus"].ID,
	}, queryPopulationNodeIDs(members))

	members = fixture.MatchedPopulation(t, queryPopulationQuery(t,
		`cobalt AND NOT name:venus`, query.Filters{}), CoverageSelection{}, "")
	require.Equal(t, []int64{fixture.nodes["mercury"].ID}, queryPopulationNodeIDs(members))

	members = fixture.MatchedPopulation(t, queryPopulationQuery(t,
		`obsolete`, query.Filters{}), CoverageSelection{}, "")
	require.Empty(t, members, "replaced content must not match its historical version")
	members = fixture.MatchedPopulation(t, queryPopulationQuery(t,
		`name:replaced`, query.Filters{}), CoverageSelection{}, "")
	require.Equal(t, []queryPopulationMember{{
		NodeID: fixture.nodes["replaced"].ID, ContentVersionID: fixture.nodes["replaced"].CurrentVersionID,
	}}, members)
}

// Choosing the global duplicate representative would drop every match here;
// the oldest node inside the matched subset is alpha-beta, not global-copy.
func TestQueryPopulationDuplicatesUseMatchedSubset(t *testing.T) {
	fixture := newQueryPopulationFixture(t)
	members := fixture.MatchedPopulation(t, queryPopulationQuery(t,
		`name:alpha`, query.Filters{HasDuplicates: true, CollapseDuplicates: true}), CoverageSelection{}, "")
	require.Len(t, members, 1)
	require.Equal(t, []int64{fixture.nodes["alpha-beta"].ID}, queryPopulationNodeIDs(members))

	members = fixture.MatchedPopulation(t, queryPopulationQuery(t,
		`has_duplicates:true AND name:alpha`, query.Filters{}), CoverageSelection{}, "")
	require.ElementsMatch(t, []int64{
		fixture.nodes["alpha-beta"].ID, fixture.nodes["alpha-copy"].ID,
	}, queryPopulationNodeIDs(members))
}

// A saved operand collapses only its own population. A sibling OR operand can
// still contribute the saved population's non-representative member.
func TestQueryPopulationSavedCollapseStaysInsideOperand(t *testing.T) {
	fixture := newQueryPopulationFixture(t)
	members := fixture.MatchedPopulation(t, queryPopulationQuery(t,
		`saved:"Collapsed alpha"`, query.Filters{}), CoverageSelection{}, "")
	require.Equal(t, []int64{fixture.nodes["alpha-beta"].ID}, queryPopulationNodeIDs(members))

	members = fixture.MatchedPopulation(t, queryPopulationQuery(t,
		`saved:"Collapsed alpha" OR tag:keep`, query.Filters{}), CoverageSelection{}, "")
	require.ElementsMatch(t, []int64{
		fixture.nodes["alpha-beta"].ID, fixture.nodes["alpha-copy"].ID,
	}, queryPopulationNodeIDs(members))
}

func TestQueryPopulationSavedCollapseUsesOnlyLiveCurrentVersions(t *testing.T) {
	t.Run("replacement", func(t *testing.T) {
		fixture := newQueryPopulationFixture(t)
		original, err := fixture.store.NodeByID(t.Context(), fixture.nodes["alpha-beta"].ID)
		require.NoError(t, err)
		replaced, _, err := fixture.store.ReplaceContent(t.Context(), original.ID, original.Revision,
			fakeHash("population-alpha-replacement"), 20, "text/plain")
		require.NoError(t, err)
		fixture.nodes["alpha-beta"] = replaced
		_, err = fixture.store.db.ExecContext(t.Context(), `UPDATE nodes SET modified_at=? WHERE id=?`,
			"2025-12-31T00:00:00.000000000Z", replaced.ID)
		require.NoError(t, err)

		members := fixture.MatchedPopulation(t, queryPopulationQuery(t,
			`saved:"Collapsed alpha"`, query.Filters{}), CoverageSelection{}, "")
		require.Equal(t, []int64{fixture.nodes["alpha-copy"].ID}, queryPopulationNodeIDs(members))
	})

	t.Run("trash", func(t *testing.T) {
		fixture := newQueryPopulationFixture(t)
		first, err := fixture.store.NodeByID(t.Context(), fixture.nodes["alpha-beta"].ID)
		require.NoError(t, err)
		_, _, err = fixture.store.Trash(t.Context(), first.ID, first.Revision)
		require.NoError(t, err)
		_, err = fixture.store.db.ExecContext(t.Context(), `UPDATE nodes SET modified_at=? WHERE id=?`,
			"2025-12-31T00:00:00.000000000Z", first.ID)
		require.NoError(t, err)

		members := fixture.MatchedPopulation(t, queryPopulationQuery(t,
			`saved:"Collapsed alpha"`, query.Filters{}), CoverageSelection{}, "")
		require.Equal(t, []int64{fixture.nodes["alpha-copy"].ID}, queryPopulationNodeIDs(members))
	})
}

func TestQueryPopulationOmittedFacetPreservesASTAndSavedConstraints(t *testing.T) {
	fixture := newQueryPopulationFixture(t)
	value := queryPopulationQuery(t, `tag:keep OR saved:"Named alpha"`, query.Filters{
		TagIDs: []string{fixture.tags["outer"].ID},
	})
	members := fixture.MatchedPopulation(t, value, CoverageSelection{}, "")
	require.Equal(t, []int64{fixture.nodes["alpha-beta"].ID}, queryPopulationNodeIDs(members))

	omitted := fixture.MatchedPopulation(t, value, CoverageSelection{}, "tags")
	require.ElementsMatch(t, []int64{
		fixture.nodes["alpha-beta"].ID, fixture.nodes["alpha-copy"].ID,
	}, queryPopulationNodeIDs(omitted))
}

func TestQueryPopulationCoverageUsesConfiguredProfile(t *testing.T) {
	s, _, nodes, profile := collectionCoverageFixture(t, 2)
	collectionCoveragePublish(t, s, nodes[0], profile, "complete")
	collectionCoveragePublish(t, s, nodes[1], profile, "partial")
	fixture := queryPopulationFixture{store: s}
	selection := CoverageSelection{Configuration: "configured", ProfileFingerprint: profile.Fingerprint}

	members := fixture.MatchedPopulation(t, queryPopulationQuery(t, "", query.Filters{
		TextCoverage: []string{"complete"},
	}), selection, "")
	require.Equal(t, []int64{nodes[0].ID}, queryPopulationNodeIDs(members))
	members = fixture.MatchedPopulation(t, queryPopulationQuery(t,
		`text_coverage:partial`, query.Filters{}), selection, "")
	require.Equal(t, []int64{nodes[1].ID}, queryPopulationNodeIDs(members))
	omitted := fixture.MatchedPopulation(t, queryPopulationQuery(t, "", query.Filters{
		TextCoverage: []string{"complete"},
	}), CoverageSelection{}, "text_coverage")
	require.ElementsMatch(t, []int64{nodes[0].ID, nodes[1].ID}, queryPopulationNodeIDs(omitted))

	err := s.withLexicalGenerationRead(t.Context(), func(q metadataQuerier, generation LexicalGeneration) error {
		compiled, err := CompileQuery(t.Context(), queryPopulationQuery(t,
			`text_coverage:complete`, query.Filters{}), queryResolver{q: q})
		require.NoError(t, err)
		population, err := matchedPopulation(compiled, generation.ID, "")
		require.NoError(t, err)
		_, _, err = bindQueryPopulation(population, CoverageSelection{}, generation.ID)
		require.ErrorIs(t, err, ErrInvalidCoverageSelection)
		return nil
	})
	require.NoError(t, err)
}
