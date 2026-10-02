package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/canonical"
)

// ErrMediaSourceConflict reports a stale source head or reuse of immutable
// source-version identity with different authority.
var ErrMediaSourceConflict = errors.New("media source revision conflict")

// MediaSourceVersionInput publishes one exact retained recording revision.
// ExpectedHeadRevision is zero when the source has no published head.
type MediaSourceVersionInput struct {
	ID, SourceID, ContentVersionID, CaptureJSON string
	Revision, ExpectedHeadRevision              int64
	BindOccurrenceIDs                           []string
}

// MediaSourceKey returns the vault-scoped identity for supplied bytes or one
// provider recording. Remote source material is represented only by its
// digest; caller references never enter portable source authority.
func MediaSourceKey(kind, vaultUID, provider, originScope, sourceKey string) (string, error) {
	if err := validateUUIDv4(vaultUID); err != nil {
		return "", fmt.Errorf("invalid media vault identity: %w", err)
	}
	if !utf8.ValidString(sourceKey) || sourceKey == "" || len(sourceKey) > 8192 {
		return "", errors.New("invalid media source key")
	}
	switch kind {
	case "supplied_media":
		if provider != "" || originScope != "" || !canonical.IsSHA256Hex(sourceKey) {
			return "", errors.New("supplied source key must be the sealed digest")
		}
	case "remote_recording":
		if provider == "" || originScope == "" || len(provider) > 128 || len(originScope) > 256 ||
			!utf8.ValidString(provider) || !utf8.ValidString(originScope) {
			return "", errors.New("remote source requires provider and scope")
		}
	default:
		return "", errors.New("unknown media source kind")
	}
	h := sha256.New()
	for _, value := range []string{"media-source/v1", kind, vaultUID, provider, originScope, sourceKey} {
		_, _ = fmt.Fprintf(h, "%d:%s", len(value), value)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// PublishMediaSourceVersion atomically appends an immutable revision and binds only explicitly selected pending occurrences.
func (s *Store) PublishMediaSourceVersion(ctx context.Context, in MediaSourceVersionInput) error {
	if err := validateMediaSourceVersionInput(in); err != nil {
		return err
	}
	return s.withLogicalTx(ctx, func(tx *sql.Tx) error { return s.publishMediaSourceVersionTx(ctx, tx, in) })
}

func (s *Store) publishMediaSourceVersionTx(ctx context.Context, tx *sql.Tx, in MediaSourceVersionInput) error {
	if err := validateMediaSourceVersionInput(in); err != nil {
		return err
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM content_versions WHERE version_id=?`, in.ContentVersionID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	var sourceID string
	if err := tx.QueryRowContext(ctx, `SELECT source_id FROM media_sources WHERE source_id=?`, in.SourceID).Scan(&sourceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	var currentRevision int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0) FROM media_source_versions WHERE source_id=?`, in.SourceID).Scan(&currentRevision); err != nil {
		return err
	}
	if currentRevision != in.ExpectedHeadRevision || in.Revision != currentRevision+1 {
		return ErrMediaSourceConflict
	}
	for _, occurrenceID := range in.BindOccurrenceIDs {
		var occurrenceSource string
		var bound sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT source_id,source_version_id FROM media_occurrences
				WHERE occurrence_id=?`, occurrenceID).Scan(&occurrenceSource, &bound); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if occurrenceSource != in.SourceID || bound.Valid {
			return ErrMediaOccurrenceConflict
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO media_source_versions(
			source_version_id,source_id,revision,content_version_id,capture_json,created_at
		) VALUES(?,?,?,?,?,?)`, in.ID, in.SourceID, in.Revision, in.ContentVersionID, in.CaptureJSON, nowRFC3339()); err != nil {
		if s.driver.IsUniqueViolation(err) {
			return ErrMediaSourceConflict
		}
		return fmt.Errorf("publishing media source version: %w", err)
	}
	for _, occurrenceID := range in.BindOccurrenceIDs {
		if _, err := tx.ExecContext(ctx, `UPDATE media_occurrences SET source_version_id=?
				WHERE occurrence_id=? AND source_id=? AND source_version_id IS NULL`,
			in.ID, occurrenceID, in.SourceID); err != nil {
			return err
		}
	}
	return nil
}

func validateMediaSourceVersionInput(in MediaSourceVersionInput) error {
	for _, field := range []struct {
		name, value string
	}{
		{"media source version ID", in.ID},
		{"media source ID", in.SourceID},
		{"media content version ID", in.ContentVersionID},
	} {
		if err := validateBoundedMediaText(field.name, field.value, 256, false); err != nil {
			return err
		}
	}
	if in.Revision < 1 || in.ExpectedHeadRevision < 0 || len(in.CaptureJSON) == 0 || len(in.CaptureJSON) > 64<<10 {
		return ErrMediaSourceConflict
	}
	if err := canonicalJSONText(in.CaptureJSON, "media capture claim"); err != nil {
		return ErrMediaSourceConflict
	}
	seen := make(map[string]struct{}, len(in.BindOccurrenceIDs))
	for _, id := range in.BindOccurrenceIDs {
		if err := validateBoundedMediaText("media occurrence ID", id, 256, false); err != nil {
			return err
		}
		if _, exists := seen[id]; exists {
			return ErrMediaOccurrenceConflict
		}
		seen[id] = struct{}{}
	}
	return nil
}
