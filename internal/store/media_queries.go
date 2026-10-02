package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/docbank/internal/canonical"
)

type MediaSourceProjection struct {
	SourceID, Kind, SourceVersionID, ContentVersionID, OccurrenceID string
	Filename, CaptureJSON                                           string
	Receipt                                                         MediaPublicationReceipt
	// ProcessingReceipts lists processing attempts for the bound version, newest admission first.
	ProcessingReceipts []MediaPublicationReceipt
}

type MediaOccurrenceProjection struct {
	OccurrenceID, SourceID, Kind, SourceVersionID, Ref, Revision string
	Filename, PersonRef, SpeakerLabel, MessageJSON               string
}

// MediaSources returns the page of visible sources after the given source ID
// and whether more follow.
func (s *Store) MediaSources(
	ctx context.Context, principal, after string, limit int,
) ([]MediaSourceProjection, int, bool, error) {
	if limit < 1 || limit > 250 {
		return nil, 0, false, errors.New("invalid media source page")
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT source_id) FROM media_occurrences
		WHERE caller_principal=? AND visible=1`, principal).Scan(&total); err != nil {
		return nil, 0, false, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT o.source_id,s.kind,COALESCE(o.source_version_id,''),
		COALESCE(v.content_version_id,''),o.occurrence_id,o.caller_filename,o.message_json
		FROM media_occurrences o
		JOIN media_sources s ON s.source_id=o.source_id
		LEFT JOIN media_source_versions v ON v.source_version_id=o.source_version_id
		WHERE o.caller_principal=? AND o.visible=1 AND o.source_id>? AND o.occurrence_id=(
			SELECT x.occurrence_id FROM media_occurrences x
			WHERE x.caller_principal=o.caller_principal AND x.visible=1 AND x.source_id=o.source_id
			ORDER BY x.first_seen_at DESC,x.occurrence_id DESC LIMIT 1)
		ORDER BY o.source_id LIMIT ?`, principal, after, limit+1)
	if err != nil {
		return nil, 0, false, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]MediaSourceProjection, 0, limit)
	for rows.Next() {
		var item MediaSourceProjection
		if err := rows.Scan(&item.SourceID, &item.Kind, &item.SourceVersionID, &item.ContentVersionID,
			&item.OccurrenceID, &item.Filename, &item.CaptureJSON); err != nil {
			return nil, 0, false, err
		}
		item.Receipt, item.ProcessingReceipts, err = s.latestMediaReceipts(ctx, principal,
			item.SourceID, item.SourceVersionID, false)
		if err != nil {
			return nil, 0, false, err
		}
		result = append(result, item)
	}
	return result[:min(len(result), limit)], total, len(result) > limit, rows.Err()
}

func (s *Store) MediaSource(
	ctx context.Context, principal, sourceID string,
) (MediaSourceProjection, error) {
	return s.mediaSourceProjection(ctx, principal, sourceID, "")
}

// MediaSourceVersion returns the caller-visible projection for one exact
// source revision. An exact revision is selected before receipts are read so
// equal bytes under another source cannot supply its metadata.
func (s *Store) MediaSourceVersion(
	ctx context.Context, principal, sourceID, sourceVersionID string,
) (MediaSourceProjection, error) {
	if err := validateBoundedMediaText("media source", sourceID, 256, false); err != nil {
		return MediaSourceProjection{}, err
	}
	if err := validateBoundedMediaText("media source version", sourceVersionID, 256, false); err != nil {
		return MediaSourceProjection{}, err
	}
	return s.mediaSourceProjection(ctx, principal, sourceID, sourceVersionID)
}

func (s *Store) mediaSourceProjection(
	ctx context.Context, principal, sourceID, sourceVersionID string,
) (MediaSourceProjection, error) {
	var item MediaSourceProjection
	query := `SELECT o.source_id,s.kind,COALESCE(o.source_version_id,''),
		COALESCE(v.content_version_id,''),o.occurrence_id,o.caller_filename,o.message_json
		FROM media_occurrences o
		JOIN media_sources s ON s.source_id=o.source_id
		LEFT JOIN media_source_versions v ON v.source_version_id=o.source_version_id
		WHERE o.caller_principal=? AND o.visible=1 AND o.source_id=?`
	args := []any{principal, sourceID}
	if sourceVersionID != "" {
		query += ` AND o.source_version_id=?`
		args = append(args, sourceVersionID)
	}
	query += ` ORDER BY o.first_seen_at DESC,o.occurrence_id DESC LIMIT 1`
	err := s.db.QueryRowContext(ctx, query, args...).Scan(
		&item.SourceID, &item.Kind, &item.SourceVersionID, &item.ContentVersionID, &item.OccurrenceID,
		&item.Filename, &item.CaptureJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return MediaSourceProjection{}, ErrNotFound
	}
	if err != nil {
		return MediaSourceProjection{}, err
	}
	item.Receipt, item.ProcessingReceipts, err = s.latestMediaReceipts(ctx, principal,
		sourceID, item.SourceVersionID, sourceVersionID != "")
	return item, err
}

func (s *Store) MediaOccurrence(
	ctx context.Context, principal, occurrenceID string,
) (MediaOccurrenceProjection, error) {
	var item MediaOccurrenceProjection
	err := s.db.QueryRowContext(ctx, `SELECT o.occurrence_id,o.source_id,s.kind,COALESCE(o.source_version_id,''),
		o.caller_occurrence_ref,o.caller_revision,o.caller_filename,o.caller_person_ref,o.speaker_label,o.message_json
		FROM media_occurrences o JOIN media_sources s ON s.source_id=o.source_id
		WHERE o.caller_principal=? AND o.visible=1 AND o.occurrence_id=?`,
		principal, occurrenceID).Scan(&item.OccurrenceID, &item.SourceID, &item.Kind,
		&item.SourceVersionID, &item.Ref, &item.Revision, &item.Filename, &item.PersonRef,
		&item.SpeakerLabel, &item.MessageJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return MediaOccurrenceProjection{}, ErrNotFound
	}
	return item, err
}

