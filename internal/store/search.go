package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/docbank/document"
)

// SearchHit is a search result with its display path.
type SearchHit struct {
	Node  Node
	Path  string
	Match string
}

// SearchOptions narrows ranked search without changing its name-before-content
// ordering. TagID identifies one required assignment; MIMEType selects the
// current file version's parameter-free base media type; UnderNodeID selects
// descendants of one live directory. ModifiedSince is inclusive and
// ModifiedBefore is exclusive; both accept absolute RFC3339 timestamps.
type SearchOptions struct {
	TagID          string
	MIMEType       string
	UnderNodeID    int64
	ModifiedSince  string
	ModifiedBefore string
}

const (
	SearchMatchName    = "name"
	SearchMatchContent = "content"
)

// LexicalGeneration identifies one complete, immutable FTS projection. Rows
// remain unreachable until rendition and lexical heads are flipped together.
type LexicalGeneration struct {
	ID             string
	SegmentCount   int
	ManifestDigest string
	BuildCount     int
	BuildDigest    string
}

// ErrLexicalGenerationStale reports a complete immutable generation that no
// longer covers every rendition head current in the publication transaction.
// Callers may safely stage a fresh generation; no provider egress is needed.
var ErrLexicalGenerationStale = errors.New("lexical generation is stale")

// LexicalGenerationRoot is one exact immutable generation currently retained
// by in-process readers. Task 8 garbage collection consumes these roots in
// addition to the active database head.
type LexicalGenerationRoot struct {
	GenerationID string
	ReaderCount  int
}

// LexicalGenerationLease pins one exact generation until Release. Generation
// does not follow later head flips.
type LexicalGenerationLease struct {
	Generation LexicalGeneration
	store      *Store
	released   bool
}

// lexicalGenerationReaders tracks live reader leases per store. The mutex
// guards only the map and is held briefly, never while waiting on SQLite.
// Ordering against collection comes from SQLite itself: a standalone lease
// is pinned inside a write transaction, so it serializes with the
// publication or purge that snapshots pins and deletes generations, and a
// read-bound lease is pinned inside the read snapshot it serves.
var lexicalGenerationReaders = struct {
	sync.Mutex

	stores map[*Store]map[string]int
}{stores: make(map[*Store]map[string]int)}

// RecordRenditionBlob grants catalog authority to one verified Docbank blob
// receipt without creating a document version or conferring visibility.
func (s *Store) RecordRenditionBlob(
	ctx context.Context, hash string, size int64, physical BlobPhysical,
) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if err := s.EnsureBlobTx(tx, hash, size, physical); err != nil {
			return err
		}
		if physical.Created {
			if _, err := tx.ExecContext(ctx, `INSERT INTO rendition_blob_staging(blob_hash)
				VALUES(?) ON CONFLICT(blob_hash) DO NOTHING`, hash); err != nil {
				return fmt.Errorf("marking rendition blob %s as uncommitted staging: %w", hash, err)
			}
		}
		return nil
	})
}

// StageLexicalGeneration builds a complete unreachable FTS projection over
// every immutable rendition build currently staged in this vault.
func (s *Store) StageLexicalGeneration(
	ctx context.Context, generationID string,
) (LexicalGeneration, error) {
	if err := validateCatalogSHA256(generationID, "lexical generation ID"); err != nil {
		return LexicalGeneration{}, err
	}
	var generation LexicalGeneration
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var err error
		generation, err = stageLexicalGenerationTx(ctx, tx, generationID)
		return err
	})
	if err != nil {
		return LexicalGeneration{}, err
	}
	return generation, nil
}

// StageLexicalGenerationWithRoot atomically records a complete immutable
// projection and the exact fenced authority protecting it from maintenance.
func (s *Store) StageLexicalGenerationWithRoot(
	ctx context.Context, generationID string, root CurrentRenditionRoot,
) (LexicalGeneration, error) {
	if err := validateCatalogSHA256(generationID, "lexical generation ID"); err != nil {
		return LexicalGeneration{}, err
	}
	if err := validateCurrentRenditionRoot(root); err != nil {
		return LexicalGeneration{}, err
	}
	if root.TargetKind != RenditionRootLexicalGeneration || root.TargetID != generationID {
		return LexicalGeneration{}, errors.New(
			"rooted lexical generation requires a root for the staged generation")
	}
	var generation LexicalGeneration
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var err error
		generation, err = stageLexicalGenerationTx(ctx, tx, generationID)
		if err != nil {
			return err
		}
		return putCurrentRenditionRootTx(ctx, tx, root)
	})
	if err != nil {
		return LexicalGeneration{}, err
	}
	return generation, nil
}

func stageLexicalGenerationTx(
	ctx context.Context, tx *sql.Tx, generationID string,
) (LexicalGeneration, error) {
	segments, err := readCatalogLexicalManifestRowsTx(ctx, tx, "")
	if err != nil {
		return LexicalGeneration{}, err
	}
	buildIDs, err := lexicalCatalogBuildIDsTx(ctx, tx)
	if err != nil {
		return LexicalGeneration{}, err
	}
	return stageLexicalGenerationRowsTx(ctx, tx, generationID, segments, buildIDs)
}

func stageLexicalGenerationExcludingTx(
	ctx context.Context, tx *sql.Tx, excludedBuildIDs map[string]struct{},
) (LexicalGeneration, error) {
	segments, err := readCatalogLexicalManifestRowsTx(ctx, tx, "")
	if err != nil {
		return LexicalGeneration{}, err
	}
	segments = slices.DeleteFunc(segments, func(row lexicalManifestRow) bool {
		_, excluded := excludedBuildIDs[row.buildID]
		return excluded
	})
	buildIDs, err := lexicalCatalogBuildIDsTx(ctx, tx)
	if err != nil {
		return LexicalGeneration{}, err
	}
	buildIDs = slices.DeleteFunc(buildIDs, func(buildID string) bool {
		_, excluded := excludedBuildIDs[buildID]
		return excluded
	})
	if len(buildIDs) == 0 {
		return LexicalGeneration{}, nil
	}
	generationID := lexicalReplacementGenerationID(segments, buildIDs)
	return stageLexicalGenerationRowsTx(ctx, tx, generationID, segments, buildIDs)
}

