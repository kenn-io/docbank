package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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

// The connector supplies only cursor lifecycle behavior. SQL membership is
// exercised by the real-store tests; these rows are exact synthetic identities.
type preparationLifecycleConnector struct{ rows *preparationLifecycleRows }

func (c preparationLifecycleConnector) Connect(context.Context) (driver.Conn, error) {
	return preparationLifecycleConn{c.rows}, nil
}
func (c preparationLifecycleConnector) Driver() driver.Driver { return c }
func (c preparationLifecycleConnector) Open(string) (driver.Conn, error) {
	return preparationLifecycleConn{c.rows}, nil
}

type preparationLifecycleConn struct{ rows *preparationLifecycleRows }

func (preparationLifecycleConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (preparationLifecycleConn) Begin() (driver.Tx, error) {
	return nil, errors.New("unexpected transaction")
}
func (preparationLifecycleConn) Close() error { return nil }
func (c preparationLifecycleConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	c.rows.queries++
	return c.rows, nil
}

type preparationLifecycleRows struct {
	queries, next, closed  int
	cancel                 context.CancelFunc
	iterationErr, closeErr error
}

func (*preparationLifecycleRows) Columns() []string { return []string{"node_id", "content_version_id"} }
func (r *preparationLifecycleRows) Close() error    { r.closed++; return r.closeErr }
func (r *preparationLifecycleRows) Next(values []driver.Value) error {
	r.next++
	if r.next == 1 {
		values[0], values[1] = int64(42), "synthetic-version"
		return nil
	}
	if r.cancel != nil {
		r.cancel()
	}
	if r.iterationErr != nil {
		return r.iterationErr
	}
	return io.EOF
}

func TestPreparedMembershipCursorLifecycle(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	boundary := compileFixtureQuery(t, s, `path:/missing`, query.Filters{})
	final := compileFixtureQuery(t, s, `name:synthetic`, query.Filters{})
	iterationErr := errors.New("synthetic iteration error")
	closeErr := errors.New("synthetic close error")
	for _, owner := range []string{"boundary", "final"} {
		for _, failure := range []string{"cancel", "iteration", "close"} {
			t.Run(owner+"/"+failure, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				r := &preparationLifecycleRows{}
				want := error(context.Canceled)
				switch failure {
				case "cancel":
					r.cancel = cancel
				case "iteration":
					r.iterationErr, want = iterationErr, iterationErr
				case "close":
					r.closeErr, want = closeErr, closeErr
				}
				db := sql.OpenDB(preparationLifecycleConnector{r})
				t.Cleanup(func() { _ = db.Close() })
				var got compiledQueryFragment
				var err error
				if owner == "boundary" {
					got, err = prepareCompiledPredicate(ctx, db, boundary, "", nil, CoverageSelection{})
				} else {
					got, err = materializeDocumentPopulation(ctx, db, final, "", nil, CoverageSelection{})
				}
				if !errors.Is(err, want) {
					t.Fatalf("got error %v, want %v", err, want)
				}
				if got.sql != "" || len(got.args) != 0 {
					t.Fatalf("fabricated membership: %+v", got)
				}
				if r.queries != 1 || r.next != 2 || r.closed != 1 {
					t.Fatalf("cursor counts: queries=%d next=%d close=%d", r.queries, r.next, r.closed)
				}
			})
		}
	}
}

// Recording the statements at the existing querier boundary checks the actual
// consumer route without introducing a second query evaluator.
type populationRecordingQuerier struct {
	metadataQuerier
	statements []string
}

func (q *populationRecordingQuerier) QueryContext(ctx context.Context, statement string, args ...any) (*sql.Rows, error) {
	q.statements = append(q.statements, statement)
	return q.metadataQuerier.QueryContext(ctx, statement, args...)
}

