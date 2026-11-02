package sqlitevec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrUncovered reports that Activate refused to publish a generation
// because at least one document is not covered at its current revision.
// The state change is not committed. Callers retry after a later fill.
var ErrUncovered = errors.New("generation has uncovered documents")

// ErrGenerationNotFound reports a generation key that EnsureGeneration never
// registered.
var ErrGenerationNotFound = errors.New("generation not ensured")

// ErrRetired reports a retired generation used where only a building or
// active one is allowed. A retired generation never becomes live again.
var ErrRetired = errors.New("generation is retired")

// UncoveredError is the Activate failure for an incomplete generation.
// Backlog is the number of documents that still need a current stamp.
type UncoveredError struct {
	Backlog int64
}

func (e *UncoveredError) Error() string {
	return fmt.Sprintf("generation has %d uncovered documents", e.Backlog)
}

func (e *UncoveredError) Unwrap() error { return ErrUncovered }

// Coverage counts documents in the caller's table for one generation.
// Embedded documents have a current stamp and at least one chunk.
// Skipped documents have a current stamp and no chunks, which is how a
// stamp-only save records blank or deliberately skipped content.
// Backlog documents are not covered: they have no stamp, or the stamped
// revision no longer matches. where is an optional predicate over the
// documents table aliased as d, with ? placeholders bound from args.
// An empty where counts the whole table. The counts use the same
// freshness rule as search, so a stale revision is backlog even when
// old vectors are still stored.
type Coverage struct {
	Embedded int64
	Skipped  int64
	Backlog  int64
}

type rowQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Coverage reports gen's embedded, stamp-only, and uncovered documents.
func (s *Store[K, G]) Coverage(ctx context.Context, gen G, where string, args ...any) (Coverage, error) {
	ordinal, _, err := s.lookupGeneration(ctx, gen)
	if err != nil {
		return Coverage{}, err
	}
	return s.coverageOn(ctx, s.db, ordinal, where, args)
}

func (s *Store[K, G]) coverageOn(ctx context.Context, q rowQueryer, ordinal int64, where string, args []any) (Coverage, error) {
	var coverage Coverage
	if err := q.QueryRowContext(ctx, s.coverageQuery(where), append([]any{ordinal}, args...)...).Scan(
		&coverage.Embedded, &coverage.Skipped, &coverage.Backlog); err != nil {
		return Coverage{}, fmt.Errorf("count generation coverage: %w", err)
	}
	return coverage, nil
}

func (s *Store[K, G]) coverageQuery(where string) string {
	covered := s.coveredPredicate("d", "stamp")
	hasChunks := fmt.Sprintf(`EXISTS (SELECT 1 FROM %s c WHERE c.ordinal = stamp.ordinal AND c.doc_key = d.%s)`,
		s.chunksTable(), s.schema.IDColumn)
	filter := ""
	if where != "" {
		filter = " WHERE (" + where + ")"
	}
	return fmt.Sprintf(`
SELECT
  COALESCE(SUM(CASE WHEN %[1]s AND %[2]s THEN 1 ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN %[1]s AND NOT %[2]s THEN 1 ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN NOT %[1]s THEN 1 ELSE 0 END), 0)
  FROM %[3]s d
  LEFT JOIN %[4]s stamp ON stamp.ordinal = ? AND stamp.doc_key = d.%[5]s%[6]s`,
		covered, hasChunks, s.schema.DocsTable, s.stampsTable(), s.schema.IDColumn, filter)
}