func stageLexicalGenerationRowsTx(
	ctx context.Context, tx *sql.Tx, generationID string,
	segments []lexicalManifestRow, buildIDs []string,
) (LexicalGeneration, error) {
	stored, err := loadAndValidateLexicalGenerationTx(ctx, tx, generationID)
	if err == nil {
		return stored, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return LexicalGeneration{}, err
	}
	generation := LexicalGeneration{
		ID: generationID, SegmentCount: len(segments), ManifestDigest: lexicalManifestDigest(segments),
		BuildCount: len(buildIDs), BuildDigest: lexicalBuildDigest(buildIDs),
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rendition_lexical_generations(generation_id,segment_count,build_count,built_at)
		VALUES(?,?,?,?)`, generation.ID, generation.SegmentCount, generation.BuildCount, nowRFC3339(),
	); err != nil {
		return LexicalGeneration{}, fmt.Errorf("completing lexical generation %s: %w", generationID, err)
	}
	for _, buildID := range buildIDs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rendition_lexical_generation_builds(generation_id,build_id)
			VALUES(?,?)`, generationID, buildID); err != nil {
			return LexicalGeneration{}, fmt.Errorf(
				"recording lexical generation %s build %s: %w", generationID, buildID, err)
		}
	}
	if err := indexLexicalSegmentsTx(ctx, tx, segments); err != nil {
		return LexicalGeneration{}, fmt.Errorf("building lexical generation %s: %w", generationID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rendition_lexical_generation_manifests(
			generation_id,manifest_digest,build_digest
		) VALUES(?,?,?)`, generation.ID, generation.ManifestDigest, generation.BuildDigest,
	); err != nil {
		return LexicalGeneration{}, fmt.Errorf("recording lexical generation %s manifest: %w", generationID, err)
	}
	return loadAndValidateLexicalGenerationTx(ctx, tx, generationID)
}

func lexicalReplacementGenerationID(rows []lexicalManifestRow, buildIDs []string) string {
	digest := sha256.Sum256([]byte("docbank-lexical-gc-replacement/v1\x00" +
		lexicalManifestDigest(rows) + lexicalBuildDigest(buildIDs)))
	return hex.EncodeToString(digest[:])
}

type lexicalManifestRow struct {
	buildID   string
	segmentID string
	text      string
}

func readCatalogLexicalManifestRowsTx(
	ctx context.Context, tx metadataQuerier, buildID string,
) (_ []lexicalManifestRow, retErr error) {
	query := `
		SELECT s.build_id,s.segment_id,s.text,b.provider_operation_id
		FROM rendition_lexical_segments s
		JOIN rendition_builds b ON b.build_id=s.build_id
		ORDER BY s.build_id,s.segment_order,s.segment_id`
	var (
		rows *sql.Rows
		err  error
	)
	if buildID == "" {
		rows, err = tx.QueryContext(ctx, query)
	} else {
		rows, err = tx.QueryContext(ctx, `
			SELECT s.build_id,s.segment_id,s.text,b.provider_operation_id
			FROM rendition_lexical_segments s
			JOIN rendition_builds b ON b.build_id=s.build_id
			WHERE s.build_id=?
			ORDER BY s.build_id,s.segment_order,s.segment_id`, buildID)
	}
	if err != nil {
		return nil, fmt.Errorf("reading staged lexical manifest: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("closing staged lexical manifest: %w", err))
		}
	}()

	var result []lexicalManifestRow
	for rows.Next() {
		var (
			row               lexicalManifestRow
			providerOperation string
		)
		if err := rows.Scan(&row.buildID, &row.segmentID, &row.text, &providerOperation); err != nil {
			return nil, fmt.Errorf("reading staged lexical manifest row: %w", err)
		}
		if providerOperation == legacyPlainTextProvider && len(result) > 0 &&
			result[len(result)-1].buildID == row.buildID {
			result[len(result)-1].text += row.text
			continue
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading staged lexical manifest rows: %w", err)
	}
	return result, nil
}

// indexLexicalSegmentsTx adds segment text for builds that no generation has
// indexed yet. Builds are immutable, so rows already present are exact.
func indexLexicalSegmentsTx(
	ctx context.Context, tx *sql.Tx, segments []lexicalManifestRow,
) error {
	indexed := make(map[string]bool)
	for _, segment := range segments {
		done, known := indexed[segment.buildID]
		if !known {
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
				SELECT 1 FROM rendition_lexical_index WHERE build_id=?)`, segment.buildID,
			).Scan(&done); err != nil {
				return fmt.Errorf("checking lexical index for build %s: %w", segment.buildID, err)
			}
			indexed[segment.buildID] = done
		}
		if done {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rendition_lexical_index(build_id,segment_id,text) VALUES(?,?,?)`,
			segment.buildID, segment.segmentID, segment.text,
		); err != nil {
			return fmt.Errorf("indexing lexical segment %s of build %s: %w",
				segment.segmentID, segment.buildID, err)
		}
	}
	return nil
}

// publishLexicalHeadTx moves the lexical head to generationID, records the
// generation it replaced as superseded, and collects every superseded
// generation nothing can read any more. Running collection inside the flip
// keeps the projection proportional to the catalog instead of to the number
// of publications.
func (s *Store) publishLexicalHeadTx(ctx context.Context, tx *sql.Tx, generationID string) error {
	var previous string
	err := tx.QueryRowContext(ctx,
		`SELECT generation_id FROM rendition_lexical_heads WHERE singleton=1`).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reading current lexical head: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rendition_lexical_heads(singleton,generation_id) VALUES(1,?)
		ON CONFLICT(singleton) DO UPDATE SET generation_id=excluded.generation_id`,
		generationID); err != nil {
		return fmt.Errorf("publishing lexical head: %w", err)
	}
	if previous != "" && previous != generationID {
		if _, err := tx.ExecContext(ctx, `INSERT INTO rendition_lexical_superseded(generation_id)
			VALUES(?) ON CONFLICT(generation_id) DO NOTHING`, previous); err != nil {
			return fmt.Errorf("recording superseded lexical generation %s: %w", previous, err)
		}
	}
	_, err = s.collectSupersededLexicalGenerationsTx(ctx, tx)
	return err
}

