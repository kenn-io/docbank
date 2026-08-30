package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/kit/packstore"
)

const auxiliaryMD5Length = 32

// BlobChecksumRecord binds auxiliary interoperability metadata to Docbank's
// authoritative SHA-256 blob identity.
type BlobChecksumRecord struct {
	BlobSHA256 string `json:"blob_sha256"`
	MD5        string `json:"md5"`
}

// BlobChecksumTarget identifies retained bytes whose auxiliary checksum is
// not yet known. The SHA-256 identity and size remain authoritative.
type BlobChecksumTarget struct {
	BlobSHA256 string
	Size       int64
}

func validateAuxiliaryMD5(value string) error {
	if len(value) != auxiliaryMD5Length || value != strings.ToLower(value) {
		return errors.New("auxiliary checksum must be canonical lowercase MD5")
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return errors.New("auxiliary checksum must be canonical lowercase MD5")
		}
	}
	return nil
}

func validateBlobChecksumRecord(record BlobChecksumRecord) error {
	parsed, err := packstore.ParseHash(record.BlobSHA256)
	if err != nil || parsed.String() != record.BlobSHA256 {
		return errors.New("blob checksum SHA-256 identity is invalid")
	}
	return validateAuxiliaryMD5(record.MD5)
}

func ensureBlobChecksumTx(tx *sql.Tx, record BlobChecksumRecord) error {
	if record.MD5 == "" {
		return nil
	}
	if err := validateBlobChecksumRecord(record); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO blob_checksums(blob_sha256,md5) VALUES(?,?)
		ON CONFLICT(blob_sha256) DO NOTHING`, record.BlobSHA256, record.MD5); err != nil {
		return fmt.Errorf("recording auxiliary checksum for blob %s: %w", record.BlobSHA256, err)
	}
	var stored string
	if err := tx.QueryRow(`SELECT md5 FROM blob_checksums WHERE blob_sha256=?`,
		record.BlobSHA256).Scan(&stored); err != nil {
		return fmt.Errorf("reading auxiliary checksum for blob %s: %w", record.BlobSHA256, err)
	}
	if stored != record.MD5 {
		return fmt.Errorf("blob %s has different MD5 %s", record.BlobSHA256, stored)
	}
	return nil
}

// BlobChecksums returns auxiliary checksums for one retained SHA-256 blob.
func (s *Store) BlobChecksums(ctx context.Context, sha256 string) (BlobChecksumRecord, error) {
	if _, err := packstore.ParseHash(sha256); err != nil {
		return BlobChecksumRecord{}, fmt.Errorf("blob checksum %q: %w", sha256, ErrNotFound)
	}
	record := BlobChecksumRecord{}
	if err := s.db.QueryRowContext(ctx,
		`SELECT blob_sha256,md5 FROM blob_checksums WHERE blob_sha256=?`, sha256,
	).Scan(&record.BlobSHA256, &record.MD5); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return BlobChecksumRecord{}, ErrNotFound
		}
		return BlobChecksumRecord{}, fmt.Errorf("reading blob checksum %s: %w", sha256, err)
	}
	return record, nil
}

// RecordVerifiedBlobChecksum commits a locally computed auxiliary checksum.
// The caller must have read the exact bytes through a verified blob resolver.
func (s *Store) RecordVerifiedBlobChecksum(ctx context.Context, record BlobChecksumRecord) error {
	if err := validateBlobChecksumRecord(record); err != nil {
		return err
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var present int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM blobs WHERE hash=?`, record.BlobSHA256,
		).Scan(&present); err != nil {
			return fmt.Errorf("checking blob checksum target %s: %w", record.BlobSHA256, err)
		}
		if present != 1 {
			return fmt.Errorf("blob checksum target %s: %w", record.BlobSHA256, ErrNotFound)
		}
		return ensureBlobChecksumTx(tx, record)
	})
}

// MissingBlobChecksumTargets returns a deterministic resumable batch of
// retained originals and sanitized Markdown without auxiliary checksums.
func (s *Store) MissingBlobChecksumTargets(
	ctx context.Context, limit int,
) ([]BlobChecksumTarget, error) {
	return s.MissingBlobChecksumTargetsAfter(ctx, "", limit)
}

// MissingBlobChecksumTargetsAfter returns the next ordered page after one
// SHA-256 cursor. Callers can keep later blobs progressing while a failed
// target waits for retry.
func (s *Store) MissingBlobChecksumTargetsAfter(
	ctx context.Context, afterSHA256 string, limit int,
) ([]BlobChecksumTarget, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("blob checksum backfill limit must be between 1 and 1000")
	}
	if afterSHA256 != "" {
		if err := validateCatalogSHA256(afterSHA256, "blob checksum backfill cursor"); err != nil {
			return nil, err
		}
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT b.hash,b.size FROM blobs b
		LEFT JOIN blob_checksums c ON c.blob_sha256=b.hash
		WHERE b.hash>? AND c.blob_sha256 IS NULL AND (
		  EXISTS(SELECT 1 FROM content_versions v WHERE v.blob_hash=b.hash)
		  OR EXISTS(SELECT 1 FROM rendition_artifacts a
		            WHERE a.blob_hash=b.hash AND a.role='sanitized_markdown')
		)
		ORDER BY b.hash LIMIT ?`, afterSHA256, limit)
	if err != nil {
		return nil, fmt.Errorf("listing missing blob checksums: %w", err)
	}
	defer func() { _ = rows.Close() }()
	targets := make([]BlobChecksumTarget, 0)
	for rows.Next() {
		var target BlobChecksumTarget
		if err := rows.Scan(&target.BlobSHA256, &target.Size); err != nil {
			return nil, fmt.Errorf("scanning missing blob checksum: %w", err)
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing missing blob checksums: %w", err)
	}
	return targets, nil
}
