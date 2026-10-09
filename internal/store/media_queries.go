package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
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
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, false, err
	}
	items := make(map[MediaSourceVersionKey]MediaSourceProjection, len(result))
	for _, item := range result {
		items[MediaSourceVersionKey{item.SourceID, item.SourceVersionID}] = item
	}
	if err := s.mediaSourceReceipts(ctx, s.db, principal, items, false); err != nil {
		return nil, 0, false, err
	}
	for i, item := range result {
		var ok bool
		result[i], ok = items[MediaSourceVersionKey{item.SourceID, item.SourceVersionID}]
		if !ok {
			return nil, 0, false, ErrNotFound
		}
	}
	return result[:min(len(result), limit)], total, len(result) > limit, nil
}

func (s *Store) MediaSource(
	ctx context.Context, principal, sourceID string,
) (MediaSourceProjection, error) {
	return s.mediaSourceProjection(ctx, principal, sourceID)
}

// MediaSourceVersion returns the caller-visible projection for one exact
// source revision. An exact revision is selected before receipts are read so
// equal bytes under another source cannot supply its metadata.
func (s *Store) MediaSourceVersion(
	ctx context.Context, principal, sourceID, sourceVersionID string,
) (MediaSourceProjection, error) {
	key := MediaSourceVersionKey{sourceID, sourceVersionID}
	items, err := s.MediaSourceVersions(ctx, principal, []MediaSourceVersionKey{key})
	if err != nil {
		return MediaSourceProjection{}, err
	}
	item, ok := items[key]
	if !ok {
		return MediaSourceProjection{}, ErrNotFound
	}
	return item, nil
}

type MediaSourceVersionKey struct {
	SourceID        string `json:"source_id"`
	SourceVersionID string `json:"source_version_id"`
}

// MediaSourceVersions reads exact visible revisions and their receipts in one snapshot.
func (s *Store) MediaSourceVersions(
	ctx context.Context, principal string, keys []MediaSourceVersionKey,
) (map[MediaSourceVersionKey]MediaSourceProjection, error) {
	if len(keys) == 0 {
		return map[MediaSourceVersionKey]MediaSourceProjection{}, nil
	}
	for _, key := range keys {
		if err := validateBoundedMediaText("media source", key.SourceID, 256, false); err != nil {
			return nil, err
		}
		err := validateBoundedMediaText("media source version", key.SourceVersionID, 256, false)
		if err != nil {
			return nil, err
		}
	}
	encoded, err := json.Marshal(keys)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `WITH visible AS (
		SELECT o.source_id,s.kind,o.source_version_id,COALESCE(v.content_version_id,''),
		o.occurrence_id,o.caller_filename,o.message_json,
		ROW_NUMBER() OVER (PARTITION BY o.source_id,o.source_version_id
			ORDER BY o.first_seen_at DESC,o.occurrence_id DESC) position
		FROM media_occurrences o JOIN media_sources s ON s.source_id=o.source_id
		LEFT JOIN media_source_versions v ON v.source_version_id=o.source_version_id
		WHERE o.caller_principal=? AND o.visible=1 AND (o.source_id,o.source_version_id) IN
		(SELECT json_extract(value,'$.source_id'),json_extract(value,'$.source_version_id')
			FROM json_each(?)))
		SELECT * FROM visible WHERE position=1`, principal, string(encoded))
	if err != nil {
		return nil, err
	}
	result := make(map[MediaSourceVersionKey]MediaSourceProjection, len(keys))
	defer func(rows *sql.Rows) { _ = rows.Close() }(rows)
	for rows.Next() {
		var item MediaSourceProjection
		var position int
		if err := rows.Scan(&item.SourceID, &item.Kind, &item.SourceVersionID, &item.ContentVersionID,
			&item.OccurrenceID, &item.Filename, &item.CaptureJSON, &position); err != nil {
			return nil, err
		}
		key := MediaSourceVersionKey{item.SourceID, item.SourceVersionID}
		result[key] = item
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.mediaSourceReceipts(ctx, tx, principal, result, true); err != nil {
		return nil, err
	}
	return result, tx.Commit()
}

