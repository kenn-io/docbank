// Package vector owns Docbank's rebuildable semantic-search sidecar. The
// authoritative metadata store supplies current extracted text; Kit owns the
// generation, fill, freshness, and sqlite-vec persistence contracts.
package vector

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"strconv"
	"sync"

	kitvec "go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

const (
	sidecarSchemaVersion = "1"
	vectorsPrefix        = "document_vectors"
	mirrorPageSize       = 500
	splitMaxRunes        = 2000
	splitOverlap         = 200
	documentRecipe       = "1"
)

var ErrBuildRunning = errors.New("an embeddings build is already running")

// Document is one current live source row for the derived mirror.
type Document struct {
	BlobHash         string
	ExtractorVersion int
	Text             string
}

// SourceFunc pages unique current embeddable content in stable digest order.
type SourceFunc func(ctx context.Context, afterBlobHash string, limit int) ([]Document, error)

// Progress reports unique-content build progress. Phase is "scanning" or
// "embedding"; Total becomes known once the mirror is current.
type Progress struct {
	Phase string
	Done  int
	Total int
}

// BuildResult summarizes one completed unique-content mirror refresh and generation fill.
type BuildResult struct {
	Fingerprint string
	Model       string
	Dimensions  int
	Mirrored    int
	Removed     int
	Embedded    int
	Chunks      int
	Skipped     int
	Stale       int
	Activated   bool
}

// GenerationInfo is one locally retained vector generation and its coverage
// of the current unique-content mirror.
type GenerationInfo struct {
	Fingerprint string
	Model       string
	Dimensions  int
	State       string
	Embedded    int
	Skipped     int
	Pending     int
}

// Index owns one vectors.db and serializes builds over it.
type Index struct {
	db    *sql.DB
	store *sqlitevec.Store[string, string]
	path  string

	mu      sync.Mutex
	running bool
}

// Open creates or opens the derived sidecar. A sidecar schema mismatch is
// handled by deleting and rebuilding it; vectors are never archive authority.
func Open(ctx context.Context, path string) (*Index, error) {
	if path == "" {
		return nil, errors.New("vector: sidecar path is required")
	}
	db, current, err := openCurrentSidecar(ctx, path)
	if err != nil {
		return nil, err
	}
	if !current {
		_ = db.Close()
		if err := removeSidecar(path); err != nil {
			return nil, err
		}
		db, current, err = openCurrentSidecar(ctx, path)
		if err != nil {
			return nil, err
		}
		if !current {
			_ = db.Close()
			return nil, errors.New("vector: recreated sidecar has an unexpected schema")
		}
	}
	store, err := sqlitevec.New[string, string](ctx, db, sqlitevec.Schema{
		DocsTable: "document_mirror", IDColumn: "blob_hash", ContentColumn: "content",
		EmbedGenColumn: "embed_gen", RevisionColumn: "source_revision",
		VectorsPrefix: vectorsPrefix,
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("vector: initialize Kit store: %w", err)
	}
	return &Index{db: db, store: store, path: path}, nil
}

func openCurrentSidecar(ctx context.Context, path string) (*sql.DB, bool, error) {
	sqlitevec.Register()
	db, err := sql.Open(sidecarDriver, sidecarDSN(path))
	if err != nil {
		return nil, false, fmt.Errorf("vector: open sidecar: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, false, fmt.Errorf("vector: open sidecar: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, false, fmt.Errorf("vector: protect sidecar: %w", err)
	}
	var found bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM sqlite_master WHERE type='table' AND name='vector_meta')`).Scan(&found); err != nil {
		_ = db.Close()
		return nil, false, fmt.Errorf("vector: inspect sidecar schema: %w", err)
	}
	if found {
		var version string
		err := db.QueryRowContext(ctx,
			`SELECT value FROM vector_meta WHERE key='schema_version'`).Scan(&version)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			_ = db.Close()
			return nil, false, fmt.Errorf("vector: inspect sidecar version: %w", err)
		}
		if version != sidecarSchemaVersion {
			return db, false, nil
		}
	}
	if err := ensureSidecarSchema(ctx, db); err != nil {
		_ = db.Close()
		return nil, false, err
	}
	return db, true, nil
}

func ensureSidecarSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS vector_meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS document_mirror (
  blob_hash       TEXT PRIMARY KEY,
  content         TEXT NOT NULL,
  source_revision TEXT NOT NULL,
  embed_gen       TEXT
);
CREATE TABLE IF NOT EXISTS embedding_generation_details (
  fingerprint TEXT PRIMARY KEY,
  model       TEXT NOT NULL
);
INSERT INTO vector_meta(key,value) VALUES('schema_version','`+sidecarSchemaVersion+`')
ON CONFLICT(key) DO NOTHING;`)
	if err != nil {
		return fmt.Errorf("vector: prepare sidecar schema: %w", err)
	}
	return nil
}

