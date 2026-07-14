package metadatav2

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Export writes a deterministic zero-scope metadata-v2 JSONL stream from one
// pinned SQLite snapshot.
func Export(ctx context.Context, db *sql.DB, w io.Writer) error {
	if db == nil {
		return errors.New("exporting metadata v2: nil database")
	}
	if w == nil {
		return errors.New("exporting metadata v2: nil writer")
	}
	if err := requireForeignKeys(ctx, db); err != nil {
		return fmt.Errorf("exporting metadata v2: %w", err)
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("beginning metadata v2 export: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	write := func(record any) error {
		if err := enc.Encode(record); err != nil {
			return fmt.Errorf("encoding metadata v2: %w", err)
		}
		return nil
	}

	var header Header
	header.Type = "meta"
	header.Format = Format
	header.Version = FormatVersion
	if err := tx.QueryRowContext(ctx,
		`SELECT vault_id FROM vault_metadata WHERE singleton = 1 AND format_version = 2`,
	).Scan(&header.VaultID); err != nil {
		return fmt.Errorf("reading metadata v2 header: %w", err)
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'nodes'), 0)`,
	).Scan(&header.NodeSequence); err != nil {
		return fmt.Errorf("reading metadata v2 node sequence: %w", err)
	}
	if err := validateTx(ctx, tx, header.NodeSequence); err != nil {
		return fmt.Errorf("validating metadata v2 export: %w", err)
	}
	if err := write(header); err != nil {
		return err
	}

	exporters := []func(context.Context, *sql.Tx, func(any) error) error{
		exportBlobs,
		exportNodes,
		exportContentVersions,
		exportIngests,
		exportProvenance,
		exportTags,
		exportNodeTags,
		exportExtractedText,
	}
	for _, export := range exporters {
		if err := export(ctx, tx, write); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("closing metadata v2 export snapshot: %w", err)
	}
	return nil
}

func exportBlobs(ctx context.Context, tx *sql.Tx, write func(any) error) error {
	rows, err := tx.QueryContext(ctx, `SELECT hash,size,created_at FROM blobs ORDER BY hash`)
	if err != nil {
		return fmt.Errorf("exporting metadata v2 blobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := Blob{Type: "blob"}
		if err := rows.Scan(&record.Hash, &record.Size, &record.CreatedAt); err != nil {
			return fmt.Errorf("scanning metadata v2 blob: %w", err)
		}
		if err := write(record); err != nil {
			return err
		}
	}
	return rows.Err()
}

func exportNodes(ctx context.Context, tx *sql.Tx, write func(any) error) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT id,parent_id,name,kind,current_version_id,revision,created_at,modified_at,
		       trashed_at,trash_parent,trash_name
		FROM nodes ORDER BY id`)
	if err != nil {
		return fmt.Errorf("exporting metadata v2 nodes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := Node{Type: "node"}
		var parent, trashParent sql.NullInt64
		var current, trashed sql.NullString
		var name, trashName any
		if err := rows.Scan(
			&record.ID, &parent, &name, &record.Kind, &current, &record.Revision,
			&record.CreatedAt, &record.ModifiedAt, &trashed, &trashParent, &trashName,
		); err != nil {
			return fmt.Errorf("scanning metadata v2 node: %w", err)
		}
		record.ParentID = nullInt64(parent)
		record.CurrentVersionID = nullString(current)
		record.TrashedAt = nullString(trashed)
		record.TrashParent = nullInt64(trashParent)
		parsedName, err := requiredBytes("node name", name)
		if err != nil {
			return err
		}
		record.Name = parsedName
		record.TrashName, err = optionalBytes("node trash name", trashName)
		if err != nil {
			return err
		}
		if err := write(record); err != nil {
			return err
		}
	}
	return rows.Err()
}

func exportContentVersions(ctx context.Context, tx *sql.Tx, write func(any) error) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT version_id,node_id,blob_hash,size,media_type,recorded_at,node_revision,
		       version_origin,introduced_operation_id,transition_kind,source_version_id
		FROM content_versions ORDER BY node_id,version_id`)
	if err != nil {
		return fmt.Errorf("exporting metadata v2 content versions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := ContentVersion{Type: "content_version"}
		var mediaType, operationID, transition, source sql.NullString
		var nodeRevision sql.NullInt64
		if err := rows.Scan(
			&record.VersionID, &record.NodeID, &record.BlobHash, &record.Size,
			&mediaType, &record.RecordedAt, &nodeRevision, &record.VersionOrigin,
			&operationID, &transition, &source,
		); err != nil {
			return fmt.Errorf("scanning metadata v2 content version: %w", err)
		}
		record.MediaType = nullString(mediaType)
		record.NodeRevision = nullInt64(nodeRevision)
		record.IntroducedOperationID = nullString(operationID)
		record.TransitionKind = nullString(transition)
		record.SourceVersionID = nullString(source)
		if err := write(record); err != nil {
			return err
		}
	}
	return rows.Err()
}

func exportIngests(ctx context.Context, tx *sql.Tx, write func(any) error) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT ingest_id,started_at,source_kind,source_desc FROM ingests ORDER BY ingest_id`)
	if err != nil {
		return fmt.Errorf("exporting metadata v2 ingests: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := Ingest{Type: "ingest"}
		var sourceDesc any
		if err := rows.Scan(
			&record.IngestID, &record.StartedAt, &record.SourceKind, &sourceDesc,
		); err != nil {
			return fmt.Errorf("scanning metadata v2 ingest: %w", err)
		}
		record.SourceDesc, err = requiredBytes("ingest source description", sourceDesc)
		if err != nil {
			return err
		}
		if err := write(record); err != nil {
			return err
		}
	}
	return rows.Err()
}

