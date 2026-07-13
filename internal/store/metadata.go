package store

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"go.kenn.io/kit/packstore"
)

const metadataFormatVersion = 1

type metadataHeader struct {
	Type         string `json:"type"`
	Format       string `json:"format"`
	Version      int    `json:"version"`
	NodeSequence int64  `json:"node_sequence"`
}

type metadataBlob struct {
	Type      string `json:"type"`
	Hash      string `json:"hash"`
	Size      int64  `json:"size"`
	CreatedAt string `json:"created_at"`
}

type metadataNode struct {
	Type        string  `json:"type"`
	ID          int64   `json:"id"`
	ParentID    *int64  `json:"parent_id"`
	Name        string  `json:"name"`
	Kind        string  `json:"kind"`
	BlobHash    *string `json:"blob_hash"`
	Size        *int64  `json:"size"`
	MIMEType    *string `json:"mime_type"`
	Revision    int64   `json:"revision"`
	CreatedAt   string  `json:"created_at"`
	ModifiedAt  string  `json:"modified_at"`
	TrashedAt   *string `json:"trashed_at"`
	TrashParent *int64  `json:"trash_parent"`
	TrashName   *string `json:"trash_name"`
}

type metadataNodeVersion struct {
	Type       string `json:"type"`
	NodeID     int64  `json:"node_id"`
	BlobHash   string `json:"blob_hash"`
	Size       int64  `json:"size"`
	ReplacedAt string `json:"replaced_at"`
}

type metadataIngest struct {
	Type       string `json:"type"`
	ID         int64  `json:"id"`
	StartedAt  string `json:"started_at"`
	SourceKind string `json:"source_kind"`
	SourceDesc string `json:"source_desc"`
}

type metadataProvenance struct {
	Type          string  `json:"type"`
	NodeID        int64   `json:"node_id"`
	IngestID      int64   `json:"ingest_id"`
	OriginalPath  string  `json:"original_path"`
	OriginalMTime *string `json:"original_mtime"`
}

type metadataTag struct {
	Type string `json:"type"`
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type metadataNodeTag struct {
	Type   string `json:"type"`
	NodeID int64  `json:"node_id"`
	TagID  int64  `json:"tag_id"`
}

type metadataExtractedText struct {
	Type             string  `json:"type"`
	BlobHash         string  `json:"blob_hash"`
	Extractor        string  `json:"extractor"`
	ExtractorVersion int64   `json:"extractor_version"`
	Status           string  `json:"status"`
	Error            *string `json:"error"`
	Attempts         int64   `json:"attempts"`
	Text             *string `json:"text"`
	ExtractedAt      string  `json:"extracted_at"`
}

// ExportMetadata writes a deterministic JSONL description of Docbank's
// logical state. Rebuildable FTS data and physical pack authority are omitted.
func (s *Store) ExportMetadata(ctx context.Context, w io.Writer) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("beginning metadata snapshot: %w", err)
	}
	if err := ExportMetadataTx(ctx, tx, w); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing metadata snapshot: %w", err)
	}
	return nil
}