func removeSidecar(path string) error {
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("vector: remove stale sidecar %s: %w", candidate, err)
		}
	}
	return nil
}

// Close releases the sidecar database.
func (ix *Index) Close() error {
	if ix == nil || ix.db == nil {
		return nil
	}
	return ix.db.Close()
}

// Build refreshes the mirror, fills the configured generation, and activates
// it only after every current mirror row is covered. A failed new-generation
// build leaves the prior active generation available.
func (ix *Index) Build(
	ctx context.Context, source SourceFunc, generation kitvec.Generation,
	encode kitvec.EncodeFunc, batchSize, concurrency int, progress func(Progress),
) (BuildResult, error) {
	if !ix.beginBuild() {
		return BuildResult{}, ErrBuildRunning
	}
	defer ix.endBuild()
	if source == nil || encode == nil {
		return BuildResult{}, errors.New("vector: source and encoder are required")
	}
	generation = withDocumentRecipe(generation)
	if progress != nil {
		progress(Progress{Phase: "scanning"})
	}
	written, removed, err := ix.refresh(ctx, source)
	if err != nil {
		return BuildResult{}, err
	}
	fingerprint := generation.Fingerprint()
	result := BuildResult{
		Fingerprint: fingerprint, Model: generation.Model,
		Dimensions: generation.Dimensions, Mirrored: written, Removed: removed,
	}
	if err := ix.ensureGeneration(ctx, fingerprint, generation); err != nil {
		return result, err
	}
	total, err := ix.backlog(ctx, fingerprint)
	if err != nil {
		return result, err
	}
	if progress != nil {
		progress(Progress{Phase: "embedding", Total: total})
	}
	done := 0
	flowStore := progressStore{
		Store: ix.store,
		afterSave: func() {
			done++
			if progress != nil {
				progress(Progress{Phase: "embedding", Done: done, Total: total})
			}
		},
	}
	stats, err := kitvec.Fill[string, string](ctx, flowStore, fingerprint, encode,
		kitvec.FillOptions[string]{
			Split: kitvec.SplitOptions{MaxRunes: splitMaxRunes, Overlap: splitOverlap},
			Batch: kitvec.BatchOptions{BatchSize: batchSize}, Concurrency: concurrency,
		})
	result.Embedded, result.Chunks = stats.Documents, stats.Chunks
	result.Skipped, result.Stale = stats.Skipped, stats.Stale
	if err != nil {
		return result, fmt.Errorf("vector: build generation: %w", err)
	}
	remaining, err := ix.backlog(ctx, fingerprint)
	if err != nil {
		return result, err
	}
	if remaining == 0 {
		if err := ix.activate(ctx, fingerprint); err != nil {
			return result, err
		}
		result.Activated = true
	}
	return result, nil
}

func withDocumentRecipe(generation kitvec.Generation) kitvec.Generation {
	params := make(map[string]string, len(generation.Params)+4)
	maps.Copy(params, generation.Params)
	params["docbank_recipe"] = documentRecipe
	params["source"] = "plain-text"
	params["split_max_runes"] = strconv.Itoa(splitMaxRunes)
	params["split_overlap"] = strconv.Itoa(splitOverlap)
	generation.Params = params
	return generation
}

