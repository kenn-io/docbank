package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"mime"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/kit/packstore"
)

const (
	ExtractionOK     = "ok"
	ExtractionFailed = "failed"
)

// ExtractionCandidate is one catalog-authorized text blob that has not been
// processed by the current extractor version.
type ExtractionCandidate struct {
	BlobHash string
	Size     int64
}

// ExtractionResult is one complete, versioned derived-text attempt. Text is
// present only for a successful, terminally verified extraction.
type ExtractionResult struct {
	BlobHash         string
	Extractor        string
	ExtractorVersion int64
	Status           string
	Error            string
	Text             string
}

func supportsTextExtractionMIME(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	mediaType = strings.ToLower(mediaType)
	return strings.HasPrefix(mediaType, "text/") || mediaType == "application/json" ||
		mediaType == "application/x-ndjson" || mediaType == "application/jsonl"
}

func markTextSearchableVersionTx(tx *sql.Tx, versionID, mimeType string) (bool, error) {
	if !supportsTextExtractionMIME(mimeType) {
		return false, nil
	}
	if _, err := tx.Exec(`
		INSERT INTO text_searchable_versions(version_id) VALUES(?)
		ON CONFLICT(version_id) DO NOTHING`, versionID); err != nil {
		return false, fmt.Errorf("recording text-search eligibility for %s: %w", versionID, err)
	}
	return true, nil
}

func enqueueTextBlobTx(tx *sql.Tx, blobHash string) error {
	if _, err := tx.Exec(`
		INSERT INTO text_extraction_queue(blob_hash,next_attempt_at) VALUES(?,?)
		ON CONFLICT(blob_hash) DO UPDATE SET next_attempt_at=excluded.next_attempt_at`,
		blobHash, nowRFC3339()); err != nil {
		return fmt.Errorf("queueing text extraction for %s: %w", blobHash, err)
	}
	return nil
}

func queueTextExtractionTx(tx *sql.Tx, versionID, blobHash, mimeType string) error {
	eligible, err := markTextSearchableVersionTx(tx, versionID, mimeType)
	if err != nil || !eligible {
		return err
	}
	return enqueueTextBlobTx(tx, blobHash)
}