// ExportMetadataTx writes metadata from an already pinned SQLite snapshot.
// Backup capture uses this entry point so metadata and blob membership come
// from the same frozen transaction.
func ExportMetadataTx(ctx context.Context, tx *sql.Tx, w io.Writer) error {
	if tx == nil {
		return errors.New("exporting metadata: nil transaction")
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	write := func(v any) error {
		if err := enc.Encode(v); err != nil {
			return fmt.Errorf("encoding metadata: %w", err)
		}
		return nil
	}
	var nodeSequence int64
	if err := tx.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name = 'nodes'`).Scan(&nodeSequence); err != nil {
		return fmt.Errorf("reading node ID high-water mark: %w", err)
	}
	if err := write(metadataHeader{
		Type: "meta", Format: "docbank-metadata", Version: metadataFormatVersion, NodeSequence: nodeSequence,
	}); err != nil {
		return err
	}
	if err := exportBlobs(ctx, tx, write); err != nil {
		return err
	}
	if err := exportNodes(ctx, tx, write); err != nil {
		return err
	}
	if err := exportIngests(ctx, tx, write); err != nil {
		return err
	}
	if err := exportNodeVersions(ctx, tx, write); err != nil {
		return err
	}
	if err := exportProvenance(ctx, tx, write); err != nil {
		return err
	}
	if err := exportTags(ctx, tx, write); err != nil {
		return err
	}
	if err := exportNodeTags(ctx, tx, write); err != nil {
		return err
	}
	return exportExtractedText(ctx, tx, write)
}

type metadataWrite func(any) error

func exportBlobs(ctx context.Context, tx *sql.Tx, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `SELECT hash, size, created_at FROM blobs ORDER BY hash`)
	if err != nil {
		return fmt.Errorf("exporting blobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		r := metadataBlob{Type: "blob"}
		if err := rows.Scan(&r.Hash, &r.Size, &r.CreatedAt); err != nil {
			return fmt.Errorf("scanning blob metadata: %w", err)
		}
		if err := write(r); err != nil {
			return err
		}
	}
	return rowsError("blob", rows)
}

func exportNodes(ctx context.Context, tx *sql.Tx, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, parent_id, name, kind, blob_hash, size, mime_type, revision,
		       created_at, modified_at, trashed_at, trash_parent, trash_name
		FROM nodes ORDER BY id`)
	if err != nil {
		return fmt.Errorf("exporting nodes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		r := metadataNode{Type: "node"}
		var parent, size, trashParent sql.NullInt64
		var blobHash, mimeType, trashedAt, trashName sql.NullString
		if err := rows.Scan(&r.ID, &parent, &r.Name, &r.Kind, &blobHash, &size, &mimeType,
			&r.Revision, &r.CreatedAt, &r.ModifiedAt, &trashedAt, &trashParent, &trashName); err != nil {
			return fmt.Errorf("scanning node metadata: %w", err)
		}
		r.ParentID, r.Size, r.TrashParent = int64Ptr(parent), int64Ptr(size), int64Ptr(trashParent)
		r.BlobHash, r.MIMEType = stringPtr(blobHash), stringPtr(mimeType)
		r.TrashedAt, r.TrashName = stringPtr(trashedAt), stringPtr(trashName)
		if err := write(r); err != nil {
			return err
		}
	}
	return rowsError("node", rows)
}

func exportNodeVersions(ctx context.Context, tx *sql.Tx, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT node_id, blob_hash, size, replaced_at FROM node_versions
		ORDER BY node_id, replaced_at, blob_hash, size`)
	if err != nil {
		return fmt.Errorf("exporting node versions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		r := metadataNodeVersion{Type: "node_version"}
		if err := rows.Scan(&r.NodeID, &r.BlobHash, &r.Size, &r.ReplacedAt); err != nil {
			return fmt.Errorf("scanning node version metadata: %w", err)
		}
		if err := write(r); err != nil {
			return err
		}
	}
	return rowsError("node version", rows)
}

func exportIngests(ctx context.Context, tx *sql.Tx, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, started_at, source_kind, source_desc FROM ingests ORDER BY id`)
	if err != nil {
		return fmt.Errorf("exporting ingests: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		r := metadataIngest{Type: "ingest"}
		if err := rows.Scan(&r.ID, &r.StartedAt, &r.SourceKind, &r.SourceDesc); err != nil {
			return fmt.Errorf("scanning ingest metadata: %w", err)
		}
		if err := write(r); err != nil {
			return err
		}
	}
	return rowsError("ingest", rows)
}

func exportProvenance(ctx context.Context, tx *sql.Tx, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT node_id, ingest_id, original_path, original_mtime FROM provenance
		ORDER BY node_id, ingest_id, original_path, original_mtime`)
	if err != nil {
		return fmt.Errorf("exporting provenance: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		r := metadataProvenance{Type: "provenance"}
		var mtime sql.NullString
		if err := rows.Scan(&r.NodeID, &r.IngestID, &r.OriginalPath, &mtime); err != nil {
			return fmt.Errorf("scanning provenance metadata: %w", err)
		}
		r.OriginalMTime = stringPtr(mtime)
		if err := write(r); err != nil {
			return err
		}
	}
	return rowsError("provenance", rows)
}

func exportTags(ctx context.Context, tx *sql.Tx, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, name FROM tags ORDER BY id`)
	if err != nil {
		return fmt.Errorf("exporting tags: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		r := metadataTag{Type: "tag"}
		if err := rows.Scan(&r.ID, &r.Name); err != nil {
			return fmt.Errorf("scanning tag metadata: %w", err)
		}
		if err := write(r); err != nil {
			return err
		}
	}
	return rowsError("tag", rows)
}

func exportNodeTags(ctx context.Context, tx *sql.Tx, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `SELECT node_id, tag_id FROM node_tags ORDER BY node_id, tag_id`)
	if err != nil {
		return fmt.Errorf("exporting node tags: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		r := metadataNodeTag{Type: "node_tag"}
		if err := rows.Scan(&r.NodeID, &r.TagID); err != nil {
			return fmt.Errorf("scanning node tag metadata: %w", err)
		}
		if err := write(r); err != nil {
			return err
		}
	}
	return rowsError("node tag", rows)
}

func exportExtractedText(ctx context.Context, tx *sql.Tx, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT blob_hash, extractor, extractor_version, status, error, attempts, text, extracted_at
		FROM extracted_text ORDER BY blob_hash, extractor`)
	if err != nil {
		return fmt.Errorf("exporting extracted text: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		r := metadataExtractedText{Type: "extracted_text"}
		var extractErr, text sql.NullString
		if err := rows.Scan(&r.BlobHash, &r.Extractor, &r.ExtractorVersion, &r.Status,
			&extractErr, &r.Attempts, &text, &r.ExtractedAt); err != nil {
			return fmt.Errorf("scanning extracted text metadata: %w", err)
		}
		r.Error, r.Text = stringPtr(extractErr), stringPtr(text)
		if err := write(r); err != nil {
			return err
		}
	}
	return rowsError("extracted text", rows)
}

func rowsError(kind string, rows *sql.Rows) error {
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating %s metadata: %w", kind, err)
	}
	return nil
}

func int64Ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}

func stringPtr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	return &v.String
}

// ImportMetadata replaces the pristine root in a newly created store with a
// logical JSONL snapshot. It refuses a store containing user or pack state.
func (s *Store) ImportMetadata(ctx context.Context, r io.Reader) error {
	rootID := int64(0)
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := requirePristineMetadataTarget(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM nodes`); err != nil {
			return fmt.Errorf("removing bootstrap root: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys = ON`); err != nil {
			return fmt.Errorf("deferring metadata foreign keys: %w", err)
		}
		nodeSequence, err := importMetadataLines(ctx, tx, r)
		if err != nil {
			return err
		}
		if err := validateImportedMetadata(ctx, tx); err != nil {
			return err
		}
		var maxNodeID int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM nodes`).Scan(&maxNodeID); err != nil {
			return fmt.Errorf("reading imported maximum node ID: %w", err)
		}
		if nodeSequence < maxNodeID {
			return fmt.Errorf("node ID high-water mark %d is below imported maximum %d", nodeSequence, maxNodeID)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sqlite_sequence SET seq = ? WHERE name = 'nodes'`, nodeSequence); err != nil {
			return fmt.Errorf("restoring node ID high-water mark: %w", err)
		}
		if err := tx.QueryRowContext(ctx, `SELECT id FROM nodes WHERE parent_id IS NULL`).Scan(&rootID); err != nil {
			return fmt.Errorf("finding imported root: %w", err)
		}
		rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
		if err != nil {
			return fmt.Errorf("checking imported foreign keys: %w", err)
		}
		defer func() { _ = rows.Close() }()
		if rows.Next() {
			var table string
			var rowID, parent any
			var fk int64
			if err := rows.Scan(&table, &rowID, &parent, &fk); err != nil {
				return fmt.Errorf("reading imported foreign-key failure: %w", err)
			}
			return fmt.Errorf("imported metadata violates foreign key %s[%v] constraint %d", table, rowID, fk)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	s.rootID = rootID
	return nil
}

func requirePristineMetadataTarget(ctx context.Context, tx *sql.Tx) error {
	var nodes, other, packs int64
	if err := tx.QueryRowContext(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM nodes),
		  (SELECT COUNT(*) FROM blobs) + (SELECT COUNT(*) FROM node_versions)
		    + (SELECT COUNT(*) FROM ingests) + (SELECT COUNT(*) FROM provenance)
		    + (SELECT COUNT(*) FROM tags) + (SELECT COUNT(*) FROM node_tags)
		    + (SELECT COUNT(*) FROM extracted_text),
		  (SELECT COUNT(*) FROM blob_packs) + (SELECT COUNT(*) FROM blob_pack_index)
	`).Scan(&nodes, &other, &packs); err != nil {
		return fmt.Errorf("checking metadata import target: %w", err)
	}
	if nodes != 1 || other != 0 || packs != 0 {
		return fmt.Errorf("metadata import target is not pristine: nodes=%d logical_rows=%d pack_rows=%d",
			nodes, other, packs)
	}
	return nil
}

func importMetadataLines(ctx context.Context, tx *sql.Tx, r io.Reader) (int64, error) {
	dec := json.NewDecoder(bufio.NewReader(r))
	dec.DisallowUnknownFields()
	var header metadataHeader
	if err := dec.Decode(&header); err != nil {
		return 0, fmt.Errorf("decoding metadata header: %w", err)
	}
	if header.Type != "meta" || header.Format != "docbank-metadata" ||
		header.Version != metadataFormatVersion || header.NodeSequence <= 0 {
		return 0, fmt.Errorf("unsupported metadata header: type=%q format=%q version=%d node_sequence=%d",
			header.Type, header.Format, header.Version, header.NodeSequence)
	}
	for record := 2; ; record++ {
		var raw json.RawMessage
		err := dec.Decode(&raw)
		if errors.Is(err, io.EOF) {
			return header.NodeSequence, nil
		}
		if err != nil {
			return 0, fmt.Errorf("decoding metadata record %d: %w", record, err)
		}
		var kind struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &kind); err != nil {
			return 0, fmt.Errorf("decoding metadata record %d type: %w", record, err)
		}
		if err := importMetadataRecord(ctx, tx, kind.Type, raw); err != nil {
			return 0, fmt.Errorf("importing metadata record %d (%s): %w", record, kind.Type, err)
		}
	}
}

func importMetadataRecord(ctx context.Context, tx *sql.Tx, kind string, raw json.RawMessage) error {
	required, ok := metadataRequiredFields[kind]
	if !ok {
		return fmt.Errorf("unknown record type %q", kind)
	}
	if err := requireMetadataFields(raw, required); err != nil {
		return err
	}
	decode := func(dst any) error {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		return dec.Decode(dst)
	}
	switch kind {
	case "blob":
		var v metadataBlob
		if err := decode(&v); err != nil {
			return err
		}
		if err := validateBlobRecord(v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO blobs(hash,size,created_at) VALUES(?,?,?)`, v.Hash, v.Size, v.CreatedAt)
		return err
	case "node":
		var v metadataNode
		if err := decode(&v); err != nil {
			return err
		}
		if err := validateNodeRecord(v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO nodes(id,parent_id,name,kind,blob_hash,size,mime_type,revision,created_at,modified_at,trashed_at,trash_parent,trash_name) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			v.ID, v.ParentID, v.Name, v.Kind, v.BlobHash, v.Size, v.MIMEType, v.Revision, v.CreatedAt, v.ModifiedAt, v.TrashedAt, v.TrashParent, v.TrashName)
		return err
	case "node_version":
		var v metadataNodeVersion
		if err := decode(&v); err != nil {
			return err
		}
		if err := validateNodeVersionRecord(v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO node_versions(node_id,blob_hash,size,replaced_at) VALUES(?,?,?,?)`, v.NodeID, v.BlobHash, v.Size, v.ReplacedAt)
		return err
	case "ingest":
		var v metadataIngest
		if err := decode(&v); err != nil {
			return err
		}
		if v.Type != kind || v.ID <= 0 || v.SourceKind == "" || v.SourceDesc == "" {
			return errors.New("invalid ingest record")
		}
		if err := validateMetadataTime("ingest started_at", v.StartedAt); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO ingests(id,started_at,source_kind,source_desc) VALUES(?,?,?,?)`, v.ID, v.StartedAt, v.SourceKind, v.SourceDesc)
		return err
	case "provenance":
		var v metadataProvenance
		if err := decode(&v); err != nil {
			return err
		}
		if v.Type != kind || v.NodeID <= 0 || v.IngestID <= 0 || v.OriginalPath == "" {
			return errors.New("invalid provenance record")
		}
		if v.OriginalMTime != nil {
			if err := validateMetadataTime("provenance original_mtime", *v.OriginalMTime); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO provenance(node_id,ingest_id,original_path,original_mtime) VALUES(?,?,?,?)`, v.NodeID, v.IngestID, v.OriginalPath, v.OriginalMTime)
		return err
	case "tag":
		var v metadataTag
		if err := decode(&v); err != nil {
			return err
		}
		if v.Type != kind || v.ID <= 0 || v.Name == "" {
			return errors.New("invalid tag record")
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO tags(id,name) VALUES(?,?)`, v.ID, v.Name)
		return err
	case "node_tag":
		var v metadataNodeTag
		if err := decode(&v); err != nil {
			return err
		}
		if v.Type != kind || v.NodeID <= 0 || v.TagID <= 0 {
			return errors.New("invalid node tag record")
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO node_tags(node_id,tag_id) VALUES(?,?)`, v.NodeID, v.TagID)
		return err
	case "extracted_text":
		var v metadataExtractedText
		if err := decode(&v); err != nil {
			return err
		}
		if err := validateExtractedTextRecord(v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO extracted_text(blob_hash,extractor,extractor_version,status,error,attempts,text,extracted_at) VALUES(?,?,?,?,?,?,?,?)`,
			v.BlobHash, v.Extractor, v.ExtractorVersion, v.Status, v.Error, v.Attempts, v.Text, v.ExtractedAt)
		return err
	default:
		return fmt.Errorf("unknown record type %q", kind)
	}
}