func TestDocumentPopulationPersonEdgeRouting(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, "Route Person", "operator")
	require.NoError(t, err)
	_, err = s.CreatePerson(ctx, "Route Other", "operator")
	require.NoError(t, err)
	_, err = s.CreateSavedQuery(ctx, "Role saved", "", SavedQueryKindQuery, []byte(fmt.Sprintf(`{"filters":{"people":[{"person_id":%q,"roles":["author"]}]}}`, person.PersonID)))
	require.NoError(t, err)
	_, err = s.CreateSavedQuery(ctx, "Role collapse", "", SavedQueryKindQuery, []byte(fmt.Sprintf(`{"filters":{"collapse_duplicates":true,"people":[{"person_id":%q,"roles":["author"]}]}}`, person.PersonID)))
	require.NoError(t, err)
	_, err = s.CreateSavedQuery(ctx, "Plain collapse", "", SavedQueryKindQuery, []byte(`{"syntax":"advanced","text":"path:/missing","filters":{"collapse_duplicates":true}}`))
	require.NoError(t, err)
	collection := createCollectionRun(t, s, "route.txt", "route-population")
	tests := []struct {
		name, text, filters string
		edges               bool
	}{
		{"negative role", `NOT author:"Route Person"`, `{}`, true},
		{"negative saved", `NOT saved:"Role saved"`, `{}`, true},
		{"saved collapse role", `saved:"Role collapse"`, `{}`, true},
		{"between", `between:("Route Person", "Route Other")`, `{}`, true},
		{"person any role", `person:"Route Person"`, `{}`, true},
		{"root people", ``, fmt.Sprintf(`{"people":[{"person_id":%q,"roles":["author"]}]}`, person.PersonID), true},
		{"path", `path:/missing`, `{}`, false},
		{"collection", fmt.Sprintf(`collection:%s`, collection.ID()), `{}`, false},
		{"coverage", `text_coverage:complete`, `{}`, false},
		{"duplicates", `has_duplicates:true`, `{}`, false},
		{"saved collapse plain", `saved:"Plain collapse"`, `{}`, false},
		{"mentions", `mentions:"Route Person"`, `{}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			value := snapshotTestQuery(t, fmt.Sprintf(`{"syntax":"advanced","text":%q,"filters":%s}`, tc.text, tc.filters))
			compiled, err := compileQuery(ctx, value, queryResolver{q: s.db})
			require.NoError(t, err)
			require.Equal(t, tc.edges, compiled.predicate.personEdges)
			if tc.name == "negative role" {
				require.Empty(t, compiled.roleMatches)
			}
			rows, err := documentSnapshotPopulation(ctx, s.db, compiled, "", nil, CoverageSelection{})
			require.NoError(t, err)
			require.Equal(t, tc.edges, strings.Contains(rows.sql, "FROM json_each(?)"))
			reportQueries := &populationRecordingQuerier{metadataQuerier: s.db}
			report, err := documentReportPopulation(ctx, reportQueries, compiled, "", nil, CoverageSelection{})
			require.NoError(t, err)
			if !tc.edges {
				require.NotContains(t, report.sql, "prepared_member")
				require.Empty(t, reportQueries.statements)
			} else if tc.name == "saved collapse role" {
				require.NotEmpty(t, reportQueries.statements)
				require.Contains(t, report.sql, "prepared_member")
			}
		})
	}
}

func TestDocumentFacetPopulationRoutes(t *testing.T) {
	t.Parallel()
	fixture := newSnapshotFacetFixture(t)
	s, ctx := fixture.store, t.Context()
	person, err := s.CreatePerson(ctx, "Facet Route", "operator")
	require.NoError(t, err)
	_, err = s.CreateSavedQuery(ctx, "Facet Role", "", SavedQueryKindQuery, []byte(fmt.Sprintf(`{"filters":{"people":[{"person_id":%q,"roles":["author"]}]}}`, person.PersonID)))
	require.NoError(t, err)
	for _, text := range []string{`NOT author:"Facet Route"`, `NOT saved:"Facet Role"`} {
		value := snapshotTestQuery(t, fmt.Sprintf(`{"syntax":"advanced","text":%q,"filters":{"people":[{"person_id":%q,"roles":["author"]}],"collection_ids":[%q]}}`, text, person.PersonID, fixture.collections[0].ID()))
		original, err := compileQuery(ctx, value, queryResolver{q: s.db})
		require.NoError(t, err)
		facetQueries := &populationRecordingQuerier{metadataQuerier: s.db}
		_, err = materializeSnapshotFacet(ctx, facetQueries, original, "", fixture.selection, "collections", nil, 1000)
		require.NoError(t, err)
		var facetSQL string
		for _, statement := range facetQueries.statements {
			if strings.HasPrefix(statement, "WITH matched_members AS (") {
				facetSQL = statement
				break
			}
		}
		require.Contains(t, facetSQL, "FROM json_each(?)")
		for _, dimension := range []string{"person_role", "collections"} {
			scoped := queryWithoutSnapshotFacet(value, dimension)
			compiled, err := compileQuery(ctx, scoped, queryResolver{q: s.db})
			require.NoError(t, err)
			require.True(t, compiled.predicate.personEdges)
			population, err := documentSnapshotPopulation(ctx, s.db, compiled, "", nil, fixture.selection)
			require.NoError(t, err)
			require.Contains(t, population.sql, "FROM json_each(?)")
		}
	}
	rootOnly := snapshotTestQuery(t, fmt.Sprintf(`{"filters":{"people":[{"person_id":%q,"roles":["author"]}]}}`, person.PersonID))
	withoutPeople, err := compileQuery(ctx, queryWithoutSnapshotFacet(rootOnly, "person_role"), queryResolver{q: s.db})
	require.NoError(t, err)
	require.False(t, withoutPeople.predicate.personEdges)
	// A generic facet recomputes its own self-excluded population inline.
	plain := snapshotTestQuery(t, fmt.Sprintf(`{"syntax":"advanced","text":"path:/missing","filters":{"collection_ids":[%q]}}`, fixture.collections[0].ID()))
	plainCompiled, err := compileQuery(ctx, plain, queryResolver{q: s.db})
	require.NoError(t, err)
	facetQueries := &populationRecordingQuerier{metadataQuerier: s.db}
	_, err = materializeSnapshotFacet(ctx, facetQueries, plainCompiled, "", fixture.selection, "collections", nil, 1000)
	require.NoError(t, err)
	var facetSQL string
	for _, statement := range facetQueries.statements {
		if strings.HasPrefix(statement, "WITH matched_members AS (") {
			facetSQL = statement
			break
		}
	}
	require.NotEmpty(t, facetSQL)
	require.NotContains(t, facetSQL, "FROM json_each(?)")
	// The People facet prepares its own role-free population before joining edges.
	compiled := compileFixtureQuery(t, s, `path:/missing`, query.Filters{})
	require.False(t, compiled.predicate.personEdges)
	recording := &populationRecordingQuerier{metadataQuerier: s.db}
	_, err = materializePersonRoleFacet(ctx, recording, compiled, "", fixture.selection, 1000)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(recording.statements), 2)
	require.Contains(t, recording.statements[len(recording.statements)-1], "FROM json_each(?)")
}

func TestPreparedMembershipExactPairsAndEmptyNot(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	first, err := s.CreateFile(ctx, s.RootID(), "pair-first.txt", fakeHash("ce01"), 10, "text/plain")
	require.NoError(t, err)
	second, err := s.CreateFile(ctx, s.RootID(), "pair-second.txt", fakeHash("ce02"), 10, "text/plain")
	require.NoError(t, err)
	compiled := compileFixtureQuery(t, s, `path:/pair-first.txt OR path:/pair-second.txt`, query.Filters{})
	predicate, err := prepareCompiledPredicate(ctx, s.db, compiled, "", nil, CoverageSelection{})
	require.NoError(t, err)
	for _, tc := range []struct {
		node    int64
		version string
		want    bool
	}{{first.ID, first.CurrentVersionID, true}, {second.ID, second.CurrentVersionID, true}, {first.ID, second.CurrentVersionID, false}, {second.ID, first.CurrentVersionID, false}} {
		args := append(append([]any(nil), predicate.args...), tc.node, tc.version)
		var got bool
		err := s.db.QueryRowContext(ctx, `SELECT `+predicate.sql+` FROM (SELECT ? AS id) n, (SELECT ? AS version_id) cv`, args...).Scan(&got)
		require.NoError(t, err)
		require.Equal(t, tc.want, got)
	}
	empty := compileFixtureQuery(t, s, `NOT path:/absent`, query.Filters{})
	predicate, err = prepareCompiledPredicate(ctx, s.db, empty, "", nil, CoverageSelection{})
	require.NoError(t, err)
	args := append(append([]any(nil), predicate.args...), first.ID, first.CurrentVersionID)
	var got bool
	err = s.db.QueryRowContext(ctx, `SELECT `+predicate.sql+` FROM (SELECT ? AS id) n, (SELECT ? AS version_id) cv`, args...).Scan(&got)
	require.NoError(t, err)
	require.True(t, got)
}