// collectSupersededLexicalGenerationsTx removes every superseded generation
// nothing can read any more: not the head, not held by an active root, not
// pinned by a live reader lease, and not the staged generation of a rendition
// job. Index rows for builds that no remaining generation names go with them.
func (s *Store) collectSupersededLexicalGenerationsTx(
	ctx context.Context, tx *sql.Tx,
) (int, error) {
	candidates, err := stringColumnTx(ctx, tx, "superseded lexical generations", `
		SELECT g.generation_id FROM rendition_lexical_superseded g
		WHERE NOT EXISTS (SELECT 1 FROM rendition_lexical_heads h
		                  WHERE h.generation_id=g.generation_id)
		  AND NOT EXISTS (SELECT 1 FROM current_rendition_roots r
		                  WHERE r.target_kind='lexical_generation' AND r.target_id=g.generation_id
		                    AND r.active=1 AND (r.expires_at IS NULL OR r.expires_at>?))
		  AND NOT EXISTS (SELECT 1 FROM rendition_jobs j
		                  WHERE j.lexical_generation_id=g.generation_id)
		ORDER BY g.generation_id`, nowRFC3339())
	if err != nil {
		return 0, err
	}
	pinned := s.pinnedLexicalGenerationIDs()
	removed := 0
	for _, generationID := range candidates {
		if _, live := pinned[generationID]; live {
			continue
		}
		for _, statement := range []string{
			`DELETE FROM rendition_lexical_generation_manifests WHERE generation_id=?`,
			`DELETE FROM rendition_lexical_generations WHERE generation_id=?`,
		} {
			if _, err := tx.ExecContext(ctx, statement, generationID); err != nil {
				return removed, fmt.Errorf("collecting lexical generation %s: %w", generationID, err)
			}
		}
		removed++
	}
	if removed == 0 {
		return 0, nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM rendition_lexical_index
		WHERE NOT EXISTS (SELECT 1 FROM rendition_lexical_generation_builds gb
		                  WHERE gb.build_id=rendition_lexical_index.build_id)`); err != nil {
		return removed, fmt.Errorf("collecting unreferenced lexical index rows: %w", err)
	}
	return removed, nil
}

func (s *Store) pinnedLexicalGenerationIDs() map[string]struct{} {
	lexicalGenerationReaders.Lock()
	defer lexicalGenerationReaders.Unlock()
	pinned := make(map[string]struct{})
	for generationID, readers := range lexicalGenerationReaders.stores[s] {
		if readers > 0 {
			pinned[generationID] = struct{}{}
		}
	}
	return pinned
}

func readLexicalManifestRowsTx(
	ctx context.Context, tx metadataQuerier, generationID, buildID string,
) ([]lexicalManifestRow, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if buildID == "" {
		rows, err = tx.QueryContext(ctx, `
			SELECT i.build_id,i.segment_id,i.text
			FROM rendition_lexical_generation_builds gb
			JOIN rendition_lexical_index i ON i.build_id=gb.build_id
			WHERE gb.generation_id=?`, generationID)
	} else {
		rows, err = tx.QueryContext(ctx, `
			SELECT i.build_id,i.segment_id,i.text
			FROM rendition_lexical_generation_builds gb
			JOIN rendition_lexical_index i ON i.build_id=gb.build_id
			WHERE gb.generation_id=? AND gb.build_id=?`, generationID, buildID)
	}
	if err != nil {
		return nil, fmt.Errorf("reading lexical generation %s manifest: %w", generationID, err)
	}
	return scanLexicalManifestRows(rows, "lexical generation "+generationID+" manifest")
}

func scanLexicalManifestRows(
	rows *sql.Rows, description string,
) (_ []lexicalManifestRow, retErr error) {
	defer func() {
		if err := rows.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("closing %s: %w", description, err))
		}
	}()
	var result []lexicalManifestRow
	for rows.Next() {
		var row lexicalManifestRow
		if err := rows.Scan(&row.buildID, &row.segmentID, &row.text); err != nil {
			return nil, fmt.Errorf("reading %s row: %w", description, err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading %s rows: %w", description, err)
	}
	return result, nil
}

func lexicalManifestDigest(rows []lexicalManifestRow) string {
	ordered := append([]lexicalManifestRow(nil), rows...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].buildID != ordered[j].buildID {
			return ordered[i].buildID < ordered[j].buildID
		}
		if ordered[i].segmentID != ordered[j].segmentID {
			return ordered[i].segmentID < ordered[j].segmentID
		}
		return ordered[i].text < ordered[j].text
	})
	hash := sha256.New()
	for _, row := range ordered {
		for _, field := range [...]string{row.buildID, row.segmentID, row.text} {
			_, _ = io.WriteString(hash, strconv.Itoa(len(field)))
			_, _ = io.WriteString(hash, ":")
			_, _ = io.WriteString(hash, field)
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func lexicalCatalogBuildIDsTx(ctx context.Context, tx *sql.Tx) ([]string, error) {
	return lexicalBuildIDsTx(ctx, tx,
		`SELECT build_id FROM rendition_builds ORDER BY build_id`, nil)
}

func lexicalGenerationBuildIDsTx(
	ctx context.Context, tx metadataQuerier, generationID string,
) ([]string, error) {
	return lexicalBuildIDsTx(ctx, tx, `
		SELECT build_id FROM rendition_lexical_generation_builds
		WHERE generation_id=? ORDER BY build_id`, []any{generationID})
}

func lexicalBuildIDsTx(
	ctx context.Context, tx metadataQuerier, query string, args []any,
) (_ []string, retErr error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("reading lexical generation build membership: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf(
				"closing lexical generation build membership: %w", err))
		}
	}()
	var buildIDs []string
	for rows.Next() {
		var buildID string
		if err := rows.Scan(&buildID); err != nil {
			return nil, fmt.Errorf("scanning lexical generation build membership: %w", err)
		}
		buildIDs = append(buildIDs, buildID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading lexical generation build membership: %w", err)
	}
	return buildIDs, nil
}

func lexicalBuildDigest(buildIDs []string) string {
	ordered := append([]string(nil), buildIDs...)
	sort.Strings(ordered)
	hash := sha256.New()
	for _, buildID := range ordered {
		_, _ = io.WriteString(hash, strconv.Itoa(len(buildID)))
		_, _ = io.WriteString(hash, ":")
		_, _ = io.WriteString(hash, buildID)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func loadAndValidateLexicalGenerationTx(
	ctx context.Context, tx *sql.Tx, generationID string,
) (LexicalGeneration, error) {
	generation := LexicalGeneration{ID: generationID}
	var manifestDigest, buildDigest sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT g.segment_count,g.build_count,m.manifest_digest,m.build_digest
		FROM rendition_lexical_generations g
		LEFT JOIN rendition_lexical_generation_manifests m
		  ON m.generation_id=g.generation_id
		WHERE g.generation_id=?`, generationID,
	).Scan(&generation.SegmentCount, &generation.BuildCount, &manifestDigest, &buildDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return LexicalGeneration{}, ErrNotFound
	}
	if err != nil {
		return LexicalGeneration{}, fmt.Errorf("reading lexical generation %s: %w", generationID, err)
	}
	segments, err := readLexicalManifestRowsTx(ctx, tx, generationID, "")
	if err != nil {
		return LexicalGeneration{}, err
	}
	buildIDs, err := lexicalGenerationBuildIDsTx(ctx, tx, generationID)
	if err != nil {
		return LexicalGeneration{}, err
	}
	if !manifestDigest.Valid || !buildDigest.Valid ||
		len(segments) != generation.SegmentCount ||
		len(buildIDs) != generation.BuildCount ||
		lexicalManifestDigest(segments) != manifestDigest.String ||
		lexicalBuildDigest(buildIDs) != buildDigest.String {
		return LexicalGeneration{}, fmt.Errorf(
			"lexical generation %s has a different immutable manifest", generationID)
	}
	generation.ManifestDigest = manifestDigest.String
	generation.BuildDigest = buildDigest.String
	return generation, nil
}

