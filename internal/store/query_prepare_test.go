package store

import (
	"context"
	"errors"
	"slices"
	"testing"

	"go.kenn.io/docbank/internal/query"
)

func TestCompiledBoundaryRangesSurviveComposition(t *testing.T) {
	t.Parallel()
	path := compilePathPredicate("/inbox")
	coverage, err := compileTextCoveragePredicate("complete", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	nested := joinCompiledFragments([]compiledQueryFragment{path, negateCompiledFragment(coverage)}, ` OR `)
	population := selectCompiledPopulation(nested, true)
	saved := markCompiledBoundary(wrapCompiledFragment(population, `EXISTS (SELECT 1 FROM (`, `) saved WHERE saved.node_id=n.id AND saved.content_version_id=cv.version_id)`), 0)
	combined := joinCompiledFragments([]compiledQueryFragment{{sql: `n.name=?`, args: []any{"example"}}, saved, compileCollectionPredicate("collection")}, ` AND `)
	if len(combined.boundaries) != 4 {
		t.Fatalf("got %d boundaries", len(combined.boundaries))
	}
	for _, b := range combined.boundaries {
		if b.sqlStart < 0 || b.sqlEnd > len(combined.sql) || b.sqlStart >= b.sqlEnd || b.argStart < 0 || b.argEnd > len(combined.args) || b.argStart > b.argEnd {
			t.Fatalf("invalid boundary: %+v", b)
		}
	}
	pathBoundary, coverageBoundary, savedBoundary, collectionBoundary := combined.boundaries[0], combined.boundaries[1], combined.boundaries[2], combined.boundaries[3]
	if combined.sql[pathBoundary.sqlStart:pathBoundary.sqlEnd] != path.sql || !slices.Equal(combined.args[pathBoundary.argStart:pathBoundary.argEnd], path.args) {
		t.Fatal("path range changed")
	}
	if combined.sql[coverageBoundary.sqlStart:coverageBoundary.sqlEnd] != coverage.sql || !slices.Equal(combined.args[coverageBoundary.argStart:coverageBoundary.argEnd], coverage.args) || coverageBoundary.relations != compiledRelationProcessingCoverage {
		t.Fatal("coverage range changed")
	}
	if !(savedBoundary.sqlStart < pathBoundary.sqlStart && savedBoundary.sqlEnd > coverageBoundary.sqlEnd && savedBoundary.argStart <= pathBoundary.argStart && savedBoundary.argEnd >= coverageBoundary.argEnd) {
		t.Fatal("saved boundary lost its nested domain")
	}
	if combined.sql[collectionBoundary.sqlStart:collectionBoundary.sqlEnd] != compileCollectionPredicate("collection").sql {
		t.Fatal("collection range changed")
	}
}

// This exercises SQL membership under OR and NOT, including a zero-argument
// boundary followed by another boundary whose argument range must move.
func TestPreparedPredicateDuplicateAndPath(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	first, err := s.CreateFile(ctx, s.RootID(), "first.txt", fakeHash("prepared-duplicate"), 10, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateFile(ctx, s.RootID(), "second.txt", first.BlobHash, first.Size, first.MimeType)
	if err != nil {
		t.Fatal(err)
	}
	unique, err := s.CreateFile(ctx, s.RootID(), "unique.txt", fakeHash("prepared-unique"), 11, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	compiled := compileFixtureQuery(t, s, `NOT has_duplicates:true OR path:/missing`, query.Filters{})
	population, err := prepareCompiledPopulation(ctx, s.db, compiled, "", nil, CoverageSelection{})
	if err != nil {
		t.Fatal(err)
	}
	statement, args, err := bindQueryPopulation(population, CoverageSelection{}, "")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		var version string
		if err := rows.Scan(&id, &version); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []int64{unique.ID}) {
		t.Fatalf("got %v, want only %d (duplicates %d, %d)", ids, unique.ID, first.ID, second.ID)
	}
}

func TestPreparedDocumentPopulationCancellation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	compiled := compileFixtureQuery(t, s, `path:/missing`, query.Filters{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := materializeDocumentPopulation(ctx, s.db, compiled, "", nil, CoverageSelection{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want canceled", err)
	}
}