var metadataRequiredFields = map[string][]string{
	"blob":           {"type", "hash", "size", "created_at"},
	"node":           {"type", "id", "parent_id", "name", "kind", "blob_hash", "size", "mime_type", "revision", "created_at", "modified_at", "trashed_at", "trash_parent", "trash_name"},
	"node_version":   {"type", "node_id", "blob_hash", "size", "replaced_at"},
	"ingest":         {"type", "id", "started_at", "source_kind", "source_desc"},
	"provenance":     {"type", "node_id", "ingest_id", "original_path", "original_mtime"},
	"tag":            {"type", "id", "name"},
	"node_tag":       {"type", "node_id", "tag_id"},
	"extracted_text": {"type", "blob_hash", "extractor", "extractor_version", "status", "error", "attempts", "text", "extracted_at"},
}

func requireMetadataFields(raw json.RawMessage, required []string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("decoding metadata fields: %w", err)
	}
	for _, field := range required {
		if _, ok := fields[field]; !ok {
			return fmt.Errorf("metadata record lacks required field %q", field)
		}
	}
	return nil
}

func validateBlobRecord(v metadataBlob) error {
	if v.Type != "blob" || v.Size < 0 {
		return errors.New("invalid blob record")
	}
	if _, err := packstore.ParseHash(v.Hash); err != nil {
		return fmt.Errorf("invalid blob hash: %w", err)
	}
	return validateMetadataTime("blob created_at", v.CreatedAt)
}