type progressStore struct {
	kitvec.Store[string, string]

	afterSave func()
}

func (s progressStore) SaveVectors(
	ctx context.Context, generation string, document string, revision any,
	vectors []kitvec.ChunkVector,
) error {
	if err := s.Store.SaveVectors(ctx, generation, document, revision, vectors); err != nil {
		return fmt.Errorf("vector: save progress document: %w", err)
	}
	if s.afterSave != nil {
		s.afterSave()
	}
	return nil
}

func (ix *Index) beginBuild() bool {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.running {
		return false
	}
	ix.running = true
	return true
}

func (ix *Index) endBuild() {
	ix.mu.Lock()
	ix.running = false
	ix.mu.Unlock()
}

func (ix *Index) refresh(ctx context.Context, source SourceFunc) (int, int, error) {
	seen := make(map[string]struct{})
	written := 0
	after := ""
	for {
		page, err := source(ctx, after, mirrorPageSize)
		if err != nil {
			return written, 0, fmt.Errorf("vector: refresh source: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for _, document := range page {
			if document.BlobHash <= after || document.ExtractorVersion < 1 {
				return written, 0, errors.New("vector: source returned an invalid or unordered document page")
			}
			after = document.BlobHash
			seen[document.BlobHash] = struct{}{}
			revision := sourceRevision(document)
			result, err := ix.db.ExecContext(ctx, `
				INSERT INTO document_mirror(blob_hash,content,source_revision)
				VALUES(?,?,?)
				ON CONFLICT(blob_hash) DO UPDATE SET
				  content=excluded.content,
				  source_revision=excluded.source_revision
				WHERE document_mirror.source_revision<>excluded.source_revision
				   OR document_mirror.content<>excluded.content`,
				document.BlobHash, document.Text, revision)
			if err != nil {
				return written, 0, fmt.Errorf("vector: mirror content %s: %w", document.BlobHash, err)
			}
			if changed, err := result.RowsAffected(); err == nil {
				written += int(changed)
			}
		}
	}
	rows, err := ix.db.QueryContext(ctx, `SELECT blob_hash FROM document_mirror ORDER BY blob_hash`)
	if err != nil {
		return written, 0, fmt.Errorf("vector: list mirror nodes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var stale []string
	for rows.Next() {
		var blobHash string
		if err := rows.Scan(&blobHash); err != nil {
			_ = rows.Close()
			return written, 0, fmt.Errorf("vector: list mirror nodes: %w", err)
		}
		if _, exists := seen[blobHash]; !exists {
			stale = append(stale, blobHash)
		}
	}
	if err := rows.Close(); err != nil {
		return written, 0, fmt.Errorf("vector: list mirror nodes: %w", err)
	}
	if err := rows.Err(); err != nil {
		return written, 0, fmt.Errorf("vector: list mirror nodes: %w", err)
	}
	for _, blobHash := range stale {
		if err := ix.store.DeleteVectors(ctx, blobHash); err != nil {
			return written, 0, fmt.Errorf("vector: remove content %s vectors: %w", blobHash, err)
		}
		if _, err := ix.db.ExecContext(ctx,
			`DELETE FROM document_mirror WHERE blob_hash=?`, blobHash); err != nil {
			return written, 0, fmt.Errorf("vector: remove mirror content %s: %w", blobHash, err)
		}
	}
	return written, len(stale), nil
}

func sourceRevision(document Document) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(strconv.Itoa(document.ExtractorVersion)))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(document.Text))
	return hex.EncodeToString(hash.Sum(nil))
}

func (ix *Index) ensureGeneration(
	ctx context.Context, fingerprint string, generation kitvec.Generation,
) error {
	state, err := ix.generationState(ctx, fingerprint)
	if err != nil {
		return err
	}
	if state != string(sqlitevec.StateActive) {
		if err := ix.store.EnsureGeneration(
			ctx, fingerprint, generation, sqlitevec.StateBuilding,
		); err != nil {
			return fmt.Errorf("vector: ensure generation: %w", err)
		}
	}
	if _, err := ix.db.ExecContext(ctx, `
		INSERT INTO embedding_generation_details(fingerprint,model)
		VALUES(?,?) ON CONFLICT(fingerprint) DO NOTHING`,
		fingerprint, generation.Model); err != nil {
		return fmt.Errorf("vector: record generation details: %w", err)
	}
	return nil
}