func exportProvenance(ctx context.Context, tx *sql.Tx, write func(any) error) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT identity,node_id,ingest_id,original_path,original_mtime,supersedes
		FROM provenance ORDER BY identity`)
	if err != nil {
		return fmt.Errorf("exporting metadata v2 provenance: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := Provenance{Type: "provenance"}
		var originalPath any
		var originalMTime, supersedes sql.NullString
		if err := rows.Scan(
			&record.Identity, &record.NodeID, &record.IngestID, &originalPath,
			&originalMTime, &supersedes,
		); err != nil {
			return fmt.Errorf("scanning metadata v2 provenance: %w", err)
		}
		var err error
		record.OriginalPath, err = optionalBytes("provenance original path", originalPath)
		if err != nil {
			return err
		}
		record.OriginalMTime = nullString(originalMTime)
		record.Supersedes = nullString(supersedes)
		if err := write(record); err != nil {
			return err
		}
	}
	return rows.Err()
}

func exportTags(ctx context.Context, tx *sql.Tx, write func(any) error) error {
	rows, err := tx.QueryContext(ctx, `SELECT tag_id,name FROM tags ORDER BY tag_id`)
	if err != nil {
		return fmt.Errorf("exporting metadata v2 tags: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := Tag{Type: "tag"}
		if err := rows.Scan(&record.TagID, &record.Name); err != nil {
			return fmt.Errorf("scanning metadata v2 tag: %w", err)
		}
		if err := write(record); err != nil {
			return err
		}
	}
	return rows.Err()
}

func exportNodeTags(ctx context.Context, tx *sql.Tx, write func(any) error) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT node_id,tag_id FROM node_tags ORDER BY node_id,tag_id`)
	if err != nil {
		return fmt.Errorf("exporting metadata v2 node tags: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := NodeTag{Type: "node_tag"}
		if err := rows.Scan(&record.NodeID, &record.TagID); err != nil {
			return fmt.Errorf("scanning metadata v2 node tag: %w", err)
		}
		if err := write(record); err != nil {
			return err
		}
	}
	return rows.Err()
}

func exportExtractedText(ctx context.Context, tx *sql.Tx, write func(any) error) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT blob_hash,extractor,extractor_version,status,error,attempts,text,extracted_at
		FROM extracted_text ORDER BY blob_hash,extractor`)
	if err != nil {
		return fmt.Errorf("exporting metadata v2 extracted text: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := ExtractedText{Type: "extracted_text"}
		var errorText, text sql.NullString
		if err := rows.Scan(
			&record.BlobHash, &record.Extractor, &record.ExtractorVersion,
			&record.Status, &errorText, &record.Attempts, &text, &record.ExtractedAt,
		); err != nil {
			return fmt.Errorf("scanning metadata v2 extracted text: %w", err)
		}
		record.Error = nullString(errorText)
		record.Text = nullString(text)
		if err := write(record); err != nil {
			return err
		}
	}
	return rows.Err()
}

func nullInt64(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	return new(value.Int64)
}

func nullString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return new(value.String)
}

func requiredBytes(field string, value any) (OpaqueBytes, error) {
	parsed, err := bytesValue(field, value)
	if err != nil {
		return nil, err
	}
	if parsed == nil {
		return nil, fmt.Errorf("%s is null", field)
	}
	return *parsed, nil
}

func optionalBytes(field string, value any) (*OpaqueBytes, error) {
	return bytesValue(field, value)
}

func bytesValue(field string, value any) (*OpaqueBytes, error) {
	if value == nil {
		//nolint:nilnil // SQL NULL is the successful absent optional value.
		return nil, nil
	}
	switch value := value.(type) {
	case []byte:
		copyValue := OpaqueBytes(append([]byte(nil), value...))
		return &copyValue, nil
	case string:
		copyValue := OpaqueBytes([]byte(value))
		return &copyValue, nil
	default:
		return nil, fmt.Errorf("%s has unsupported SQLite type %T", field, value)
	}
}
