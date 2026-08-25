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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
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
}

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

var lexicalGenerationReaders = struct {
	sync.Mutex

	stores map[*Store]map[string]int
}{stores: make(map[*Store]map[string]int)}

const lexicalProjectionSchema = `
CREATE TABLE IF NOT EXISTS rendition_lexical_generations (
    generation_id TEXT PRIMARY KEY,
    segment_count INTEGER NOT NULL CHECK (segment_count >= 0),
    built_at      TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS rendition_lexical_generation_manifests (
    generation_id  TEXT PRIMARY KEY REFERENCES rendition_lexical_generations(generation_id),
    manifest_digest TEXT NOT NULL CHECK (length(manifest_digest) = 64)
);
CREATE VIRTUAL TABLE IF NOT EXISTS rendition_lexical_fts USING fts5(
    generation_id UNINDEXED,
    build_id      UNINDEXED,
    segment_id    UNINDEXED,
    text
);
CREATE TABLE IF NOT EXISTS rendition_lexical_heads (
    singleton     INTEGER PRIMARY KEY CHECK (singleton = 1),
    generation_id TEXT NOT NULL REFERENCES rendition_lexical_generations(generation_id)
);`

// RecordRenditionBlob grants catalog authority to one verified Docbank blob
// receipt without creating a document version or conferring visibility.
func (s *Store) RecordRenditionBlob(
	ctx context.Context, hash string, size int64, physical BlobPhysical,
) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		return s.EnsureBlobTx(tx, hash, size, physical)
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
	generation := LexicalGeneration{ID: generationID}
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, lexicalProjectionSchema); err != nil {
			return fmt.Errorf("initializing lexical projection: %w", err)
		}
		var storedCount int
		var storedManifest sql.NullString
		err := tx.QueryRowContext(ctx, `
			SELECT g.segment_count,m.manifest_digest
			FROM rendition_lexical_generations g
			LEFT JOIN rendition_lexical_generation_manifests m
			  ON m.generation_id=g.generation_id
			WHERE g.generation_id=?`,
			generationID,
		).Scan(&storedCount, &storedManifest)
		if err == nil {
			actualRows, err := readLexicalManifestRowsTx(ctx, tx, generationID, "")
			if err != nil {
				return err
			}
			actualManifest := lexicalManifestDigest(actualRows)
			if !storedManifest.Valid || len(actualRows) != storedCount ||
				actualManifest != storedManifest.String {
				return fmt.Errorf("lexical generation %s has a different immutable manifest", generationID)
			}
			generation.SegmentCount = storedCount
			generation.ManifestDigest = storedManifest.String
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("reading lexical generation %s: %w", generationID, err)
		}

		segments, err := readCatalogLexicalManifestRowsTx(ctx, tx, "")
		if err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx,
			`DELETE FROM rendition_lexical_fts WHERE generation_id=?`, generationID,
		); err != nil {
			return fmt.Errorf("clearing interrupted lexical generation %s: %w", generationID, err)
		}
		for _, segment := range segments {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO rendition_lexical_fts(generation_id,build_id,segment_id,text)
				VALUES(?,?,?,?)`, generationID, segment.buildID, segment.segmentID, segment.text,
			); err != nil {
				return fmt.Errorf("building lexical generation %s: %w", generationID, err)
			}
		}
		generation.SegmentCount = len(segments)
		generation.ManifestDigest = lexicalManifestDigest(segments)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rendition_lexical_generations(generation_id,segment_count,built_at)
			VALUES(?,?,?)`, generationID, generation.SegmentCount, nowRFC3339(),
		); err != nil {
			return fmt.Errorf("completing lexical generation %s: %w", generationID, err)
		}
		actualRows, err := readLexicalManifestRowsTx(ctx, tx, generationID, "")
		if err != nil {
			return err
		}
		if len(actualRows) != generation.SegmentCount ||
			lexicalManifestDigest(actualRows) != generation.ManifestDigest {
			return fmt.Errorf("lexical generation %s has a different immutable manifest", generationID)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rendition_lexical_generation_manifests(generation_id,manifest_digest)
			VALUES(?,?)`, generationID, generation.ManifestDigest,
		); err != nil {
			return fmt.Errorf("recording lexical generation %s manifest: %w", generationID, err)
		}
		return nil
	})
	if err != nil {
		return LexicalGeneration{}, err
	}
	return generation, nil
}

type lexicalManifestRow struct {
	buildID   string
	segmentID string
	text      string
}

func readCatalogLexicalManifestRowsTx(
	ctx context.Context, tx *sql.Tx, buildID string,
) ([]lexicalManifestRow, error) {
	query := `
		SELECT build_id,segment_id,text
		FROM rendition_lexical_segments
		ORDER BY build_id,segment_order,segment_id`
	var (
		rows *sql.Rows
		err  error
	)
	if buildID == "" {
		rows, err = tx.QueryContext(ctx, query)
	} else {
		rows, err = tx.QueryContext(ctx, `
			SELECT build_id,segment_id,text
			FROM rendition_lexical_segments
			WHERE build_id=?
			ORDER BY build_id,segment_order,segment_id`, buildID)
	}
	if err != nil {
		return nil, fmt.Errorf("reading staged lexical manifest: %w", err)
	}
	return scanLexicalManifestRows(rows, "staged lexical manifest")
}

func readLexicalManifestRowsTx(
	ctx context.Context, tx *sql.Tx, generationID, buildID string,
) ([]lexicalManifestRow, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if buildID == "" {
		rows, err = tx.QueryContext(ctx, `
			SELECT build_id,segment_id,text
			FROM rendition_lexical_fts
			WHERE generation_id=?`, generationID)
	} else {
		rows, err = tx.QueryContext(ctx, `
			SELECT build_id,segment_id,text
			FROM rendition_lexical_fts
			WHERE generation_id=? AND build_id=?`, generationID, buildID)
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
		SELECT g.generation_id,g.segment_count,m.manifest_digest
		FROM rendition_lexical_heads h
		JOIN rendition_lexical_generations g ON g.generation_id=h.generation_id
		JOIN rendition_lexical_generation_manifests m ON m.generation_id=g.generation_id
		WHERE h.singleton=1`).Scan(
		&generation.ID, &generation.SegmentCount, &generation.ManifestDigest,
	)
	if errors.Is(err, sql.ErrNoRows) || isMissingLexicalSchema(err) {
		return LexicalGeneration{}, ErrNotFound
	}
	if err != nil {
		return LexicalGeneration{}, fmt.Errorf("reading active lexical generation: %w", err)
	}
	return generation, nil
}