func validateNodeRecord(v metadataNode) error {
	if v.Type != "node" || v.ID <= 0 || v.Revision <= 0 {
		return errors.New("invalid node record")
	}
	if err := validateMetadataTime("node created_at", v.CreatedAt); err != nil {
		return err
	}
	if err := validateMetadataTime("node modified_at", v.ModifiedAt); err != nil {
		return err
	}
	if v.TrashedAt != nil {
		if err := validateMetadataTime("node trashed_at", *v.TrashedAt); err != nil {
			return err
		}
	}
	if v.ParentID == nil {
		if v.Name != "" || v.Kind != "dir" || v.BlobHash != nil || v.Size != nil || v.TrashedAt != nil ||
			v.TrashParent != nil || v.TrashName != nil {
			return errors.New("invalid root node record")
		}
		return nil
	}
	if *v.ParentID <= 0 {
		return errors.New("invalid node parent_id")
	}
	normalized, err := NormalizeName(v.Name)
	if err != nil || normalized != v.Name {
		return fmt.Errorf("invalid node name %q", v.Name)
	}
	switch v.Kind {
	case "dir":
		if v.BlobHash != nil || v.Size != nil || v.MIMEType != nil {
			return errors.New("directory record carries file content")
		}
	case "file":
		if v.BlobHash == nil || v.Size == nil || *v.Size < 0 {
			return errors.New("file record lacks valid content identity")
		}
		if _, err := packstore.ParseHash(*v.BlobHash); err != nil {
			return fmt.Errorf("invalid node blob hash: %w", err)
		}
	default:
		return fmt.Errorf("invalid node kind %q", v.Kind)
	}
	if (v.TrashParent == nil) != (v.TrashName == nil) || (v.TrashParent != nil && v.TrashedAt == nil) {
		return errors.New("incomplete node trash coordinates")
	}
	return nil
}

