// Package sqlitevec implements vector.Store on top of SQLite with the
// sqlite-vec extension. It is a reference backend: a worked example of the
// storage contract the vector flows depend on, built against sqlite-vec.
//
// The caller selects the SQLite driver and registers its sqlite-vec extension.
// Driver selection is independent of whether the application enables cgo.
// For modernc.org/sqlite, import both the driver and its extension:
//
//	import _ "modernc.org/sqlite"
//	import _ "modernc.org/sqlite/vec"
//	db, _ := sql.Open("sqlite", path)
//
// For github.com/mattn/go-sqlite3, register its extension before opening:
//
//	import _ "github.com/mattn/go-sqlite3"
//	import vecext "github.com/asg017/sqlite-vec-go-bindings/cgo"
//	vecext.Auto()
//	db, _ := sql.Open("sqlite3", path)
//
// The caller owns the documents table; this package owns a small set of
// vector tables derived from VectorsPrefix. Each generation gets its own
// vec0 virtual table sized to that generation's dimension, so generations
// with different model dimensions coexist while generations overlap.
package sqlitevec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"

	"go.kenn.io/kit/vector"
)

var identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// State is a generation's role in the active/building lifecycle. Only
// building and active generations are searched; building sorts ahead of
// active so Merge keeps the newer generation's hit on overlap.
type State string

const (
	StatePending  State = "pending"
	StateBuilding State = "building"
	StateActive   State = "active"
	StateRetired  State = "retired"
)

// Schema names the caller's documents table and the prefix for the
// vector tables this package manages. Every field must be a bare SQL
// identifier; values are validated before being interpolated into SQL.
type Schema struct {
	DocsTable      string // caller's documents table, e.g. "messages"
	IDColumn       string // primary key column, e.g. "id"
	ContentColumn  string // text to embed, e.g. "body"
	EmbedGenColumn string // nullable generation stamp, e.g. "embed_gen"
	VectorsPrefix  string // prefix for managed tables, e.g. "message_vectors"

	// RevisionColumn optionally names a column that changes whenever the
	// content changes, e.g. "last_modified". When set, SaveVectors stamps
	// a document only if its revision still matches the value read by
	// PendingForGeneration, and returns vector.ErrStale otherwise, so a
	// concurrent edit is never overwritten by a stale stamp. Leave it
	// empty only when documents are immutable while a fill runs.
	RevisionColumn string
}

func (s Schema) validate() error {
	for name, value := range map[string]string{
		"docs table":       s.DocsTable,
		"id column":        s.IDColumn,
		"content column":   s.ContentColumn,
		"embed gen column": s.EmbedGenColumn,
		"vectors prefix":   s.VectorsPrefix,
	} {
		if !identifierPattern.MatchString(value) {
			return fmt.Errorf("invalid %s %q", name, value)
		}
	}
	if s.RevisionColumn != "" && !identifierPattern.MatchString(s.RevisionColumn) {
		return fmt.Errorf("invalid revision column %q", s.RevisionColumn)
	}
	return nil
}

// Store implements vector.Store[K, G] against SQLite + sqlite-vec. K is the
// caller's document key type and G its generation key type; both must be
// types database/sql can bind and scan (for example int64 or string).
type Store[K, G comparable] struct {
	db     *sql.DB
	schema Schema
}