// rebuildImportedTextExtractionStateTx reconstructs the derived search
// eligibility and work queue after every authoritative metadata record has
// been imported. Historical versions remain available for explicit version
// reads, but only selected heads can appear in ordinary search and therefore
// only selected heads need extraction work.
func rebuildImportedTextExtractionStateTx(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM text_searchable_versions`); err != nil {
		return fmt.Errorf("clearing imported text-search eligibility: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM text_extraction_queue`); err != nil {
		return fmt.Errorf("clearing imported text-extraction queue: %w", err)
	}
	type seed struct {
		versionID, hash, mimeType string
		hasExtraction             bool
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT v.version_id, v.blob_hash, COALESCE(v.mime_type, ''),
		       EXISTS(SELECT 1 FROM extracted_text e WHERE e.blob_hash=v.blob_hash)
		FROM content_versions v
		JOIN nodes n ON n.current_version_id=v.version_id
		ORDER BY v.version_id`)
	if err != nil {
		return fmt.Errorf("discovering imported text extraction work: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var seeds []seed
	for rows.Next() {
		var item seed
		if err := rows.Scan(
			&item.versionID, &item.hash, &item.mimeType, &item.hasExtraction,
		); err != nil {
			return fmt.Errorf("discovering imported text extraction work: scanning row: %w", err)
		}
		if supportsTextExtractionMIME(item.mimeType) {
			seeds = append(seeds, item)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("discovering imported text extraction work: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("discovering imported text extraction work: %w", err)
	}

	queued := make(map[string]struct{})
	for _, item := range seeds {
		if _, err := markTextSearchableVersionTx(tx, item.versionID, item.mimeType); err != nil {
			return err
		}
		if item.hasExtraction {
			continue
		}
		if _, exists := queued[item.hash]; exists {
			continue
		}
		queued[item.hash] = struct{}{}
		if err := enqueueTextBlobTx(tx, item.hash); err != nil {
			return err
		}
	}
	return nil
}

// SeedTextExtractionQueue discovers supported selected content once at daemon
// startup. Discovery and projection writes share one storage transaction, so a
// concurrent deletion cannot invalidate a selected version between those steps.
// Later logical writes enqueue in Go while existing vaults converge here.
func (s *Store) SeedTextExtractionQueue(
	ctx context.Context, extractor string, version int64,
) error {
	if extractor == "" || version < 1 {
		return errors.New("extractor name and positive version are required")
	}
	type seed struct {
		versionID, hash, mimeType string
		needsExtraction           bool
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT v.version_id, v.blob_hash, COALESCE(v.mime_type, ''),
			       CASE WHEN e.blob_hash IS NULL OR e.extractor_version < ? THEN 1 ELSE 0 END
			FROM content_versions v
			JOIN nodes n ON n.current_version_id = v.version_id
			LEFT JOIN extracted_text e
			  ON e.blob_hash=v.blob_hash AND e.extractor=?
			ORDER BY v.version_id`, version, extractor)
		if err != nil {
			return fmt.Errorf("discovering text extraction work: %w", err)
		}
		defer func() { _ = rows.Close() }()
		var seeds []seed
		for rows.Next() {
			var item seed
			if err := rows.Scan(
				&item.versionID, &item.hash, &item.mimeType, &item.needsExtraction,
			); err != nil {
				return fmt.Errorf("discovering text extraction work: scanning row: %w", err)
			}
			if supportsTextExtractionMIME(item.mimeType) {
				seeds = append(seeds, item)
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("discovering text extraction work: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("discovering text extraction work: %w", err)
		}

		queued := make(map[string]struct{})
		for _, item := range seeds {
			if _, err := markTextSearchableVersionTx(tx, item.versionID, item.mimeType); err != nil {
				return err
			}
			if !item.needsExtraction {
				continue
			}
			if _, exists := queued[item.hash]; exists {
				continue
			}
			queued[item.hash] = struct{}{}
			if err := enqueueTextBlobTx(tx, item.hash); err != nil {
				return err
			}
		}
		return nil
	})
}

// PendingTextExtractions returns a bounded hash-ordered batch from the derived
// work queue. Logical writes and one startup seed of selected versions fill the
// queue, so steady-state polling never scans the version catalog.
func (s *Store) PendingTextExtractions(
	ctx context.Context, limit int,
) ([]ExtractionCandidate, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("extraction batch limit must be between 1 and 1000")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT q.blob_hash, b.size
		FROM text_extraction_queue q
		JOIN blobs b ON b.hash=q.blob_hash
		WHERE EXISTS(
		  SELECT 1
		  FROM content_versions v
		  JOIN nodes n ON n.current_version_id=v.version_id
		  JOIN text_searchable_versions sv ON sv.version_id=v.version_id
		  WHERE v.blob_hash=q.blob_hash
		)
		  AND q.next_attempt_at <= ?
		ORDER BY q.next_attempt_at, q.blob_hash
		LIMIT ?`, nowRFC3339(), limit)
	if err != nil {
		return nil, fmt.Errorf("listing pending text extractions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	items := make([]ExtractionCandidate, 0)
	for rows.Next() {
		var item ExtractionCandidate
		if err := rows.Scan(&item.BlobHash, &item.Size); err != nil {
			return nil, fmt.Errorf("listing pending text extractions: scanning row: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing pending text extractions: %w", err)
	}
	return items, nil
}

// DeferTextExtraction leaves a retryable item queued but moves it behind work
// that is ready now. The worker chooses the bounded delay in Go.
func (s *Store) DeferTextExtraction(
	ctx context.Context, blobHash string, notBefore time.Time,
) error {
	if _, err := packstore.ParseHash(blobHash); err != nil {
		return fmt.Errorf("invalid deferred blob hash: %w", err)
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE text_extraction_queue SET next_attempt_at = ? WHERE blob_hash = ?`,
			notBefore.UTC().Format(timestampLayout), blobHash)
		if err != nil {
			return fmt.Errorf("deferring text extraction for %s: %w", blobHash, err)
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("deferring text extraction for %s: %w", blobHash, err)
		}
		if updated == 0 {
			return fmt.Errorf("text extraction %s: %w", blobHash, ErrNotFound)
		}
		return nil
	})
}