// ActiveLexicalGeneration returns the exact complete projection selected by
// the lexical head. Call AcquireLexicalGeneration when the generation must
// remain rooted after this lookup returns.
func (s *Store) ActiveLexicalGeneration(ctx context.Context) (LexicalGeneration, error) {
	return readActiveLexicalGeneration(ctx, s.db)
}

func readActiveLexicalGeneration(
	ctx context.Context, queryer rowQuerier,
) (LexicalGeneration, error) {
	var generation LexicalGeneration
	err := queryer.QueryRowContext(ctx, `
		SELECT g.generation_id,g.segment_count,m.manifest_digest,g.build_count,m.build_digest
		FROM rendition_lexical_heads h
		JOIN rendition_lexical_generations g ON g.generation_id=h.generation_id
		JOIN rendition_lexical_generation_manifests m ON m.generation_id=g.generation_id
		WHERE h.singleton=1`).Scan(
		&generation.ID, &generation.SegmentCount, &generation.ManifestDigest,
		&generation.BuildCount, &generation.BuildDigest,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return LexicalGeneration{}, ErrNotFound
	}
	if err != nil {
		return LexicalGeneration{}, fmt.Errorf("reading active lexical generation: %w", err)
	}
	return generation, nil
}

// AcquireLexicalGeneration pins the exact generation selected by the current
// lexical head. The caller must release the returned lease. The pin is taken
// inside a write transaction so it cannot interleave with a publication or
// purge that is deciding which generations nothing reads any more.
func (s *Store) AcquireLexicalGeneration(ctx context.Context) (*LexicalGenerationLease, error) {
	var lease *LexicalGenerationLease
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var err error
		lease, err = s.acquireLexicalGeneration(ctx, tx)
		return err
	})
	if err != nil {
		return nil, err
	}
	return lease, nil
}

func (s *Store) acquireLexicalGeneration(
	ctx context.Context, queryer rowQuerier,
) (*LexicalGenerationLease, error) {
	generation, err := readActiveLexicalGeneration(ctx, queryer)
	if err != nil {
		return nil, err
	}
	lexicalGenerationReaders.Lock()
	defer lexicalGenerationReaders.Unlock()
	readers := lexicalGenerationReaders.stores[s]
	if readers == nil {
		readers = make(map[string]int)
		lexicalGenerationReaders.stores[s] = readers
	}
	readers[generation.ID]++
	return &LexicalGenerationLease{Generation: generation, store: s}, nil
}

func (s *Store) withLexicalGenerationRead(
	ctx context.Context, fn func(queryer metadataQuerier, generation LexicalGeneration) error,
) (retErr error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquiring lexical generation connection: %w", err)
	}
	active := false
	var lease *LexicalGenerationLease
	defer func() {
		if active {
			_, err := conn.ExecContext(context.Background(), "ROLLBACK")
			retErr = errors.Join(retErr, err)
		}
		if lease != nil {
			retErr = errors.Join(retErr, lease.Release())
		}
		retErr = errors.Join(retErr, conn.Close())
	}()
	if _, err := conn.ExecContext(ctx, "BEGIN DEFERRED"); err != nil {
		return fmt.Errorf("starting lexical generation read: %w", err)
	}
	active = true
	lease, err = s.acquireLexicalGeneration(ctx, conn)
	if err != nil {
		return err
	}
	if err := fn(conn, lease.Generation); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("committing lexical generation read: %w", err)
	}
	active = false
	return nil
}

// Release removes this lease's generation root. Repeated release is safe.
func (l *LexicalGenerationLease) Release() error {
	if l == nil {
		return nil
	}
	lexicalGenerationReaders.Lock()
	defer lexicalGenerationReaders.Unlock()
	if l.released {
		return nil
	}
	l.released = true
	readers := lexicalGenerationReaders.stores[l.store]
	readers[l.Generation.ID]--
	if readers[l.Generation.ID] == 0 {
		delete(readers, l.Generation.ID)
	}
	if len(readers) == 0 {
		delete(lexicalGenerationReaders.stores, l.store)
	}
	return nil
}

// LeasedLexicalGenerationRoots returns a deterministic snapshot of exact
// generations pinned by this store's current readers.
func (s *Store) LeasedLexicalGenerationRoots() []LexicalGenerationRoot {
	lexicalGenerationReaders.Lock()
	defer lexicalGenerationReaders.Unlock()

	readers := lexicalGenerationReaders.stores[s]
	roots := make([]LexicalGenerationRoot, 0, len(readers))
	for generationID, readerCount := range readers {
		roots = append(roots, LexicalGenerationRoot{
			GenerationID: generationID,
			ReaderCount:  readerCount,
		})
	}
	sort.Slice(roots, func(i, j int) bool {
		return roots[i].GenerationID < roots[j].GenerationID
	})
	return roots
}

// PublishRenditionAndLexicalHeads inserts one version-scoped attachment and
// flips its rendition head together with the complete lexical generation.
// Any failure rolls back all three visibility changes.
func (s *Store) PublishRenditionAndLexicalHeads(
	ctx context.Context, attachment RenditionAttachmentRecord,
	head RenditionHeadRecord, generationID string,
) error {
	return s.publishRenditionAndLexicalHeads(ctx, attachment, head, generationID, nil)
}