// New returns a Store bound to db. The caller retains ownership of db. New
// creates the generations and chunks bookkeeping tables if they do not
// exist; per-generation vec0 tables are created by EnsureGeneration.
func New[K, G comparable](ctx context.Context, db *sql.DB, schema Schema) (*Store[K, G], error) {
	if err := schema.validate(); err != nil {
		return nil, err
	}
	if db == nil {
		return nil, errors.New("db is nil")
	}
	s := &Store[K, G]{db: db, schema: schema}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s (
    ordinal     INTEGER PRIMARY KEY,
    gen_key     UNIQUE,
    fingerprint TEXT NOT NULL,
    dimension   INTEGER NOT NULL,
    state       TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS %s (
    ordinal     INTEGER NOT NULL,
    doc_key     NOT NULL,
    chunk_index INTEGER NOT NULL,
    vec_rowid   INTEGER NOT NULL,
    PRIMARY KEY (ordinal, doc_key, chunk_index)
);
CREATE TABLE IF NOT EXISTS %s (
    ordinal  INTEGER NOT NULL,
    doc_key  NOT NULL,
    revision,
    PRIMARY KEY (ordinal, doc_key)
);
CREATE INDEX IF NOT EXISTS %s ON %s (ordinal, vec_rowid);
CREATE INDEX IF NOT EXISTS %s ON %s (doc_key, ordinal, vec_rowid);
CREATE INDEX IF NOT EXISTS %s ON %s (doc_key, revision);`,
		s.generationsTable(), s.chunksTable(), s.stampsTable(),
		s.chunksByVectorIndex(), s.chunksTable(),
		s.chunksByDocIndex(), s.chunksTable(),
		s.stampsByDocRevisionIndex(), s.stampsTable())); err != nil {
		return nil, fmt.Errorf("create bookkeeping tables: %w", err)
	}
	return s, nil
}

func (s *Store[K, G]) generationsTable() string { return s.schema.VectorsPrefix + "_generations" }
func (s *Store[K, G]) chunksTable() string      { return s.schema.VectorsPrefix + "_chunks" }
func (s *Store[K, G]) stampsTable() string      { return s.schema.VectorsPrefix + "_stamps" }
func (s *Store[K, G]) chunksByVectorIndex() string {
	return s.schema.VectorsPrefix + "_chunks_by_vector"
}
func (s *Store[K, G]) chunksByDocIndex() string { return s.schema.VectorsPrefix + "_chunks_by_doc" }
func (s *Store[K, G]) stampsByDocRevisionIndex() string {
	return s.schema.VectorsPrefix + "_stamps_by_doc_revision"
}

func (s *Store[K, G]) vecTable(ordinal int64) string {
	return fmt.Sprintf("%s_v%d", s.schema.VectorsPrefix, ordinal)
}

// EnsureGeneration registers gen with model's vector-space fingerprint,
// dimension, and the given state, creating its vec0 table on first use.
// Calling it again updates only the state; a generation's vector space is
// fixed once created. A retired generation stays retired: ensuring it as
// retired is a no-op that does not recreate reclaimed storage, and any other
// state is an error. Use Activate to publish a covered generation.
func (s *Store[K, G]) EnsureGeneration(ctx context.Context, gen G, model vector.Generation, state State) error {
	if model.Dimensions <= 0 {
		return fmt.Errorf("generation dimension must be positive, got %d", model.Dimensions)
	}
	fingerprint := model.Fingerprint()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin ensure generation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	current, found, err := s.generationStateTx(ctx, tx, gen)
	if err != nil {
		return err
	}
	if found && current == StateRetired {
		if state == StateRetired {
			return nil
		}
		return fmt.Errorf("generation %v: %w and cannot become %s", gen, ErrRetired, state)
	}

	res, err := tx.ExecContext(ctx, fmt.Sprintf(`
INSERT INTO %s (gen_key, fingerprint, dimension, state) VALUES (?, ?, ?, ?)
ON CONFLICT(gen_key) DO UPDATE SET state = excluded.state
WHERE fingerprint = excluded.fingerprint AND dimension = excluded.dimension`, s.generationsTable()),
		gen, fingerprint, model.Dimensions, string(state))
	if err != nil {
		return fmt.Errorf("upsert generation: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("upsert generation rows: %w", err)
	} else if n == 0 {
		return fmt.Errorf("generation %v already exists with different model fingerprint or dimension", gen)
	}

	ordinal, dimension, err := s.lookupGenerationTx(ctx, tx, gen)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(
		`CREATE VIRTUAL TABLE IF NOT EXISTS %s USING vec0(embedding float[%d] distance_metric=cosine)`,
		s.vecTable(ordinal), dimension)); err != nil {
		return fmt.Errorf("create vec0 table: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit ensure generation: %w", err)
	}
	return nil
}

// SetGenerationState transitions gen to state. The caller owns the
// active/building lifecycle; this only records the decision. A retired
// generation cannot leave retired, because Reclaim may already have dropped
// its vectors. Activate is the checked way to publish a generation.
func (s *Store[K, G]) SetGenerationState(ctx context.Context, gen G, state State) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin set generation state: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	current, found, err := s.generationStateTx(ctx, tx, gen)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("generation %v: %w", gen, ErrGenerationNotFound)
	}
	if current == StateRetired && state != StateRetired {
		return fmt.Errorf("generation %v: %w and cannot become %s", gen, ErrRetired, state)
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %s SET state = ? WHERE gen_key = ?`, s.generationsTable()),
		string(state), gen); err != nil {
		return fmt.Errorf("set generation state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit generation state: %w", err)
	}
	return nil
}

// generationStateTx reads gen's state inside tx. found is false when gen was
// never ensured.
func (s *Store[K, G]) generationStateTx(ctx context.Context, tx *sql.Tx, gen G) (State, bool, error) {
	var state string
	err := tx.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT state FROM %s WHERE gen_key = ?`, s.generationsTable()), gen).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read generation %v state: %w", gen, err)
	}
	return State(state), true, nil
}

func (s *Store[K, G]) lookupGeneration(ctx context.Context, gen G) (ordinal int64, dimension int, err error) {
	return s.scanGeneration(s.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT ordinal, dimension FROM %s WHERE gen_key = ?`, s.generationsTable()), gen), gen)
}

func (s *Store[K, G]) lookupGenerationTx(ctx context.Context, tx *sql.Tx, gen G) (int64, int, error) {
	return s.scanGeneration(tx.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT ordinal, dimension FROM %s WHERE gen_key = ?`, s.generationsTable()), gen), gen)
}

func (s *Store[K, G]) scanGeneration(row *sql.Row, gen G) (int64, int, error) {
	var ordinal int64
	var dimension int
	if err := row.Scan(&ordinal, &dimension); err != nil {
		if err == sql.ErrNoRows {
			return 0, 0, fmt.Errorf("generation %v: %w", gen, ErrGenerationNotFound)
		}
		return 0, 0, fmt.Errorf("lookup generation %v: %w", gen, err)
	}
	return ordinal, dimension, nil
}