// MediaSourceBindingForContentVersion finds the source that owns one content
// version, preferring visible occurrences so processing retains source
// authority when equal bytes occur under separate remote references.
func (s *Store) MediaSourceBindingForContentVersion(
	ctx context.Context, principal, contentVersionID string,
) (string, string, error) {
	if err := validateBoundedMediaText("media principal", principal, 256, false); err != nil {
		return "", "", err
	}
	if err := validateBoundedMediaText("media content version", contentVersionID, 256, false); err != nil {
		return "", "", err
	}
	var sourceID, sourceVersionID string
	err := s.db.QueryRowContext(ctx, `SELECT o.source_id,o.source_version_id
		FROM media_occurrences o
		JOIN media_source_versions v ON v.source_version_id=o.source_version_id
		WHERE o.caller_principal=? AND v.content_version_id=?
		ORDER BY o.visible DESC,o.first_seen_at DESC,o.occurrence_id DESC LIMIT 1`, principal, contentVersionID).Scan(
		&sourceID, &sourceVersionID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return sourceID, sourceVersionID, err
}

func (s *Store) latestMediaReceipts(
	ctx context.Context, principal, sourceID, sourceVersionID string, exactVersion bool,
) (MediaPublicationReceipt, []MediaPublicationReceipt, error) {
	var raw string
	query := `SELECT receipt_json FROM media_operations
		WHERE principal=? AND source_id=?
			AND verb IN ('submit_supplied_media','submit_remote_recording')`
	args := []any{principal, sourceID}
	if exactVersion {
		query += ` AND (json_extract(receipt_json, '$.source_version_id')=?
			OR COALESCE(json_extract(receipt_json, '$.source_version_id'), '')='')`
		args = append(args, sourceVersionID)
		query += ` ORDER BY CASE WHEN json_extract(receipt_json, '$.source_version_id')=? THEN 0 ELSE 1 END,
			updated_at DESC,operation_id DESC LIMIT 1`
		args = append(args, sourceVersionID)
	} else {
		query += ` ORDER BY updated_at DESC,operation_id DESC LIMIT 1`
	}
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return MediaPublicationReceipt{}, nil, ErrNotFound
	}
	if err != nil {
		return MediaPublicationReceipt{}, nil, err
	}
	retention, err := canonical.Decode[MediaPublicationReceipt]([]byte(raw))
	if err != nil {
		return MediaPublicationReceipt{}, nil, fmt.Errorf("decoding media retention receipt: %w", err)
	}
	if sourceVersionID == "" {
		return retention, nil, nil
	}
	processing, err := s.mediaProcessingReceiptsForVersion(ctx, principal, sourceID, sourceVersionID)
	return retention, processing, err
}

func (s *Store) mediaProcessingReceiptsForVersion(
	ctx context.Context, principal, sourceID, sourceVersionID string,
) (_ []MediaPublicationReceipt, retErr error) {
	// Admission order defines the current attempt. An older worker finishing
	// later must not hide a retry that was already admitted.
	rows, err := s.db.QueryContext(ctx, `SELECT receipt_json FROM media_operations
		WHERE principal=? AND source_id=? AND verb IN ('submit_supplied_media','retry_media')
			AND receipt_json LIKE '%"processing_profile"%'
			AND json_extract(receipt_json, '$.source_version_id') = ?
		ORDER BY created_at DESC,operation_id DESC`, principal, sourceID, sourceVersionID)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	var result []MediaPublicationReceipt
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		processing, err := canonical.Decode[MediaPublicationReceipt]([]byte(raw))
		if err != nil {
			return nil, fmt.Errorf("decoding media processing receipt: %w", err)
		}
		result = append(result, processing)
	}
	return result, rows.Err()
}

// MediaOccurrences returns the page of visible occurrences after the given
// occurrence ID and whether more follow.
func (s *Store) MediaOccurrences(
	ctx context.Context, principal, sourceID, after string, limit int,
) ([]MediaOccurrenceProjection, int, bool, error) {
	if limit < 1 || limit > 250 {
		return nil, 0, false, errors.New("invalid media occurrence page")
	}
	where := `caller_principal=? AND visible=1`
	args := []any{principal}
	if sourceID != "" {
		where += ` AND source_id=?`
		args = append(args, sourceID)
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_occurrences WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, false, err
	}
	args = append(args, after, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT occurrence_id,source_id,COALESCE(source_version_id,''),
		caller_occurrence_ref,caller_revision,caller_filename,caller_person_ref,speaker_label,message_json
		FROM media_occurrences WHERE `+where+` AND occurrence_id>? ORDER BY occurrence_id LIMIT ?`, args...)
	if err != nil {
		return nil, 0, false, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]MediaOccurrenceProjection, 0, limit)
	for rows.Next() {
		var item MediaOccurrenceProjection
		if err := rows.Scan(&item.OccurrenceID, &item.SourceID, &item.SourceVersionID,
			&item.Ref, &item.Revision, &item.Filename, &item.PersonRef, &item.SpeakerLabel,
			&item.MessageJSON); err != nil {
			return nil, 0, false, err
		}
		result = append(result, item)
	}
	return result[:min(len(result), limit)], total, len(result) > limit, rows.Err()
}