// PublishAuthorizedRenditionAndLexicalHeads binds the verified provider
// operation and exact retained outputs to a grant in the transaction that
// makes its rendition and lexical heads visible.
func (s *Store) PublishAuthorizedRenditionAndLexicalHeads(
	ctx context.Context, attachment RenditionAttachmentRecord,
	head RenditionHeadRecord, generationID string,
	authorization ProviderOperationAuthorizationRequest,
	operation document.RenditionAuthorization,
) error {
	if authorization.PriorAuthorization == nil {
		return fmt.Errorf("publishing authorized rendition: %w", ErrProcessingConsentRequired)
	}
	operationChecksum, err := operation.Fingerprint()
	if err != nil {
		return fmt.Errorf("fingerprinting rendition authorization: %w", err)
	}
	return s.publishRenditionAndLexicalHeads(
		ctx, attachment, head, generationID, &providerPublicationAuthorization{
			consent: authorization, operationChecksum: operationChecksum,
			inputClass: string(operation.InputKind), sourceSHA256: operation.SourceSHA256,
			renditionRequestFingerprint: operation.RenditionRequestFingerprint,
		},
	)
}

type providerPublicationAuthorization struct {
	consent                     ProviderOperationAuthorizationRequest
	operationChecksum           string
	inputClass                  string
	sourceSHA256                string
	renditionRequestFingerprint string
}

func (s *Store) publishRenditionAndLexicalHeads(
	ctx context.Context, attachment RenditionAttachmentRecord,
	head RenditionHeadRecord, generationID string,
	authorization *providerPublicationAuthorization,
) error {
	normalized, err := normalizeRenditionAttachmentRecord(attachment)
	if err != nil {
		return fmt.Errorf("publishing rendition attachment: %w", err)
	}
	if err := validateRenditionHeadRecord(head); err != nil {
		return fmt.Errorf("publishing rendition head: %w", err)
	}
	if err := validateCatalogSHA256(generationID, "lexical generation ID"); err != nil {
		return err
	}
	if head.ContentVersionID != normalized.ContentVersionID ||
		head.ProcessingProfileFingerprint != normalized.Profile.Fingerprint ||
		head.AttachmentID != normalized.ID {
		return errors.New("rendition head does not resolve through its exact attachment")
	}
	if normalized.VaultID != s.vaultID {
		return fmt.Errorf("publishing rendition attachment: vault %q does not match store vault %q",
			normalized.VaultID, s.vaultID)
	}
	if authorization != nil &&
		(authorization.consent.ProfileFingerprint != normalized.Profile.Fingerprint ||
			authorization.consent.DisclosureFingerprint != normalized.Profile.RenditionDisclosureFingerprint) {
		return errors.New("provider authorization does not match rendition publication policy")
	}

	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		return s.publishRenditionAttachmentsAndLexicalHeadsTx(ctx, tx,
			[]renditionPublicationPair{{attachment: normalized, head: head}}, generationID, authorization)
	})
}

type renditionPublicationPair struct {
	attachment RenditionAttachmentRecord
	head       RenditionHeadRecord
}

// publishRenditionAttachmentsAndLexicalHeadsTx is the multi-waiter form used
// by durable rendition jobs. Every attachment/head pair and the lexical head
// become visible in this caller-owned transaction or none of them do.
func (s *Store) publishRenditionAttachmentsAndLexicalHeadsTx(
	ctx context.Context, tx *sql.Tx, pairs []renditionPublicationPair, generationID string,
	authorization *providerPublicationAuthorization,
) error {
	if len(pairs) == 0 {
		return errors.New("rendition publication requires at least one attachment")
	}
	if err := validateCatalogSHA256(generationID, "lexical generation ID"); err != nil {
		return err
	}
	for _, pair := range pairs {
		normalized, err := normalizeRenditionAttachmentRecord(pair.attachment)
		if err != nil {
			return fmt.Errorf("publishing rendition attachment: %w", err)
		}
		if err := validateRenditionHeadRecord(pair.head); err != nil {
			return fmt.Errorf("publishing rendition head: %w", err)
		}
		if pair.head.ContentVersionID != normalized.ContentVersionID ||
			pair.head.ProcessingProfileFingerprint != normalized.Profile.Fingerprint ||
			pair.head.AttachmentID != normalized.ID {
			return errors.New("rendition head does not resolve through its exact attachment")
		}
		if err := ensureProcessingProfileTx(ctx, tx, normalized.Profile); err != nil {
			return err
		}
		build, err := validateRenditionPublicationTx(ctx, tx, normalized, generationID, authorization)
		if err != nil {
			return err
		}
		if authorization != nil {
			if err := s.authorizeRenditionPublicationTx(ctx, tx, build, authorization); err != nil {
				return err
			}
		}
		if err := insertRenditionAttachmentAndHeadTx(ctx, tx, normalized, pair.head); err != nil {
			return err
		}
	}
	if _, err := loadAndValidateLexicalGenerationTx(ctx, tx, generationID); errors.Is(err, ErrNotFound) {
		return fmt.Errorf("lexical generation %s: %w", generationID, ErrNotFound)
	} else if err != nil {
		return err
	}
	if err := validateLexicalGenerationCoversCurrentHeadsTx(ctx, tx, generationID); err != nil {
		return err
	}
	return s.publishLexicalHeadTx(ctx, tx, generationID)
}