func validateNodeVersionRecord(v metadataNodeVersion) error {
	if v.Type != "node_version" || v.NodeID <= 0 || v.Size < 0 {
		return errors.New("invalid node version record")
	}
	if _, err := packstore.ParseHash(v.BlobHash); err != nil {
		return fmt.Errorf("invalid node version blob hash: %w", err)
	}
	return validateMetadataTime("node version replaced_at", v.ReplacedAt)
}

func validateExtractedTextRecord(v metadataExtractedText) error {
	if v.Type != "extracted_text" || v.Extractor == "" || v.ExtractorVersion < 0 || v.Attempts < 0 {
		return errors.New("invalid extracted text record")
	}
	if v.Status != "ok" && v.Status != "failed" {
		return fmt.Errorf("invalid extraction status %q", v.Status)
	}
	if _, err := packstore.ParseHash(v.BlobHash); err != nil {
		return fmt.Errorf("invalid extracted text blob hash: %w", err)
	}
	return validateMetadataTime("extracted text extracted_at", v.ExtractedAt)
}

func validateMetadataTime(field, value string) error {
	parsed, err := time.Parse(timestampLayout, value)
	if err != nil {
		return fmt.Errorf("invalid %s: %w", field, err)
	}
	if value != parsed.UTC().Format(timestampLayout) {
		return fmt.Errorf("invalid %s: timestamp is not canonical UTC", field)
	}
	return nil
}