// Activate publishes gen as the active generation when every document is
// covered and gen's vec0 table still exists, and retires every other
// building or active generation, in one transaction. A non-zero backlog
// returns an error wrapping ErrUncovered and leaves every state unchanged.
// A missing vec0 table, including one Reclaim has dropped, also leaves
// every state unchanged. A retired generation is refused, so publishing
// it cannot retire the generation that is active now. Activate does not
// recreate the table. Activation
// does not drop storage; call Reclaim for a retired generation.
// LiveGenerations still returns building and active generations, so
// callers that serve only the active generation select it themselves.
func (s *Store[K, G]) Activate(ctx context.Context, gen G) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin generation activation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The assignment takes the write lock before the coverage read, so a
	// revision committed by a concurrent save cannot land between the
	// count and the publication.
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %s SET state = state WHERE gen_key = ?`, s.generationsTable()), gen); err != nil {
		return fmt.Errorf("lock generation %v: %w", gen, err)
	}
	ordinal, _, err := s.lookupGenerationTx(ctx, tx, gen)
	if err != nil {
		return err
	}
	var state string
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT state FROM %s WHERE gen_key = ?`, s.generationsTable()), gen).Scan(&state); err != nil {
		return fmt.Errorf("read generation %v state: %w", gen, err)
	}
	if State(state) == StateRetired {
		return fmt.Errorf("generation %v: %w", gen, ErrRetired)
	}
	coverage, err := s.coverageOn(ctx, tx, ordinal, "", nil)
	if err != nil {
		return err
	}
	if coverage.Backlog != 0 {
		return &UncoveredError{Backlog: coverage.Backlog}
	}
	// Reclaim drops this table and leaves the generation row. An empty
	// corpus has no backlog, so publication has to see the table itself.
	exists, err := s.vecTableExists(ctx, tx, ordinal)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("generation %v vec0 table %s is missing", gen, s.vecTable(ordinal))
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
UPDATE %s
   SET state = CASE WHEN gen_key = ? THEN ? ELSE ? END
 WHERE gen_key = ? OR state IN (?, ?)`, s.generationsTable()),
		gen, string(StateActive), string(StateRetired),
		gen, string(StateActive), string(StateBuilding)); err != nil {
		return fmt.Errorf("publish generation %v: %w", gen, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit generation activation: %w", err)
	}
	return nil
}

// vecTableExists reports whether the vec0 table EnsureGeneration created
// for ordinal is still present. The name is the store's own vecTable name.
func (s *Store[K, G]) vecTableExists(ctx context.Context, q rowQueryer, ordinal int64) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM sqlite_master WHERE name = ?`, s.vecTable(ordinal)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check vec0 table %s: %w", s.vecTable(ordinal), err)
	}
	return true, nil
}

// ActiveGeneration returns the newest active generation. Newest means the
// highest ordinal, which is the tiebreak when more than one row is active.
// ok is false when no generation is active.
func (s *Store[K, G]) ActiveGeneration(ctx context.Context) (GenerationInfo[G], bool, error) {
	var info GenerationInfo[G]
	var state string
	err := s.db.QueryRowContext(ctx, fmt.Sprintf(`
SELECT gen_key, fingerprint, dimension, state
  FROM %s
 WHERE state = ?
 ORDER BY ordinal DESC
 LIMIT 1`, s.generationsTable()), string(StateActive)).Scan(
		&info.Key, &info.Fingerprint, &info.Dimension, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return GenerationInfo[G]{}, false, nil
	}
	if err != nil {
		return GenerationInfo[G]{}, false, fmt.Errorf("read active generation: %w", err)
	}
	info.State = State(state)
	return info, true, nil
}

// Reclaim drops a retired generation's vec0 table, chunk rows, and stamps.
// The generation row stays, so the caller can still see that it existed.
// Reclaiming a generation that is not retired fails and deletes nothing.
// A second call after a successful reclaim succeeds.
func (s *Store[K, G]) Reclaim(ctx context.Context, gen G) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin generation reclamation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var ordinal int64
	var state string
	err = tx.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT ordinal, state FROM %s WHERE gen_key = ?`, s.generationsTable()), gen).Scan(&ordinal, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("generation %v: %w", gen, ErrGenerationNotFound)
	}
	if err != nil {
		return fmt.Errorf("read generation %v: %w", gen, err)
	}
	if State(state) != StateRetired {
		return fmt.Errorf("generation %v is %s; reclaim only a retired generation", gen, state)
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+s.vecTable(ordinal)); err != nil {
		return fmt.Errorf("drop retired vectors for generation %v: %w", gen, err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE ordinal = ?`, s.chunksTable()), ordinal); err != nil {
		return fmt.Errorf("delete retired chunks for generation %v: %w", gen, err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE ordinal = ?`, s.stampsTable()), ordinal); err != nil {
		return fmt.Errorf("delete retired stamps for generation %v: %w", gen, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit generation reclamation: %w", err)
	}
	return nil
}