// validateRenditionPublicationTx proves that one attachment names an exact
// build whose source, profile, artifacts, and lexical rows all agree with the
// catalog and the generation about to become the head.
func validateRenditionPublicationTx(
	ctx context.Context, tx *sql.Tx, normalized RenditionAttachmentRecord, generationID string,
	authorization *providerPublicationAuthorization,
) (RenditionBuildRecord, error) {
	build, err := loadRenditionBuild(ctx, tx, normalized.BuildID)
	if err != nil {
		return build, fmt.Errorf("reading rendition build %s: %w", normalized.BuildID, err)
	}
	if build.VaultID != normalized.VaultID {
		return build, errors.New("rendition attachment and build belong to different vaults")
	}
	if build.RenditionRequestFingerprint != normalized.Profile.RenditionRequestFingerprint ||
		build.EvidenceLexicalFingerprint != normalized.Profile.EvidenceLexicalFingerprint {
		return build, errors.New("rendition attachment profile does not match build component identity")
	}
	if authorization != nil &&
		(authorization.operationChecksum != build.AuthorizationChecksum ||
			authorization.sourceSHA256 != build.SourceSHA256 ||
			authorization.renditionRequestFingerprint != build.RenditionRequestFingerprint) {
		return build, errors.New("provider authorization does not match rendition build operation")
	}
	if err := validateRenditionArtifactRolesForProfile(normalized.Profile, build); err != nil {
		return build, err
	}
	var sourceSHA256 string
	if err := tx.QueryRowContext(ctx,
		`SELECT blob_hash FROM content_versions WHERE version_id=?`, normalized.ContentVersionID,
	).Scan(&sourceSHA256); errors.Is(err, sql.ErrNoRows) {
		return build, fmt.Errorf("content version %s: %w", normalized.ContentVersionID, ErrNotFound)
	} else if err != nil {
		return build, fmt.Errorf("reading content version %s: %w", normalized.ContentVersionID, err)
	}
	if sourceSHA256 != build.SourceSHA256 {
		return build, errors.New("rendition attachment source does not match content version")
	}
	suppressed, err := derivativeAttachmentSuppressedTx(
		ctx, tx, build.SourceSHA256, normalized.ContentVersionID,
		normalized.Profile.Fingerprint, build.ID)
	if err != nil {
		return build, fmt.Errorf("checking rendition attachment purge suppression: %w", err)
	}
	if suppressed {
		return build, fmt.Errorf("rendition attachment for build %s has an active purge suppression", build.ID)
	}
	if err := validateRenditionBuildStateTx(ctx, tx, build.ID); err != nil {
		return build, err
	}
	var containsBuild bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM rendition_lexical_generation_builds
		WHERE generation_id=? AND build_id=?
	)`, generationID, build.ID).Scan(&containsBuild); err != nil {
		return build, fmt.Errorf("checking lexical generation %s build membership: %w", generationID, err)
	}
	if !containsBuild {
		return build, fmt.Errorf("lexical generation %s does not exactly contain build %s",
			generationID, build.ID)
	}
	expectedBuildRows, err := readCatalogLexicalManifestRowsTx(ctx, tx, build.ID)
	if err != nil {
		return build, err
	}
	indexedBuildRows, err := readLexicalManifestRowsTx(ctx, tx, generationID, build.ID)
	if err != nil {
		return build, err
	}
	if len(indexedBuildRows) != len(expectedBuildRows) ||
		lexicalManifestDigest(indexedBuildRows) != lexicalManifestDigest(expectedBuildRows) {
		return build, fmt.Errorf("lexical generation %s does not exactly contain build %s",
			generationID, build.ID)
	}
	return build, nil
}

// authorizeRenditionPublicationTx binds a provider-authorized publication to
// the consent that admitted the provider call and records the operation.
func (s *Store) authorizeRenditionPublicationTx(
	ctx context.Context, tx *sql.Tx, build RenditionBuildRecord,
	authorization *providerPublicationAuthorization,
) error {
	authority, err := normalizeConsentAuthority(authorization.consent)
	if err != nil {
		return err
	}
	if !slices.Equal(authority.inputs, []string{authorization.inputClass}) {
		return errors.New("processing consent input classes do not match rendition build input")
	}
	retained := make([]string, len(build.Artifacts))
	for index, artifact := range build.Artifacts {
		retained[index] = artifact.Role
	}
	slices.Sort(retained)
	retained = slices.Compact(retained)
	if !slices.Equal(authority.retained, retained) {
		return errors.New("processing consent retained artifact classes do not match rendition build artifacts")
	}
	if _, err := s.authorizeProviderOperationTx(
		ctx, tx, authorization.consent, time.Now().UTC(),
	); err != nil {
		return fmt.Errorf("authorizing rendition publication: %w", err)
	}
	return nil
}

// insertRenditionAttachmentAndHeadTx records the immutable attachment, or
// proves an existing one names the same metadata, then moves the head.
func insertRenditionAttachmentAndHeadTx(
	ctx context.Context, tx *sql.Tx, normalized RenditionAttachmentRecord, head RenditionHeadRecord,
) error {
	result, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO rendition_attachments(
				attachment_id,vault_uid,content_version_id,build_id,profile_fingerprint,
				retention_disclosure_fingerprint,attachment_policy_fingerprint,
				consent_fingerprint,rendition_disclosure_fingerprint,trust_boundary,attached_at
			) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		normalized.ID, normalized.VaultID, normalized.ContentVersionID, normalized.BuildID,
		normalized.Profile.Fingerprint, normalized.Profile.RetentionDisclosureFingerprint,
		normalized.Profile.AttachmentPolicyFingerprint, normalized.Profile.ConsentFingerprint,
		normalized.Profile.RenditionDisclosureFingerprint, normalized.Profile.TrustBoundary,
		normalized.AttachedAt)
	if err != nil {
		return fmt.Errorf("inserting rendition attachment %s: %w", normalized.ID, err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking rendition attachment %s insertion: %w", normalized.ID, err)
	}
	if inserted == 0 {
		stored, err := loadRenditionAttachment(ctx, tx, normalized.ID)
		if err != nil {
			return fmt.Errorf("reading rendition attachment %s: %w", normalized.ID, err)
		}
		// A deterministic attachment may be republished after a later head
		// superseded it. The first observation keeps its timestamp.
		normalized.AttachedAt = stored.AttachedAt
		if !reflect.DeepEqual(stored, normalized) {
			return fmt.Errorf("rendition attachment %s names different immutable metadata", normalized.ID)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rendition_heads(content_version_id,profile_fingerprint,attachment_id,published_at)
		VALUES(?,?,?,?)
		ON CONFLICT(content_version_id,profile_fingerprint) DO UPDATE SET
			attachment_id=excluded.attachment_id,published_at=excluded.published_at`,
		head.ContentVersionID, head.ProcessingProfileFingerprint,
		head.AttachmentID, head.PublishedAt); err != nil {
		return fmt.Errorf("publishing rendition head: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM embedding_heads
		WHERE content_version_id=? AND profile_fingerprint=? AND EXISTS (
			SELECT 1 FROM embedding_sets s
			JOIN embedding_input_generations g ON g.generation_id=s.input_generation_id
			WHERE s.embedding_set_id=embedding_heads.embedding_set_id
			  AND g.attachment_id IS NOT NULL AND g.attachment_id<>?
		)`, head.ContentVersionID, head.ProcessingProfileFingerprint, head.AttachmentID); err != nil {
		return fmt.Errorf("revoking stale chunk embedding heads: %w", err)
	}
	return nil
}

func validateLexicalGenerationCoversCurrentHeadsTx(
	ctx context.Context, tx metadataQuerier, generationID string,
) error {
	buildIDs, err := currentRenditionHeadBuildIDsTx(ctx, tx)
	if err != nil {
		return err
	}
	generationBuildIDs, err := lexicalGenerationBuildIDsTx(ctx, tx, generationID)
	if err != nil {
		return err
	}
	for _, buildID := range buildIDs {
		if !slices.Contains(generationBuildIDs, buildID) {
			return fmt.Errorf("lexical generation %s does not cover published rendition build %s (current rendition head build): %w",
				generationID, buildID, ErrLexicalGenerationStale)
		}
		expected, err := readCatalogLexicalManifestRowsTx(ctx, tx, buildID)
		if err != nil {
			return err
		}
		indexed, err := readLexicalManifestRowsTx(ctx, tx, generationID, buildID)
		if err != nil {
			return err
		}
		if len(indexed) != len(expected) || lexicalManifestDigest(indexed) != lexicalManifestDigest(expected) {
			return fmt.Errorf("lexical generation %s does not exactly contain current rendition head build %s",
				generationID, buildID)
		}
	}
	return nil
}

func currentRenditionHeadBuildIDsTx(
	ctx context.Context, tx metadataQuerier,
) (_ []string, retErr error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT a.build_id
		FROM rendition_heads h
		JOIN rendition_attachments a ON a.attachment_id=h.attachment_id
		ORDER BY a.build_id`)
	if err != nil {
		return nil, fmt.Errorf("reading current rendition head builds: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	var buildIDs []string
	for rows.Next() {
		var buildID string
		if err := rows.Scan(&buildID); err != nil {
			return nil, fmt.Errorf("scanning current rendition head build: %w", err)
		}
		buildIDs = append(buildIDs, buildID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading current rendition head builds: %w", err)
	}
	return buildIDs, nil
}

// ftsQuery converts free-form user input into a safe FTS5 query: each
// whitespace-separated term becomes a quoted prefix term. Embedded double
// quotes are doubled per FTS5 string syntax.
func ftsQuery(input string) string {
	var terms []string
	for t := range strings.FieldsSeq(input) {
		t = strings.ReplaceAll(t, `"`, `""`)
		terms = append(terms, `"`+t+`"*`)
	}
	return strings.Join(terms, " ")
}

// SearchPage returns live name matches in their established order, followed
// by content-only matches. Keeping the two ranks separate preserves the
// deterministic name-search contract: enabling extraction never reorders or
// hides a filename match that the same limit returned before.
func (s *Store) SearchPage(ctx context.Context, query string, limit int) ([]SearchHit, bool, error) {
	return s.SearchPageWithOptions(ctx, query, limit, SearchOptions{})
}

// SearchPageWithOptions returns ranked live matches that satisfy every
// requested filter. Filters apply equally to name and content candidates.
func (s *Store) SearchPageWithOptions(
	ctx context.Context, query string, limit int, opts SearchOptions,
) ([]SearchHit, bool, error) {
	if limit <= 0 {
		limit = 50
	}
	if opts.TagID != "" {
		if _, err := s.TagByID(ctx, opts.TagID); err != nil {
			return nil, false, fmt.Errorf("search tag %q: %w", opts.TagID, err)
		}
	}
	normalizedMIME, err := NormalizeSearchMIMEType(opts.MIMEType)
	if err != nil {
		return nil, false, err
	}
	opts.MIMEType = normalizedMIME
	if opts.UnderNodeID < 0 {
		return nil, false, errors.New("search directory node ID must be positive")
	}
	if opts.UnderNodeID != 0 {
		directory, err := s.NodeByID(ctx, opts.UnderNodeID)
		if err != nil {
			return nil, false, fmt.Errorf("search directory node %d: %w", opts.UnderNodeID, err)
		}
		if directory.TrashedAt != nil {
			return nil, false, fmt.Errorf("search directory node %d is trashed: %w",
				opts.UnderNodeID, ErrNotFound)
		}
		if !directory.IsDir() {
			return nil, false, fmt.Errorf("search scope node %d: %w", opts.UnderNodeID, ErrNotDir)
		}
	}
	modifiedSince, modifiedBefore, err := NormalizeSearchTimeBounds(
		opts.ModifiedSince, opts.ModifiedBefore,
	)
	if err != nil {
		return nil, false, err
	}
	opts.ModifiedSince = modifiedSince
	opts.ModifiedBefore = modifiedBefore
	fq := ftsQuery(query)
	if fq == "" {
		return nil, false, nil
	}
	filterSQL, filterArgs := searchFilterSQL(opts)
	nameArgs := []any{fq}
	nameArgs = append(nameArgs, filterArgs...)
	nameArgs = append(nameArgs, fq, limit+1)
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+nodeCols+`
		FROM `+nodeFrom+`
		WHERE n.id IN (SELECT rowid FROM nodes_fts WHERE nodes_fts MATCH ?)
		  AND n.trashed_at IS NULL
		  `+filterSQL+`
		ORDER BY (SELECT rank FROM nodes_fts WHERE rowid = n.id AND nodes_fts MATCH ?),
		         n.name, n.id
		LIMIT ?`, nameArgs...)
	if err != nil {
		return nil, false, fmt.Errorf("searching %q: %w", query, err)
	}
	nameHits, err := scanSearchRows(rows, SearchMatchName, query)
	if err != nil {
		return nil, false, err
	}
	if len(nameHits) > limit {
		nameHits = nameHits[:limit]
		if err := s.addSearchPaths(ctx, nameHits); err != nil {
			return nil, false, err
		}
		return nameHits, true, nil
	}

	// Content may also match a node already returned by name. Over-fetch by
	// the complete name set so duplicate filtering cannot conceal truncation.
	remaining := limit - len(nameHits)
	var contentHits []SearchHit
	queryContent := func(queryer metadataQuerier, generationID string) error {
		contentArgs := []any{fq}
		contentQuery := `
			WITH matched_blobs AS (
			  SELECT blob_hash, MIN(rank) AS best_rank
			  FROM content_fts WHERE content_fts MATCH ?
			  GROUP BY blob_hash
			)
			SELECT ` + nodeCols + `
			FROM ` + nodeFrom + `
			JOIN matched_blobs mb ON mb.blob_hash = cv.blob_hash
			JOIN text_searchable_versions tsv ON tsv.version_id = cv.version_id
			WHERE n.trashed_at IS NULL
			  ` + filterSQL + `
			ORDER BY mb.best_rank, n.name, n.id
			LIMIT ?`
		if generationID != "" {
			// Selection, attachment resolution, and row consumption share
			// this reader's one immutable publication snapshot. Once a
			// lexical head exists, legacy content_fts is a non-serving cache.
			contentQuery = `
				WITH matched_versions(version_id,best_rank) AS (
				  SELECT a.content_version_id, MIN(rendition_lexical_fts.rank)
				  FROM rendition_lexical_fts
				  JOIN rendition_attachments a ON a.build_id=rendition_lexical_fts.build_id
				  JOIN rendition_heads rh
				    ON rh.content_version_id=a.content_version_id
				   AND rh.profile_fingerprint=a.profile_fingerprint
				   AND rh.attachment_id=a.attachment_id
				  WHERE rendition_lexical_fts MATCH ?
				    AND EXISTS (SELECT 1 FROM rendition_lexical_generation_builds gb
				                WHERE gb.generation_id=? AND gb.build_id=rendition_lexical_fts.build_id)
				  GROUP BY a.content_version_id
				)
				SELECT ` + nodeCols + `
				FROM ` + nodeFrom + `
				JOIN matched_versions mv ON mv.version_id=cv.version_id
				WHERE n.trashed_at IS NULL
				  ` + filterSQL + `
				ORDER BY mv.best_rank,n.name,n.id
				LIMIT ?`
			contentArgs = append(contentArgs, generationID)
		}
		contentArgs = append(contentArgs, filterArgs...)
		contentArgs = append(contentArgs, remaining+len(nameHits)+1)
		rows, err := queryer.QueryContext(ctx, contentQuery, contentArgs...)
		if err != nil {
			return fmt.Errorf("searching extracted content for %q: %w", query, err)
		}
		contentHits, err = scanSearchRows(rows, SearchMatchContent, query)
		return err
	}
	err = s.withLexicalGenerationRead(ctx, func(
		queryer metadataQuerier, generation LexicalGeneration,
	) error {
		return queryContent(queryer, generation.ID)
	})
	if errors.Is(err, ErrNotFound) {
		err = queryContent(s.db, "")
	}
	if err != nil {
		return nil, false, err
	}
	seen := make(map[int64]struct{}, len(nameHits))
	for _, hit := range nameHits {
		seen[hit.Node.ID] = struct{}{}
	}
	filtered := contentHits[:0]
	for _, hit := range contentHits {
		if _, exists := seen[hit.Node.ID]; exists {
			continue
		}
		filtered = append(filtered, hit)
	}
	truncated := len(filtered) > remaining
	if truncated {
		filtered = filtered[:remaining]
	}
	hits := make([]SearchHit, 0, len(nameHits)+len(filtered))
	hits = append(hits, nameHits...)
	hits = append(hits, filtered...)
	if err := s.addSearchPaths(ctx, hits); err != nil {
		return nil, false, err
	}
	return hits, truncated, nil
}

func searchFilterSQL(opts SearchOptions) (string, []any) {
	var (
		clauses []string
		args    []any
	)
	if opts.TagID != "" {
		clauses = append(clauses, `AND EXISTS (
			SELECT 1 FROM node_tags nt WHERE nt.node_id=n.id AND nt.tag_id=?
		)`)
		args = append(args, opts.TagID)
	}
	if opts.MIMEType != "" {
		clauses = append(clauses, `AND lower(trim(CASE
			WHEN instr(cv.mime_type, ';')=0 THEN cv.mime_type
			ELSE substr(cv.mime_type, 1, instr(cv.mime_type, ';')-1)
		END))=?`)
		args = append(args, opts.MIMEType)
	}
	if opts.UnderNodeID != 0 {
		clauses = append(clauses, `AND n.id IN (
			WITH RECURSIVE descendants(id) AS (
				SELECT id FROM nodes WHERE parent_id=?
				UNION ALL
				SELECT child.id FROM nodes child
				JOIN descendants parent ON child.parent_id=parent.id
			)
			SELECT id FROM descendants
		)`)
		args = append(args, opts.UnderNodeID)
	}
	if opts.ModifiedSince != "" {
		clauses = append(clauses, `AND n.modified_at>=?`)
		args = append(args, opts.ModifiedSince)
	}
	if opts.ModifiedBefore != "" {
		clauses = append(clauses, `AND n.modified_at<?`)
		args = append(args, opts.ModifiedBefore)
	}
	return strings.Join(clauses, "\n"), args
}

// NormalizeSearchTimeBounds accepts optional absolute RFC3339 timestamps and
// returns canonical UTC bounds. The half-open interval makes adjacent searches
// compose without duplicate boundary results.
func NormalizeSearchTimeBounds(modifiedSince, modifiedBefore string) (string, string, error) {
	since, err := normalizeSearchTimestamp("modified_since", modifiedSince)
	if err != nil {
		return "", "", err
	}
	before, err := normalizeSearchTimestamp("modified_before", modifiedBefore)
	if err != nil {
		return "", "", err
	}
	if since != "" && before != "" && since >= before {
		return "", "", errors.New("modified_since must be earlier than modified_before")
	}
	return since, before, nil
}

func normalizeSearchTimestamp(field, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", fmt.Errorf("%s %q must be an absolute RFC3339 timestamp: %w", field, value, err)
	}
	return parsed.UTC().Format(timestampLayout), nil
}

// NormalizeSearchMIMEType accepts one parameter-free media type and returns
// its canonical base spelling. Stored parameters do not participate in search
// filtering because they describe representation details, not the format.
func NormalizeSearchMIMEType(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	mediaType, params, err := mime.ParseMediaType(value)
	if err != nil {
		return "", fmt.Errorf("search MIME type %q is invalid: %w", value, err)
	}
	if len(params) != 0 {
		return "", fmt.Errorf(
			"search MIME type %q must not include parameters; use %q", value, mediaType,
		)
	}
	if strings.Contains(mediaType, "*") {
		return "", fmt.Errorf("search MIME type %q must not contain wildcards", value)
	}
	return mediaType, nil
}

func scanSearchRows(rows *sql.Rows, match, query string) ([]SearchHit, error) {
	defer func() { _ = rows.Close() }()
	var hits []SearchHit
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		hits = append(hits, SearchHit{Node: n, Match: match})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("searching %q: %w", query, err)
	}
	return hits, nil
}

func (s *Store) addSearchPaths(ctx context.Context, hits []SearchHit) error {
	for i := range hits {
		p, err := s.Path(ctx, hits[i].Node.ID)
		if err != nil {
			return err
		}
		hits[i].Path = p
	}
	return nil
}
