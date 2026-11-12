package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
)

// documentSnapshotPopulation hands person-edge populations to row, facet and
// annotation SQL as exact identities. A supported 64-person/11-role query
// failed pure-Go SQLite row materialization ("VIEWs and/or subqueries nested
// too deep") when its predicate stayed inline. Person-edge-free queries stay
// inline. The engine guard's exact site and threshold are unmeasured. The
// identities stay in SQLite on the pinned read connection (see storePopulation)
// and q must be that connection.
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
	set, err := storePopulation(ctx, q, statement, args)
	if err != nil {
		return compiledQueryFragment{}, fmt.Errorf("materializing document membership: %w", err)
	}
	return compiledQueryFragment{sql: set.selection(), args: set.args()}, nil
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
// prepared fragment is not bound again. Each leaf contributes its two set
// bounds, never its identities.
func prepareCompiledLeaves(ctx context.Context, q metadataQuerier, predicate compiledQueryFragment, generation string, coverage CoverageSelection) (compiledQueryFragment, error) {
	if !predicate.personEdges || len(predicate.leaves) == 0 {
		return predicate, nil
	}
	sets := make([]populationSet, len(predicate.leaves))
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
		if sets[i], err = storePopulation(ctx, q, statement, args); err != nil {
			return compiledQueryFragment{}, fmt.Errorf("preparing query leaf: %w", err)
		}
	}
	var rebuilt strings.Builder
	args := make([]any, 0, len(predicate.args)+2*len(sets))
	sqlAt, argAt := 0, 0
	for i, leaf := range predicate.leaves {
		rebuilt.WriteString(predicate.sql[sqlAt:leaf.sqlStart])
		rebuilt.WriteString(`(n.id,cv.version_id) IN (` + sets[i].selection() + `)`)
		args = append(args, predicate.args[argAt:leaf.argStart]...)
		args = append(args, sets[i].args()...)
		sqlAt, argAt = leaf.sqlEnd, leaf.argEnd
	}
	rebuilt.WriteString(predicate.sql[sqlAt:])
	args = append(args, predicate.args[argAt:]...)
	// Every shared relation reference in document predicates is a recorded
	// coverage or duplicate leaf, all of which were just replaced.
	predicate.sql, predicate.args, predicate.relations, predicate.leaves = rebuilt.String(), args, 0, nil
	return predicate, nil
}

// populationMembersTable holds every node/current-version set one pinned
// lexical read prepares. It is TEMP state of that read's connection, created
// once on first use, and dropped when the read ends (pinnedLexicalRead.release).
const populationMembersTable = "temp.query_population_members"

// populationScratch tracks the pinned read's TEMP relation: whether it exists
// and how many rows it holds. Rows are only appended, so their rowids are
// consecutive and a prepared set is the rowid interval its insert added.
type populationScratch struct {
	created bool
	rows    int64
}

// populationSet is the rowid interval (after, through] of one prepared set.
type populationSet struct{ after, through int64 }

func (s populationSet) selection() string {
	return `SELECT node_id,content_version_id FROM ` + populationMembersTable + ` WHERE rowid>? AND rowid<=?`
}

func (s populationSet) args() []any { return []any{s.after, s.through} }

// populationQuerier is the narrow pinned-read owner the carrier needs: the
// read's queries, its exec, and its TEMP relation state. The pooled database
// has no such state, so a pooled handle cannot be used by mistake.
type populationQuerier interface {
	metadataQuerier
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	scratch() *populationScratch
}

// pinnedLexicalRead is the one connection of a lexical generation read and
// the owner of its TEMP population relation.
type pinnedLexicalRead struct {
	*sql.Conn

	population populationScratch
}

func (r *pinnedLexicalRead) scratch() *populationScratch { return &r.population }

func (s *Store) pinLexicalRead(ctx context.Context) (*pinnedLexicalRead, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquiring lexical generation connection: %w", err)
	}
	return &pinnedLexicalRead{Conn: conn}, nil
}

// release drops the TEMP relation, which outlives a pooled connection's Close,
// then returns the connection. It runs after the read's rollback or commit and
// is not canceled with the read. A connection that could not drop it is
// discarded rather than pooled, so no later snapshot can see these rows.
func (r *pinnedLexicalRead) release(ctx context.Context) error {
	if r.population.created {
		if _, err := r.ExecContext(context.WithoutCancel(ctx), `DROP TABLE IF EXISTS `+populationMembersTable); err != nil {
			_ = r.Raw(func(any) error { return driver.ErrBadConn })
			return fmt.Errorf("dropping population scratch: %w", err)
		}
	}
	return r.Close()
}

// storePopulation appends a node/current-version selection to the pinned read's
// TEMP relation in one INSERT...SELECT. Neither the identities nor any copy of
// them enters Go or comes back as an argument, and the selection is inserted as
// written so the statement that failed a supported 64-person/11-role query
// gains no nesting level (its limit is unmeasured).
func storePopulation(ctx context.Context, q metadataQuerier, statement string, args []any) (populationSet, error) {
	pinned, ok := q.(populationQuerier)
	if !ok {
		return populationSet{}, errors.New("population preparation needs a pinned lexical generation read")
	}
	scratch := pinned.scratch()
	if !scratch.created {
		// Marked first so a failed attempt is still dropped on release.
		scratch.created = true
		if _, err := pinned.ExecContext(ctx, `CREATE TABLE `+populationMembersTable+`(node_id INTEGER NOT NULL,content_version_id TEXT NOT NULL)`); err != nil {
			return populationSet{}, err
		}
	}
	result, err := pinned.ExecContext(ctx, `INSERT INTO `+populationMembersTable+`(node_id,content_version_id) `+statement, args...)
	if err != nil {
		return populationSet{}, err
	}
	added, err := result.RowsAffected()
	if err != nil {
		return populationSet{}, err
	}
	set := populationSet{scratch.rows, scratch.rows + added}
	scratch.rows = set.through
	return set, ctx.Err()
}