func (ix *Index) generationState(ctx context.Context, fingerprint string) (string, error) {
	var state string
	err := ix.db.QueryRowContext(ctx, `
		SELECT state FROM document_vectors_generations WHERE gen_key=?`, fingerprint).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("vector: read generation state: %w", err)
	}
	return state, nil
}

func (ix *Index) activate(ctx context.Context, fingerprint string) error {
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("vector: begin generation activation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `
		UPDATE document_vectors_generations
		SET state=CASE WHEN gen_key=? THEN 'active' ELSE 'retired' END`, fingerprint)
	if err != nil {
		return fmt.Errorf("vector: activate generation: %w", err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed == 0 {
		return fmt.Errorf("vector: generation %s was not found", fingerprint)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("vector: commit generation activation: %w", err)
	}
	return nil
}

func (ix *Index) backlog(ctx context.Context, fingerprint string) (int, error) {
	var count int
	err := ix.db.QueryRowContext(ctx, `
		SELECT count(*) FROM document_mirror m
		WHERE NOT EXISTS (
		  SELECT 1 FROM document_vectors_stamps s
		  JOIN document_vectors_generations g ON g.ordinal=s.ordinal
		  WHERE g.gen_key=? AND s.doc_key=m.blob_hash
		    AND s.revision IS m.source_revision
		)`, fingerprint).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("vector: count generation backlog: %w", err)
	}
	return count, nil
}

// Generations lists local generation coverage newest first.
func (ix *Index) Generations(ctx context.Context) ([]GenerationInfo, error) {
	rows, err := ix.db.QueryContext(ctx, `
		SELECT g.gen_key, d.model, g.dimension, g.state
		FROM document_vectors_generations g
		JOIN embedding_generation_details d ON d.fingerprint=g.gen_key
		ORDER BY g.ordinal DESC`)
	if err != nil {
		return nil, fmt.Errorf("vector: list generations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var items []GenerationInfo
	for rows.Next() {
		var item GenerationInfo
		if err := rows.Scan(
			&item.Fingerprint, &item.Model, &item.Dimensions, &item.State,
		); err != nil {
			return nil, fmt.Errorf("vector: list generations: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("vector: list generations: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("vector: list generations: %w", err)
	}
	for i := range items {
		embedded, skipped, pending, err := ix.coverage(ctx, items[i].Fingerprint)
		if err != nil {
			return nil, err
		}
		items[i].Embedded, items[i].Skipped, items[i].Pending = embedded, skipped, pending
	}
	return items, nil
}

func (ix *Index) coverage(ctx context.Context, fingerprint string) (int, int, int, error) {
	pending, err := ix.backlog(ctx, fingerprint)
	if err != nil {
		return 0, 0, 0, err
	}
	var total, embedded int
	if err := ix.db.QueryRowContext(ctx, `SELECT count(*) FROM document_mirror`).Scan(&total); err != nil {
		return 0, 0, 0, fmt.Errorf("vector: count mirror rows: %w", err)
	}
	if err := ix.db.QueryRowContext(ctx, `
		SELECT count(DISTINCT m.blob_hash)
		FROM document_mirror m
		JOIN document_vectors_stamps s ON s.doc_key=m.blob_hash AND s.revision IS m.source_revision
		JOIN document_vectors_generations g ON g.ordinal=s.ordinal AND g.gen_key=?
		JOIN document_vectors_chunks c ON c.ordinal=s.ordinal AND c.doc_key=s.doc_key`,
		fingerprint).Scan(&embedded); err != nil {
		return 0, 0, 0, fmt.Errorf("vector: count embedded rows: %w", err)
	}
	return embedded, max(total-pending-embedded, 0), pending, nil
}

// Path returns the sidecar path for diagnostics and tests.
func (ix *Index) Path() string { return ix.path }