func validateImportedMetadata(ctx context.Context, tx *sql.Tx) error {
	checks := []struct {
		name  string
		query string
	}{
		{"tree contains unreachable nodes", `
			WITH RECURSIVE reachable(id) AS (
			  SELECT id FROM nodes WHERE parent_id IS NULL
			  UNION SELECT n.id FROM nodes n JOIN reachable r ON n.parent_id = r.id
			)
			SELECT (SELECT COUNT(*) FROM reachable) != (SELECT COUNT(*) FROM nodes)`},
		{"node parent is not a directory", `
			SELECT EXISTS(SELECT 1 FROM nodes n JOIN nodes p ON p.id=n.parent_id WHERE p.kind != 'dir')`},
		{"trash parent is not a directory", `
			SELECT EXISTS(SELECT 1 FROM nodes n JOIN nodes p ON p.id=n.trash_parent WHERE p.kind != 'dir')`},
		{"node size differs from blob authority", `
			SELECT EXISTS(SELECT 1 FROM nodes n JOIN blobs b ON b.hash=n.blob_hash WHERE n.size != b.size)`},
		{"node version size differs from blob authority", `
			SELECT EXISTS(SELECT 1 FROM node_versions v JOIN blobs b ON b.hash=v.blob_hash WHERE v.size != b.size)`},
		{"extracted text references missing blob authority", `
			SELECT EXISTS(SELECT 1 FROM extracted_text e LEFT JOIN blobs b ON b.hash=e.blob_hash WHERE b.hash IS NULL)`},
	}
	for _, check := range checks {
		var failed bool
		if err := tx.QueryRowContext(ctx, check.query).Scan(&failed); err != nil {
			return fmt.Errorf("validating imported metadata (%s): %w", check.name, err)
		}
		if failed {
			return errors.New(check.name)
		}
	}
	return nil
}