// RecordExtraction atomically replaces one extractor's derived result and its
// searchable projection. It never changes document, version, or audit state.
func (s *Store) RecordExtraction(ctx context.Context, result ExtractionResult) error {
	if _, err := packstore.ParseHash(result.BlobHash); err != nil {
		return fmt.Errorf("invalid extracted blob hash: %w", err)
	}
	if result.Extractor == "" || result.ExtractorVersion < 1 {
		return errors.New("extractor name and positive version are required")
	}
	if !utf8.ValidString(result.Extractor) || !utf8.ValidString(result.Error) ||
		!utf8.ValidString(result.Text) {
		return errors.New("extraction result is not valid UTF-8")
	}
	switch result.Status {
	case ExtractionOK:
		if result.Error != "" {
			return errors.New("successful extraction must not contain an error")
		}
	case ExtractionFailed:
		if result.Error == "" || result.Text != "" {
			return errors.New("failed extraction requires an error and no text")
		}
	default:
		return fmt.Errorf("invalid extraction status %q", result.Status)
	}

	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM blobs WHERE hash = ?)`, result.BlobHash,
		).Scan(&exists); err != nil {
			return fmt.Errorf("checking extracted blob authority: %w", err)
		}
		if !exists {
			return fmt.Errorf("blob %s: %w", result.BlobHash, ErrNotFound)
		}
		var extractErr, text any
		if result.Error != "" {
			extractErr = result.Error
		}
		if result.Status == ExtractionOK {
			text = result.Text
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO extracted_text(
			  blob_hash,extractor,extractor_version,status,error,attempts,text,extracted_at
			) VALUES(?,?,?,?,?,1,?,?)
			ON CONFLICT(blob_hash,extractor) DO UPDATE SET
			  extractor_version=excluded.extractor_version,
			  status=excluded.status,
			  error=excluded.error,
			  attempts=extracted_text.attempts+1,
			  text=excluded.text,
			  extracted_at=excluded.extracted_at`,
			result.BlobHash, result.Extractor, result.ExtractorVersion,
			result.Status, extractErr, text, nowRFC3339()); err != nil {
			return fmt.Errorf("recording text extraction: %w", err)
		}
		if err := replaceContentFTSTx(ctx, tx, result.BlobHash, result.Extractor, text); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM text_extraction_queue WHERE blob_hash = ?`, result.BlobHash,
		); err != nil {
			return fmt.Errorf("finishing text extraction: %w", err)
		}
		return nil
	})
}

func replaceContentFTSTx(
	ctx context.Context, tx *sql.Tx, blobHash, extractor string, text any,
) error {
	var rowID int64
	if err := tx.QueryRowContext(ctx,
		`SELECT rowid FROM extracted_text WHERE blob_hash = ? AND extractor = ?`,
		blobHash, extractor,
	).Scan(&rowID); err != nil {
		return fmt.Errorf("resolving extracted-text row: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM content_fts WHERE rowid = ?`, rowID,
	); err != nil {
		return fmt.Errorf("removing prior content search row: %w", err)
	}
	if text == nil {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO content_fts(rowid,blob_hash,extractor,text) VALUES(?,?,?,?)`,
		rowID, blobHash, extractor, text,
	); err != nil {
		return fmt.Errorf("indexing extracted text: %w", err)
	}
	return nil
}