func (s *Store) mediaSourceReceipts(
	ctx context.Context, query metadataQuerier, principal string,
	result map[MediaSourceVersionKey]MediaSourceProjection, exactVersion bool,
) error {
	if len(result) == 0 {
		return nil
	}
	keys := make([]MediaSourceVersionKey, 0, len(result))
	for key := range result {
		keys = append(keys, key)
	}
	encoded, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	rows, err := query.QueryContext(ctx, `WITH requested AS MATERIALIZED (
  SELECT json_extract(value,'$.source_id') source_id,
   json_extract(value,'$.source_version_id') source_version_id FROM json_each(?)
 ), retention AS (
  SELECT r.source_id,r.source_version_id,o.receipt_json,
   ROW_NUMBER() OVER (PARTITION BY r.source_id,r.source_version_id ORDER BY
    CASE WHEN ? AND json_extract(o.receipt_json,'$.source_version_id')=r.source_version_id
     THEN 0 ELSE 1 END,
    o.updated_at DESC,o.operation_id DESC) selection_rank
  FROM requested r JOIN media_operations o ON o.source_id=r.source_id
  WHERE o.principal=? AND o.verb IN ('submit_supplied_media','submit_remote_recording')
   AND (?=0 OR json_extract(o.receipt_json,'$.source_version_id')=r.source_version_id
    OR COALESCE(json_extract(o.receipt_json,'$.source_version_id'),'')='')
 ), receipts AS (
  SELECT source_id,source_version_id,receipt_json,0 processing,'' created_at,'' operation_id
  FROM retention WHERE selection_rank=1
  UNION ALL
  SELECT r.source_id,r.source_version_id,o.receipt_json,1,o.created_at,o.operation_id
  FROM requested r JOIN media_operations o ON o.source_id=r.source_id
  WHERE o.principal=? AND r.source_version_id<>''
   AND o.verb IN ('submit_supplied_media','retry_media')
   AND o.receipt_json LIKE '%"processing_profile"%'
   AND json_extract(o.receipt_json,'$.source_version_id')=r.source_version_id
 ) SELECT source_id,source_version_id,receipt_json,processing FROM receipts
 ORDER BY processing,created_at DESC,operation_id DESC`,
		string(encoded), exactVersion, principal, exactVersion, principal)
	if err != nil {
		return err
	}
	defer func(rows *sql.Rows) { _ = rows.Close() }(rows)
	retained := make(map[MediaSourceVersionKey]bool, len(keys))
	for rows.Next() {
		var key MediaSourceVersionKey
		var raw string
		var processing bool
		if err := rows.Scan(&key.SourceID, &key.SourceVersionID, &raw, &processing); err != nil {
			return err
		}
		receipt, err := canonical.Decode[MediaPublicationReceipt]([]byte(raw))
		if err != nil {
			return fmt.Errorf("decoding media receipt: %w", err)
		}
		item := result[key]
		if processing {
			item.ProcessingReceipts = append(item.ProcessingReceipts, receipt)
		} else {
			item.Receipt = receipt
			retained[key] = true
		}
		result[key] = item
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, key := range keys {
		if !retained[key] {
			delete(result, key)
		}
	}
	return nil
}

func (s *Store) mediaSourceProjection(
	ctx context.Context, principal, sourceID string,
) (MediaSourceProjection, error) {
	var item MediaSourceProjection
	query := `SELECT o.source_id,s.kind,COALESCE(o.source_version_id,''),
		COALESCE(v.content_version_id,''),o.occurrence_id,o.caller_filename,o.message_json
		FROM media_occurrences o
		JOIN media_sources s ON s.source_id=o.source_id
		LEFT JOIN media_source_versions v ON v.source_version_id=o.source_version_id
		WHERE o.caller_principal=? AND o.visible=1 AND o.source_id=?`
	args := []any{principal, sourceID}
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
	key := MediaSourceVersionKey{item.SourceID, item.SourceVersionID}
	items := map[MediaSourceVersionKey]MediaSourceProjection{key: item}
	if err := s.mediaSourceReceipts(ctx, s.db, principal, items, false); err != nil {
		return MediaSourceProjection{}, err
	}
	item, ok := items[key]
	if !ok {
		return MediaSourceProjection{}, ErrNotFound
	}
	return item, nil
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