// AcquireLexicalGeneration pins the exact generation selected by the current
// lexical head. The caller must release the returned lease.
func (s *Store) AcquireLexicalGeneration(ctx context.Context) (*LexicalGenerationLease, error) {
	return s.acquireLexicalGeneration(ctx, s.db)
}

func (s *Store) acquireLexicalGeneration(
	ctx context.Context, queryer rowQuerier,
) (*LexicalGenerationLease, error) {
	lexicalGenerationReaders.Lock()
	defer lexicalGenerationReaders.Unlock()

	generation, err := readActiveLexicalGeneration(ctx, queryer)
	if err != nil {
		return nil, err
	}
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

	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, lexicalProjectionSchema); err != nil {
			return fmt.Errorf("initializing lexical projection: %w", err)
		}
		var generationSegments int
		var generationManifest sql.NullString
		if err := tx.QueryRowContext(ctx, `
			SELECT g.segment_count,m.manifest_digest
			FROM rendition_lexical_generations g
			LEFT JOIN rendition_lexical_generation_manifests m
			  ON m.generation_id=g.generation_id
			WHERE g.generation_id=?`,
			generationID,
		).Scan(&generationSegments, &generationManifest); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("lexical generation %s: %w", generationID, ErrNotFound)
		} else if err != nil {
			return fmt.Errorf("reading lexical generation %s: %w", generationID, err)
		}
		generationRows, err := readLexicalManifestRowsTx(ctx, tx, generationID, "")
		if err != nil {
			return err
		}
		if !generationManifest.Valid || len(generationRows) != generationSegments ||
			lexicalManifestDigest(generationRows) != generationManifest.String {
			return fmt.Errorf("lexical generation %s has a different immutable manifest", generationID)
		}
		if err := ensureProcessingProfileTx(ctx, tx, normalized.Profile); err != nil {
			return err
		}
		build, err := loadRenditionBuild(ctx, tx, normalized.BuildID)
		if err != nil {
			return fmt.Errorf("reading rendition build %s: %w", normalized.BuildID, err)
		}
		if build.VaultID != normalized.VaultID {
			return errors.New("rendition attachment and build belong to different vaults")
		}
		if build.RenditionRequestFingerprint != normalized.Profile.RenditionRequestFingerprint ||
			build.EvidenceLexicalFingerprint != normalized.Profile.EvidenceLexicalFingerprint {
			return errors.New("rendition attachment profile does not match build component identity")
		}
		var sourceSHA256 string
		if err := tx.QueryRowContext(ctx,
			`SELECT blob_hash FROM content_versions WHERE version_id=?`, normalized.ContentVersionID,
		).Scan(&sourceSHA256); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("content version %s: %w", normalized.ContentVersionID, ErrNotFound)
		} else if err != nil {
			return fmt.Errorf("reading content version %s: %w", normalized.ContentVersionID, err)
		}
		if sourceSHA256 != build.SourceSHA256 {
			return errors.New("rendition attachment source does not match content version")
		}
		if err := validateRenditionBuildStateTx(ctx, tx, build.ID); err != nil {
			return err
		}
		expectedBuildRows, err := readCatalogLexicalManifestRowsTx(ctx, tx, build.ID)
		if err != nil {
			return err
		}
		indexedBuildRows, err := readLexicalManifestRowsTx(ctx, tx, generationID, build.ID)
		if err != nil {
			return err
		}
		if len(indexedBuildRows) != len(expectedBuildRows) ||
			lexicalManifestDigest(indexedBuildRows) != lexicalManifestDigest(expectedBuildRows) {
			return fmt.Errorf("lexical generation %s does not exactly contain build %s",
				generationID, build.ID)
		}

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
			normalized.AttachedAt,
		)
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
			head.AttachmentID, head.PublishedAt,
		); err != nil {
			return fmt.Errorf("publishing rendition head: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rendition_lexical_heads(singleton,generation_id) VALUES(1,?)
			ON CONFLICT(singleton) DO UPDATE SET generation_id=excluded.generation_id`,
			generationID,
		); err != nil {
			return fmt.Errorf("publishing lexical head: %w", err)
		}
		return nil
	})
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
	lexical, err := s.hasLexicalProjection(ctx)
	if err != nil {
		return nil, false, err
	}
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
			// this reader's one immutable publication snapshot.
			contentQuery = `
				WITH matched_versions(version_id,best_rank) AS (
				  SELECT cv_legacy.version_id, MIN(content_fts.rank)
				  FROM content_fts
				  JOIN content_versions cv_legacy ON cv_legacy.blob_hash=content_fts.blob_hash
				  JOIN text_searchable_versions tsv_legacy
				    ON tsv_legacy.version_id=cv_legacy.version_id
				  WHERE content_fts MATCH ?
				  GROUP BY cv_legacy.version_id
				  UNION ALL
				  SELECT a.content_version_id, MIN(rendition_lexical_fts.rank)
				  FROM rendition_lexical_fts
				  JOIN rendition_attachments a ON a.build_id=rendition_lexical_fts.build_id
				  JOIN rendition_heads rh
				    ON rh.content_version_id=a.content_version_id
				   AND rh.profile_fingerprint=a.profile_fingerprint
				   AND rh.attachment_id=a.attachment_id
				  WHERE rendition_lexical_fts MATCH ?
				    AND rendition_lexical_fts.generation_id=?
				  GROUP BY a.content_version_id
				), best_versions AS (
				  SELECT version_id,MIN(best_rank) AS best_rank
				  FROM matched_versions GROUP BY version_id
				)
				SELECT ` + nodeCols + `
				FROM ` + nodeFrom + `
				JOIN best_versions mv ON mv.version_id=cv.version_id
				WHERE n.trashed_at IS NULL
				  ` + filterSQL + `
				ORDER BY mv.best_rank,n.name,n.id
				LIMIT ?`
			contentArgs = append(contentArgs, fq, generationID)
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
	if lexical {
		err = s.withLexicalGenerationRead(ctx, func(
			queryer metadataQuerier, generation LexicalGeneration,
		) error {
			return queryContent(queryer, generation.ID)
		})
		if errors.Is(err, ErrNotFound) {
			lexical = false
		} else if err != nil {
			return nil, false, err
		}
	}
	if !lexical {
		if err := queryContent(s.db, ""); err != nil {
			return nil, false, err
		}
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

func (s *Store) hasLexicalProjection(ctx context.Context) (bool, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS(
		  SELECT 1 FROM sqlite_schema
		  WHERE type='table' AND name='rendition_lexical_heads'
		)`).Scan(&exists); err != nil {
		return false, fmt.Errorf("checking lexical projection: %w", err)
	}
	return exists, nil
}

func isMissingLexicalSchema(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table: rendition_lexical_heads")
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
