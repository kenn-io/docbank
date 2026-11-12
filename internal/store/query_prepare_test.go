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

// This exercises SQL membership under OR and NOT with duplicate and path predicates.
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
	population, err := matchedPopulation(compiled, "", nil)
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
	_, err := materializeDocumentPopulation(ctx, s.db, compiled, "", CoverageSelection{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want canceled", err)
	}
}

// The connector supplies only cursor lifecycle behavior. SQL membership is
// exercised by the real-store tests; these rows are exact synthetic identities.
type preparationLifecycleConnector struct{ rows *preparationLifecycleRows }

func (c preparationLifecycleConnector) Connect(context.Context) (driver.Conn, error) {
	return preparationLifecycleConn(c), nil
}
func (c preparationLifecycleConnector) Driver() driver.Driver { return c }
func (c preparationLifecycleConnector) Open(string) (driver.Conn, error) {
	return preparationLifecycleConn(c), nil
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
	final := compileFixtureQuery(t, s, `name:synthetic`, query.Filters{})
	_, err := s.CreatePerson(t.Context(), "Lifecycle Absent", "operator")
	require.NoError(t, err)
	// A person-edge leaf is read first; its failing cursor must stop before
	// any final membership query or payload.
	leaf := compileFixtureQuery(t, s, `NOT author:"Lifecycle Absent" AND path:/missing`, query.Filters{})
	require.True(t, leaf.predicate.personEdges)
	require.Len(t, leaf.predicate.leaves, 1)
	iterationErr := errors.New("synthetic iteration error")
	closeErr := errors.New("synthetic close error")
	for _, route := range []struct {
		name     string
		compiled CompiledQuery
	}{{"final", final}, {"leaf", leaf}} {
		for _, failure := range []string{"cancel", "iteration", "close"} {
			t.Run(route.name+"/"+failure, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				r := &preparationLifecycleRows{}
				want := context.Canceled
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
				got, err := materializeDocumentPopulation(ctx, db, route.compiled, "", CoverageSelection{})
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

// Record the largest individual value handed across the query boundary. A
// stream of constant-size operations is different from one corpus-sized value.
type populationSizeQuerier struct {
	metadataQuerier
	maxArgument, maxStatement int
}

func populationArgumentSize(args []any) int {
	largest := 0
	for _, arg := range args {
		switch value := arg.(type) {
		case string:
			largest = max(largest, len(value))
		case []byte:
			largest = max(largest, len(value))
		}
	}
	return largest
}

func (q *populationSizeQuerier) QueryContext(ctx context.Context, statement string, args ...any) (*sql.Rows, error) {
	q.maxStatement = max(q.maxStatement, len(statement))
	q.maxArgument = max(q.maxArgument, populationArgumentSize(args))
	return q.metadataQuerier.QueryContext(ctx, statement, args...)
}

func TestDocumentPopulationCarrierDoesNotGrowWithCorpus(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	dir, err := s.Mkdir(ctx, s.RootID(), "bulk")
	require.NoError(t, err)
	_, err = s.CreatePerson(ctx, "Absent Author", "operator")
	require.NoError(t, err)
	compiled := compileFixtureQuery(t, s, `NOT author:"Absent Author" AND path:/bulk`, query.Filters{})
	var expected []int64
	type sizes struct{ argument, statement, fragmentArgument, fragmentSQL int }
	measure := func() sizes {
		var got sizes
		err := s.withLexicalGenerationRead(ctx, func(q metadataQuerier, generation LexicalGeneration) error {
			recording := &populationSizeQuerier{metadataQuerier: q}
			fragment, err := documentSnapshotPopulation(ctx, recording, compiled, generation.ID, CoverageSelection{})
			if err != nil {
				return err
			}
			got = sizes{recording.maxArgument, recording.maxStatement, populationArgumentSize(fragment.args), len(fragment.sql)}
			ids, _ := preparedLeafMembers(t, q, fragment.sql, fragment.args)
			require.Equal(t, expected, ids)
			return nil
		})
		require.NoError(t, err)
		return got
	}
	add := func(start, end int) {
		for i := start; i < end; i++ {
			node, err := s.CreateFile(ctx, dir.ID, fmt.Sprintf("file-%03d.txt", i), testSHA256([]byte(fmt.Sprintf("carrier-%03d", i))), 10, "text/plain")
			require.NoError(t, err)
			expected = append(expected, node.ID)
		}
	}
	const n = 16
	add(0, n)
	small := measure()
	add(n, 4*n)
	large := measure()
	require.Equal(t, small, large, "individual SQL-bound and returned carrier sizes must not grow with corpus membership")
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
		{"collection", `collection:` + collection.ID(), `{}`, false},
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
			rows, err := documentSnapshotPopulation(ctx, s.db, compiled, "", CoverageSelection{})
			require.NoError(t, err)
			require.Equal(t, tc.edges, strings.Contains(rows.sql, "FROM json_each(?)"))
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
			population, err := documentSnapshotPopulation(ctx, s.db, compiled, "", fixture.selection)
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
	// The People facet keeps a role-free population inline before joining edges.
	compiled := compileFixtureQuery(t, s, `path:/missing`, query.Filters{})
	require.False(t, compiled.predicate.personEdges)
	recording := &populationRecordingQuerier{metadataQuerier: s.db}
	_, err = materializePersonRoleFacet(ctx, recording, compiled, "", fixture.selection, 1000)
	require.NoError(t, err)
	require.NotEmpty(t, recording.statements)
	require.NotContains(t, recording.statements[len(recording.statements)-1], "FROM json_each(?)")
}

// preparedLeafFixture holds live, trashed and replaced files so leaf sets are
// checked against exact current identities. "Leaf Absent" asserts nothing, so
// NOT author:"Leaf Absent" adds person edges without restricting membership.
type preparedLeafFixture struct {
	store                           *Store
	first, second, unique, replaced int64
	collected                       int64
	current                         map[int64]string
	collection                      string
	author                          string
	selection                       CoverageSelection
}

func newPreparedLeafFixture(t *testing.T) preparedLeafFixture {
	t.Helper()
	s := newTestStore(t)
	ctx := t.Context()
	dir, err := s.Mkdir(ctx, s.RootID(), "dir")
	require.NoError(t, err)
	first, err := s.CreateFile(ctx, s.RootID(), "first.txt", fakeHash("leaf-duplicate"), 10, "text/plain")
	require.NoError(t, err)
	second, err := s.CreateFile(ctx, dir.ID, "second.txt", first.BlobHash, first.Size, first.MimeType)
	require.NoError(t, err)
	unique, err := s.CreateFile(ctx, dir.ID, "unique.txt", fakeHash("leaf-unique"), 11, "text/plain")
	require.NoError(t, err)
	// Neither a trashed copy nor an old version makes unique a duplicate.
	trashed, err := s.CreateFile(ctx, s.RootID(), "trashed.txt", unique.BlobHash, unique.Size, unique.MimeType)
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, trashed.ID, UnconditionalRev)
	require.NoError(t, err)
	replaced, err := s.CreateFile(ctx, dir.ID, "replaced.txt", unique.BlobHash, unique.Size, unique.MimeType)
	require.NoError(t, err)
	_, version, err := s.ReplaceContent(ctx, replaced.ID, UnconditionalRev, fakeHash("leaf-replaced"), 12, "text/plain")
	require.NoError(t, err)
	collection := createCollectionRun(t, s, "collected.txt", "leaf-collection")
	f := preparedLeafFixture{store: s, first: first.ID, second: second.ID, unique: unique.ID, replaced: replaced.ID,
		collection: collection.ID(), current: map[int64]string{
			first.ID: first.CurrentVersionID, second.ID: second.CurrentVersionID,
			unique.ID: unique.CurrentVersionID, replaced.ID: version.ID,
		}}
	var collectedVersion string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT id,current_version_id FROM nodes WHERE name='collected.txt'`).Scan(&f.collected, &collectedVersion))
	f.current[f.collected] = collectedVersion
	author, err := s.CreatePerson(ctx, "Leaf Author", "operator")
	require.NoError(t, err)
	f.author = author.PersonID
	_, err = s.CreatePerson(ctx, "Leaf Absent", "operator")
	require.NoError(t, err)
	for _, version := range []string{second.CurrentVersionID, unique.CurrentVersionID} {
		_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: version, PersonID: author.PersonID, Role: "author", Action: "assert", Revision: 1})
		require.NoError(t, err)
	}
	profile := catalogProcessingProfile(t, false)
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		return ensureProcessingProfileTx(ctx, tx, profile)
	}))
	f.selection = CoverageSelection{Configuration: "configured", ProfileFingerprint: profile.Fingerprint}
	return f
}

// preparedLeafMembers returns ordered node IDs and node/version pairs.
func preparedLeafMembers(t *testing.T, q metadataQuerier, statement string, args []any) ([]int64, []string) {
	t.Helper()
	rows, err := q.QueryContext(t.Context(), `SELECT node_id,content_version_id FROM (`+statement+`) members ORDER BY node_id`, args...)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var ids []int64
	var pairs []string
	for rows.Next() {
		var id int64
		var version string
		require.NoError(t, rows.Scan(&id, &version))
		ids = append(ids, id)
		pairs = append(pairs, fmt.Sprintf("%d/%s", id, version))
	}
	require.NoError(t, rows.Err())
	return ids, pairs
}

// Each leaf is replaced by its own exact live/current set; the original SQL
// of the same compiled query is the reference, and stated sets pin intent.
func TestPreparedLeafMembershipMatchesInlineSQL(t *testing.T) {
	t.Parallel()
	f := newPreparedLeafFixture(t)
	s, ctx := f.store, t.Context()
	_, err := s.CreateSavedQuery(ctx, "Leaf collapse", "", SavedQueryKindQuery, []byte(`{"syntax":"advanced","text":"has_duplicates:true","filters":{"collapse_duplicates":true}}`))
	require.NoError(t, err)
	_, err = s.CreateSavedQuery(ctx, "Leaf nested", "", SavedQueryKindQuery, []byte(`{"syntax":"advanced","text":"NOT author:\"Leaf Absent\" AND saved:\"Leaf collapse\" AND path:/dir"}`))
	require.NoError(t, err)
	absent := `NOT author:"Leaf Absent" AND `
	all := []int64{f.first, f.second, f.unique, f.replaced, f.collected}
	slices.Sort(all)
	sorted := func(ids ...int64) []int64 { slices.Sort(ids); return ids }
	duplicates := sorted(f.first, f.second)
	for _, tc := range []struct {
		text     string
		collapse bool
		want     []int64 // nil: one duplicate representative
		count    int
	}{
		{text: absent + `(has_duplicates:true OR has_duplicates:false)`, want: all},
		// Adjacent zero-argument leaves sit between parameterized person and path leaves.
		{text: absent + `has_duplicates:true AND NOT has_duplicates:false AND path:/dir`, want: []int64{f.second}},
		{text: `has_duplicates:false AND has_duplicates:false AND ` + absent + `NOT path:"/dir/unique.txt"`, want: sorted(f.replaced, f.collected)},
		{text: absent + `(NOT path:/ OR has_duplicates:false)`, want: sorted(f.unique, f.replaced, f.collected)},
		{text: `path:/ AND ` + absent + `has_duplicates:true`, want: duplicates},
		{text: absent + `NOT (has_duplicates:true OR path:/dir)`, want: []int64{f.collected}},
		{text: absent + `NOT (has_duplicates:true OR path:/)`, want: []int64{}},
		{text: absent + `collection:` + f.collection + ` AND has_duplicates:false`, want: []int64{f.collected}},
		{text: absent + `NOT collection:` + f.collection + ` AND NOT has_duplicates:true`, want: sorted(f.unique, f.replaced)},
		{text: absent + `text_coverage:unprocessed AND has_duplicates:true`, count: -1},
		{text: absent + `NOT text_coverage:complete AND path:/dir AND has_duplicates:false`, want: sorted(f.unique, f.replaced)},
		{text: `author:"Leaf Author" AND has_duplicates:false`, want: []int64{f.unique}},
		{text: absent + `saved:"Leaf collapse"`, count: 1},
		{text: absent + `NOT saved:"Leaf collapse"`, count: 4},
		{text: absent + `has_duplicates:true`, collapse: true, count: 1},
		{text: `saved:"Leaf nested" OR (` + absent + `has_duplicates:false AND path:/dir)`, count: -1},
	} {
		value := snapshotTestQuery(t, fmt.Sprintf(`{"syntax":"advanced","text":%q,"filters":{"collapse_duplicates":%t}}`, tc.text, tc.collapse))
		compiled, err := compileQuery(ctx, value, queryResolver{q: s.db})
		require.NoError(t, err, tc.text)
		require.True(t, compiled.predicate.personEdges, tc.text)
		require.NotEmpty(t, compiled.predicate.leaves, tc.text)
		inline, err := matchedPopulation(compiled, "", nil)
		require.NoError(t, err, tc.text)
		statement, args, err := bindQueryPopulation(inline, f.selection, "")
		require.NoError(t, err, tc.text)
		_, wantPairs := preparedLeafMembers(t, s.db, statement, args)

		bound, err := compiled.bind("", nil)
		require.NoError(t, err, tc.text)
		prepared, err := preparedPopulation(ctx, s.db, compiled, "", nil, f.selection)
		require.NoError(t, err, tc.text)
		require.Zero(t, prepared.relations, tc.text)
		require.Empty(t, prepared.leaves, tc.text)
		for _, cte := range []string{"query_ancestry", "collection_members", "current_content_members", "processing_coverage"} {
			require.NotContains(t, prepared.sql, cte, tc.text)
		}
		leafArgs := 0
		for _, leaf := range bound.leaves {
			leafArgs += leaf.argEnd - leaf.argStart
		}
		require.Len(t, prepared.args, len(bound.args)-leafArgs+len(bound.leaves), tc.text)

		handoff, err := materializeDocumentPopulation(ctx, s.db, compiled, "", f.selection)
		require.NoError(t, err, tc.text)
		ids, pairs := preparedLeafMembers(t, s.db, handoff.sql, handoff.args)
		require.Equal(t, wantPairs, pairs, tc.text)
		for i, id := range ids {
			require.Equal(t, fmt.Sprintf("%d/%s", id, f.current[id]), pairs[i], tc.text)
		}
		switch {
		case tc.want != nil:
			require.Equal(t, tc.want, append([]int64{}, ids...), tc.text)
		case tc.count == 1:
			require.Len(t, ids, 1, tc.text)
			require.Contains(t, duplicates, ids[0], tc.text)
		case tc.count > 0:
			require.Len(t, ids, tc.count, tc.text)
		}
	}
}
