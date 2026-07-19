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
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/jsontext"
	docsqlite "go.kenn.io/docbank/pkg/sqlite"
)

const metadataFormatVersion = 1

// MetadataSnapshot owns a dedicated deferred read transaction. Store's normal
// connections use BEGIN IMMEDIATE for mutations; using that pool here would
// hold the writer lock for the full backup instead of only pinning a WAL view.
type MetadataSnapshot struct {
	db *sql.DB
	tx *sql.Tx
}

func (s *MetadataSnapshot) QueryContext(
	ctx context.Context, query string, args ...any,
) (*sql.Rows, error) {
	return s.tx.QueryContext(ctx, query, args...)
}

func (s *MetadataSnapshot) QueryRowContext(
	ctx context.Context, query string, args ...any,
) *sql.Row {
	return s.tx.QueryRowContext(ctx, query, args...)
}

func (s *MetadataSnapshot) Close() error {
	rollbackErr := s.tx.Rollback()
	if errors.Is(rollbackErr, sql.ErrTxDone) {
		rollbackErr = nil
	}
	return errors.Join(rollbackErr, s.db.Close())
}

// Export writes the deterministic logical metadata held by this snapshot.
func (s *MetadataSnapshot) Export(ctx context.Context, w io.Writer) error {
	return exportMetadataSnapshot(ctx, s, w)
}

// BeginMetadataSnapshot establishes a pinned read transaction for logical
// backup capture. The initial read is required: BeginTx alone is lazy in
// SQLite and would not pin a snapshot before Kit releases the mutation gate.
func (s *Store) BeginMetadataSnapshot(ctx context.Context) (*MetadataSnapshot, error) {
	db, err := s.driver.Open(s.path, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Deferred,
	})
	if err != nil {
		return nil, fmt.Errorf("beginning metadata snapshot: %w", err)
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("beginning metadata snapshot: %w", err)
	}
	var schemaVersion int64
	if err := tx.QueryRowContext(ctx, `PRAGMA schema_version`).Scan(&schemaVersion); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		return nil, fmt.Errorf("pinning metadata snapshot: %w", err)
	}
	return &MetadataSnapshot{db: db, tx: tx}, nil
}

type metadataHeader struct {
	Type         string `json:"type"`
	Format       string `json:"format"`
	Version      int    `json:"version"`
	VaultID      string `json:"vault_id"`
	NodeSequence int64  `json:"node_sequence"`
}

type metadataBlob struct {
	Type      string `json:"type"`
	Hash      string `json:"hash"`
	Size      int64  `json:"size"`
	CreatedAt string `json:"created_at"`
}

type metadataNode struct {
	Type             string  `json:"type"`
	ID               int64   `json:"id"`
	ParentID         *int64  `json:"parent_id"`
	Name             string  `json:"name"`
	Kind             string  `json:"kind"`
	CurrentVersionID *string `json:"current_version_id"`
	Revision         int64   `json:"revision"`
	CreatedAt        string  `json:"created_at"`
	ModifiedAt       string  `json:"modified_at"`
	TrashedAt        *string `json:"trashed_at"`
	TrashParent      *int64  `json:"trash_parent"`
	TrashName        *string `json:"trash_name"`
}

type metadataContentVersion struct {
	Type                  string  `json:"type"`
	VersionID             string  `json:"version_id"`
	NodeID                int64   `json:"node_id"`
	BlobHash              string  `json:"blob_hash"`
	Size                  int64   `json:"size"`
	MIMEType              *string `json:"mime_type"`
	RecordedAt            string  `json:"recorded_at"`
	NodeRevision          int64   `json:"node_revision"`
	IntroducedOperationID string  `json:"introduced_operation_id"`
	TransitionKind        string  `json:"transition_kind"`
	SourceVersionID       *string `json:"source_version_id"`
}

type metadataIngest struct {
	Type       string `json:"type"`
	ID         string `json:"ingest_id"`
	StartedAt  string `json:"started_at"`
	SourceKind string `json:"source_kind"`
	SourceDesc string `json:"source_desc"`
}

type metadataProvenance struct {
	Type          string  `json:"type"`
	Identity      string  `json:"identity"`
	NodeID        int64   `json:"node_id"`
	IngestID      string  `json:"ingest_id"`
	OriginalPath  string  `json:"original_path"`
	OriginalMTime *string `json:"original_mtime"`
	Supersedes    *string `json:"supersedes"`
}

type metadataWatchSource struct {
	Type      string `json:"type"`
	WatchName string `json:"watch_name"`
	SourceRef string `json:"source_ref"`
	NodeID    int64  `json:"node_id"`
	BlobHash  string `json:"blob_hash"`
	Size      int64  `json:"size"`
}

type metadataTag struct {
	Type     string `json:"type"`
	ID       string `json:"tag_id"`
	Name     string `json:"name"`
	Revision int64  `json:"revision"`
}

type metadataNodeTag struct {
	Type   string `json:"type"`
	NodeID int64  `json:"node_id"`
	TagID  string `json:"tag_id"`
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

type metadataAuditRecord struct {
	Type   string          `json:"type"`
	Digest string          `json:"digest"`
	Record json.RawMessage `json:"record"`
}

type metadataAuditAuthority struct {
	Type                       string `json:"type"`
	LineageID                  string `json:"lineage_id"`
	OperationSequenceHighWater uint64 `json:"operation_sequence_high_water"`
	AllocationGenesisDigest    string `json:"allocation_genesis_digest"`
	AllocationEntryCount       uint64 `json:"allocation_entry_count"`
	AllocationHead             string `json:"allocation_head"`
}

type metadataAuditScope struct {
	Type              string `json:"type"`
	ScopeID           string `json:"scope_id"`
	TargetNodeID      uint64 `json:"target_node_id"`
	EnableOperationID string `json:"enable_operation_id"`
	EntryCount        uint64 `json:"entry_count"`
	ChainHead         string `json:"chain_head"`
}

type metadataAuditMembership struct {
	Type           string `json:"type"`
	ScopeID        string `json:"scope_id"`
	NodeID         uint64 `json:"node_id"`
	BaselineDigest string `json:"baseline_digest"`
}

// ExportMetadata writes a deterministic JSONL description of Docbank's
// logical state. Rebuildable FTS data and physical pack authority are omitted.
func (s *Store) ExportMetadata(ctx context.Context, w io.Writer) error {
	tx, err := s.BeginMetadataSnapshot(ctx)
	if err != nil {
		return err
	}
	if err := tx.Export(ctx, w); err != nil {
		_ = tx.Close()
		return err
	}
	if err := tx.Close(); err != nil {
		return fmt.Errorf("closing metadata snapshot: %w", err)
	}
	return nil
}

// ValidateMetadata verifies the current relational metadata and, when audit
// authority exists, independently replays its canonical history against the
// current projection. It exercises the same deterministic stream boundary as
// backup without publishing that stream or mutating the vault.
func (s *Store) ValidateMetadata(ctx context.Context) (err error) {
	snapshot, err := s.BeginMetadataSnapshot(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, snapshot.Close()) }()
	if err := snapshot.Export(ctx, io.Discard); err != nil {
		return fmt.Errorf("validating metadata: %w", err)
	}
	return nil
}

type metadataQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// exportMetadataSnapshot writes metadata from an already pinned SQLite snapshot.
// Backup capture uses this entry point so metadata and blob membership come
// from the same frozen transaction.
func exportMetadataSnapshot(ctx context.Context, tx metadataQuerier, w io.Writer) error {
	if tx == nil {
		return errors.New("exporting metadata: nil transaction")
	}
	var vaultID string
	if err := tx.QueryRowContext(ctx,
		`SELECT vault_id FROM vault_metadata WHERE singleton = 1`,
	).Scan(&vaultID); err != nil {
		return fmt.Errorf("reading vault identity: %w", err)
	}
	var nodeSequence int64
	if err := tx.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name = 'nodes'`).Scan(&nodeSequence); err != nil {
		return fmt.Errorf("reading node ID high-water mark: %w", err)
	}
	if err := validateMetadataState(ctx, tx, nodeSequence); err != nil {
		return fmt.Errorf("validating metadata snapshot: %w", err)
	}
	write := newMetadataJSONWriter(w)
	if err := write(metadataHeader{
		Type: "meta", Format: "docbank-metadata", Version: metadataFormatVersion,
		VaultID: vaultID, NodeSequence: nodeSequence,
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
	if err := exportContentVersions(ctx, tx, write); err != nil {
		return err
	}
	if err := exportProvenance(ctx, tx, write); err != nil {
		return err
	}
	if err := exportWatchSources(ctx, tx, write); err != nil {
		return err
	}
	if err := exportTags(ctx, tx, write); err != nil {
		return err
	}
	if err := exportNodeTags(ctx, tx, write); err != nil {
		return err
	}
	if err := exportExtractedText(ctx, tx, write); err != nil {
		return err
	}
	return exportAuditMetadata(ctx, tx, write)
}

type metadataWrite func(any) error

// newMetadataJSONWriter is the single JSONL encoding boundary used by exports
// and exact projected-size calculations. Keeping both on this encoder prevents
// previews from estimating a different wire representation than backups use.
func newMetadataJSONWriter(w io.Writer) metadataWrite {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return func(v any) error {
		if err := enc.Encode(v); err != nil {
			return fmt.Errorf("encoding metadata: %w", err)
		}
		return nil
	}
}

func exportBlobs(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
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
		if err := validateBlobRecord(r); err != nil {
			return fmt.Errorf("validating blob metadata for export: %w", err)
		}
		if err := write(r); err != nil {
			return err
		}
	}
	return rowsError("blob", rows)
}

func exportNodes(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, parent_id, name, kind, current_version_id, revision,
		       created_at, modified_at, trashed_at, trash_parent, trash_name
		FROM nodes ORDER BY id`)
	if err != nil {
		return fmt.Errorf("exporting nodes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		r := metadataNode{Type: "node"}
		var parent, trashParent sql.NullInt64
		var currentVersionID, trashedAt, trashName sql.NullString
		if err := rows.Scan(&r.ID, &parent, &r.Name, &r.Kind, &currentVersionID,
			&r.Revision, &r.CreatedAt, &r.ModifiedAt, &trashedAt, &trashParent, &trashName); err != nil {
			return fmt.Errorf("scanning node metadata: %w", err)
		}
		r.ParentID, r.TrashParent = int64Ptr(parent), int64Ptr(trashParent)
		r.CurrentVersionID = stringPtr(currentVersionID)
		r.TrashedAt, r.TrashName = stringPtr(trashedAt), stringPtr(trashName)
		if err := validateNodeRecord(r); err != nil {
			return fmt.Errorf("validating node metadata for export: %w", err)
		}
		if err := write(r); err != nil {
			return err
		}
	}
	return rowsError("node", rows)
}

func exportContentVersions(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT version_id, node_id, blob_hash, size, mime_type, recorded_at,
		       node_revision, introduced_operation_id, transition_kind, source_version_id
		FROM content_versions ORDER BY node_id, node_revision, version_id`)
	if err != nil {
		return fmt.Errorf("exporting content versions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		r := metadataContentVersion{Type: "content_version"}
		var mimeType, sourceVersionID sql.NullString
		if err := rows.Scan(&r.VersionID, &r.NodeID, &r.BlobHash, &r.Size,
			&mimeType, &r.RecordedAt, &r.NodeRevision, &r.IntroducedOperationID,
			&r.TransitionKind, &sourceVersionID); err != nil {
			return fmt.Errorf("scanning content version metadata: %w", err)
		}
		r.MIMEType, r.SourceVersionID = stringPtr(mimeType), stringPtr(sourceVersionID)
		if err := validateContentVersionRecord(r); err != nil {
			return fmt.Errorf("validating content version metadata for export: %w", err)
		}
		if err := write(r); err != nil {
			return err
		}
	}
	return rowsError("content version", rows)
}

func exportIngests(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, started_at, source_kind, source_desc FROM ingests ORDER BY id`)
	if err != nil {
		return fmt.Errorf("exporting ingests: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		r := metadataIngest{Type: metadataIngestType}
		if err := rows.Scan(&r.ID, &r.StartedAt, &r.SourceKind, &r.SourceDesc); err != nil {
			return fmt.Errorf("scanning ingest metadata: %w", err)
		}
		if err := validateIngestRecord(r); err != nil {
			return fmt.Errorf("validating ingest metadata for export: %w", err)
		}
		if err := write(r); err != nil {
			return err
		}
	}
	return rowsError(metadataIngestType, rows)
}

func exportProvenance(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT identity, node_id, ingest_id, original_path, original_mtime, supersedes
		FROM provenance ORDER BY identity`)
	if err != nil {
		return fmt.Errorf("exporting provenance: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		r := metadataProvenance{Type: metadataProvenanceType}
		var mtime, supersedes sql.NullString
		if err := rows.Scan(&r.Identity, &r.NodeID, &r.IngestID, &r.OriginalPath, &mtime, &supersedes); err != nil {
			return fmt.Errorf("scanning provenance metadata: %w", err)
		}
		r.OriginalMTime = stringPtr(mtime)
		r.Supersedes = stringPtr(supersedes)
		if err := validateProvenanceRecord(r); err != nil {
			return fmt.Errorf("validating provenance metadata for export: %w", err)
		}
		if err := write(r); err != nil {
			return err
		}
	}
	return rowsError(metadataProvenanceType, rows)
}

func exportWatchSources(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT watch_name, source_ref, node_id, blob_hash, size
		FROM watch_sources ORDER BY watch_name, source_ref`)
	if err != nil {
		return fmt.Errorf("exporting watched sources: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		r := metadataWatchSource{Type: metadataWatchSourceType}
		if err := rows.Scan(&r.WatchName, &r.SourceRef, &r.NodeID, &r.BlobHash, &r.Size); err != nil {
			return fmt.Errorf("scanning watched source metadata: %w", err)
		}
		if err := validateWatchSourceRecord(r); err != nil {
			return fmt.Errorf("validating watched source metadata for export: %w", err)
		}
		if err := write(r); err != nil {
			return err
		}
	}
	return rowsError(metadataWatchSourceType, rows)
}

func exportTags(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, name, revision FROM tags ORDER BY id`)
	if err != nil {
		return fmt.Errorf("exporting tags: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		r := metadataTag{Type: "tag"}
		if err := rows.Scan(&r.ID, &r.Name, &r.Revision); err != nil {
			return fmt.Errorf("scanning tag metadata: %w", err)
		}
		if err := validateTagRecord(r); err != nil {
			return fmt.Errorf("validating tag metadata for export: %w", err)
		}
		if err := write(r); err != nil {
			return err
		}
	}
	return rowsError("tag", rows)
}

func exportNodeTags(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
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
		if err := validateNodeTagRecord(r); err != nil {
			return fmt.Errorf("validating node tag metadata for export: %w", err)
		}
		if err := write(r); err != nil {
			return err
		}
	}
	return rowsError("node tag", rows)
}

func exportExtractedText(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
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
		if err := validateExtractedTextRecord(r); err != nil {
			return fmt.Errorf("validating extracted text metadata for export: %w", err)
		}
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
	vaultID := ""
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if err := requirePristineMetadataTarget(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM nodes`); err != nil {
			return fmt.Errorf("removing bootstrap root: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys = ON`); err != nil {
			return fmt.Errorf("deferring metadata foreign keys: %w", err)
		}
		header, err := importMetadataLines(ctx, tx, r)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE vault_metadata SET vault_id = ? WHERE singleton = 1`, header.VaultID,
		); err != nil {
			return fmt.Errorf("restoring vault identity: %w", err)
		}
		if err := validateMetadataState(ctx, tx, header.NodeSequence); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sqlite_sequence SET seq = ? WHERE name = 'nodes'`, header.NodeSequence); err != nil {
			return fmt.Errorf("restoring node ID high-water mark: %w", err)
		}
		if err := tx.QueryRowContext(ctx, `SELECT id FROM nodes WHERE parent_id IS NULL`).Scan(&rootID); err != nil {
			return fmt.Errorf("finding imported root: %w", err)
		}
		vaultID = header.VaultID
		return nil
	})
	if err != nil {
		return err
	}
	s.rootID = rootID
	s.vaultID = vaultID
	return nil
}

func requirePristineMetadataTarget(ctx context.Context, tx *sql.Tx) error {
	var nodes, other, packs int64
	if err := tx.QueryRowContext(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM nodes),
		  (SELECT COUNT(*) FROM blobs) + (SELECT COUNT(*) FROM content_versions)
		    + (SELECT COUNT(*) FROM ingests) + (SELECT COUNT(*) FROM provenance)
		    + (SELECT COUNT(*) FROM watch_sources)
		    + (SELECT COUNT(*) FROM tags) + (SELECT COUNT(*) FROM node_tags)
		    + (SELECT COUNT(*) FROM extracted_text)
		    + (SELECT COUNT(*) FROM text_extraction_queue)
		    + (SELECT COUNT(*) FROM text_searchable_versions)
		    + (SELECT COUNT(*) FROM content_fts)
		    + (SELECT COUNT(*) FROM audit_records)
		    + (SELECT COUNT(*) FROM audit_authority)
		    + (SELECT COUNT(*) FROM audit_scopes)
		    + (SELECT COUNT(*) FROM audit_baselines)
		    + (SELECT COUNT(*) FROM audit_memberships),
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

func importMetadataLines(ctx context.Context, tx *sql.Tx, r io.Reader) (metadataHeader, error) {
	dec := json.NewDecoder(bufio.NewReader(r))
	var rawHeader json.RawMessage
	if err := dec.Decode(&rawHeader); err != nil {
		return metadataHeader{}, fmt.Errorf("decoding metadata header: %w", err)
	}
	if err := validateMetadataJSON(rawHeader); err != nil {
		return metadataHeader{}, fmt.Errorf("decoding metadata header: %w", err)
	}
	if err := requireMetadataFields(rawHeader, metadataHeaderFields, nil); err != nil {
		return metadataHeader{}, fmt.Errorf("decoding metadata header: %w", err)
	}
	var header metadataHeader
	if err := decodeMetadataRecord(rawHeader, &header); err != nil {
		return metadataHeader{}, fmt.Errorf("decoding metadata header: %w", err)
	}
	if header.Type != "meta" || header.Format != "docbank-metadata" ||
		header.Version != metadataFormatVersion || header.NodeSequence <= 0 {
		return metadataHeader{}, fmt.Errorf("unsupported metadata header: type=%q format=%q version=%d node_sequence=%d",
			header.Type, header.Format, header.Version, header.NodeSequence)
	}
	if err := validateUUIDv4(header.VaultID); err != nil {
		return metadataHeader{}, fmt.Errorf("invalid metadata vault_id: %w", err)
	}
	for record := 2; ; record++ {
		var raw json.RawMessage
		err := dec.Decode(&raw)
		if errors.Is(err, io.EOF) {
			return header, nil
		}
		if err != nil {
			return metadataHeader{}, fmt.Errorf("decoding metadata record %d: %w", record, err)
		}
		if err := validateMetadataJSON(raw); err != nil {
			return metadataHeader{}, fmt.Errorf("decoding metadata record %d: %w", record, err)
		}
		var kind struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &kind); err != nil {
			return metadataHeader{}, fmt.Errorf("decoding metadata record %d type: %w", record, err)
		}
		if err := importMetadataRecord(ctx, tx, kind.Type, raw); err != nil {
			return metadataHeader{}, fmt.Errorf("importing metadata record %d (%s): %w", record, kind.Type, err)
		}
	}
}

func importMetadataRecord(ctx context.Context, tx *sql.Tx, kind string, raw json.RawMessage) error {
	required, ok := metadataRequiredFields[kind]
	if !ok {
		return fmt.Errorf("unknown record type %q", kind)
	}
	if err := requireMetadataFields(raw, required, metadataNullableFields[kind]); err != nil {
		return err
	}
	switch kind {
	case "blob":
		var v metadataBlob
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		if err := validateBlobRecord(v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO blobs(hash,size,created_at) VALUES(?,?,?)`, v.Hash, v.Size, v.CreatedAt)
		return err
	case "node":
		var v metadataNode
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		if err := validateNodeRecord(v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO nodes(id,parent_id,name,kind,current_version_id,revision,created_at,modified_at,trashed_at,trash_parent,trash_name) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			v.ID, v.ParentID, v.Name, v.Kind, v.CurrentVersionID, v.Revision, v.CreatedAt, v.ModifiedAt, v.TrashedAt, v.TrashParent, v.TrashName)
		return err
	case "content_version":
		var v metadataContentVersion
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		if err := validateContentVersionRecord(v); err != nil {
			return err
		}
		mimeType := ""
		if v.MIMEType != nil {
			mimeType = *v.MIMEType
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO content_versions(
			version_id,node_id,blob_hash,size,mime_type,recorded_at,node_revision,
			introduced_operation_id,transition_kind,source_version_id
		) VALUES(?,?,?,?,?,?,?,?,?,?)`, v.VersionID, v.NodeID, v.BlobHash, v.Size,
			v.MIMEType, v.RecordedAt, v.NodeRevision, v.IntroducedOperationID,
			v.TransitionKind, v.SourceVersionID)
		if err != nil {
			return err
		}
		return queueTextExtractionTx(tx, v.VersionID, v.BlobHash, mimeType)
	case metadataIngestType:
		var v metadataIngest
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		if err := validateIngestRecord(v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO ingests(id,started_at,source_kind,source_desc) VALUES(?,?,?,?)`, v.ID, v.StartedAt, v.SourceKind, v.SourceDesc)
		return err
	case metadataProvenanceType:
		var v metadataProvenance
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		if err := validateProvenanceRecord(v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO provenance(
			identity,node_id,ingest_id,original_path,original_mtime,supersedes
		) VALUES(?,?,?,?,?,?)`, v.Identity, v.NodeID, v.IngestID, v.OriginalPath,
			v.OriginalMTime, v.Supersedes)
		return err
	case metadataWatchSourceType:
		var v metadataWatchSource
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		if err := validateWatchSourceRecord(v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO watch_sources(
			watch_name,source_ref,node_id,blob_hash,size
		) VALUES(?,?,?,?,?)`, v.WatchName, v.SourceRef, v.NodeID, v.BlobHash, v.Size)
		return err
	case "tag":
		var v metadataTag
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		if err := validateTagRecord(v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO tags(id,name,revision) VALUES(?,?,?)`, v.ID, v.Name, v.Revision)
		return err
	case "node_tag":
		var v metadataNodeTag
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		if err := validateNodeTagRecord(v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO node_tags(node_id,tag_id) VALUES(?,?)`, v.NodeID, v.TagID)
		return err
	case "extracted_text":
		var v metadataExtractedText
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		if err := validateExtractedTextRecord(v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO extracted_text(blob_hash,extractor,extractor_version,status,error,attempts,text,extracted_at) VALUES(?,?,?,?,?,?,?,?)`,
			v.BlobHash, v.Extractor, v.ExtractorVersion, v.Status, v.Error, v.Attempts, v.Text, v.ExtractedAt)
		if err != nil {
			return err
		}
		var text any
		if v.Status == ExtractionOK && v.Text != nil {
			text = *v.Text
		}
		if err := replaceContentFTSTx(ctx, tx, v.BlobHash, v.Extractor, text); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`DELETE FROM text_extraction_queue WHERE blob_hash = ?`, v.BlobHash)
		return err
	case metadataAuditAuthorityType:
		var v metadataAuditAuthority
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		return importAuditAuthority(ctx, tx, v)
	case metadataAuditScopeType:
		var v metadataAuditScope
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		return importAuditScope(ctx, tx, v)
	case metadataAuditMembershipType:
		var v metadataAuditMembership
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		return importAuditMembership(ctx, tx, v)
	case metadataAuditRecordType:
		var v metadataAuditRecord
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		return importAuditRecord(ctx, tx, v)
	default:
		return fmt.Errorf("unknown record type %q", kind)
	}
}

const (
	metadataTypeField                     = "type"
	metadataNodeIDField                   = "node_id"
	auditVaultIDField                     = "vault_id"
	auditOperationIDField                 = "operation_id"
	auditScopeIDField                     = "scope_id"
	auditOriginField                      = "origin"
	auditTopologyDeltaField               = "topology_delta"
	auditWitnessChangeCountField          = "witness_change_count"
	auditAttachedMetadataChangeCountField = "attached_metadata_change_count"
	auditEventField                       = "event"
	metadataIngestType                    = "ingest"
	metadataProvenanceType                = "provenance"
	metadataWatchSourceType               = "watch_source"
	metadataTagRecordType                 = "tag"
	metadataAuditAuthorityType            = "audit_authority"
	metadataAuditScopeType                = "audit_scope"
	metadataAuditMembershipType           = "audit_membership"
	metadataAuditRecordType               = "audit_record"
)

var metadataHeaderFields = []string{metadataTypeField, "format", "version", auditVaultIDField, "node_sequence"}

var metadataRequiredFields = map[string][]string{
	"blob":                      {metadataTypeField, "hash", "size", "created_at"},
	"node":                      {metadataTypeField, "id", "parent_id", "name", "kind", "current_version_id", "revision", "created_at", "modified_at", "trashed_at", "trash_parent", "trash_name"},
	"content_version":           {metadataTypeField, "version_id", metadataNodeIDField, "blob_hash", "size", "mime_type", auditRecordedAtField, "node_revision", "introduced_operation_id", "transition_kind", "source_version_id"},
	metadataIngestType:          {metadataTypeField, "ingest_id", "started_at", "source_kind", "source_desc"},
	metadataProvenanceType:      {metadataTypeField, "identity", metadataNodeIDField, "ingest_id", "original_path", "original_mtime", "supersedes"},
	metadataWatchSourceType:     {metadataTypeField, "watch_name", "source_ref", metadataNodeIDField, "blob_hash", "size"},
	"tag":                       {metadataTypeField, "tag_id", "name", "revision"},
	"node_tag":                  {metadataTypeField, metadataNodeIDField, "tag_id"},
	"extracted_text":            {metadataTypeField, "blob_hash", "extractor", "extractor_version", "status", "error", "attempts", "text", "extracted_at"},
	metadataAuditAuthorityType:  {metadataTypeField, "lineage_id", "operation_sequence_high_water", "allocation_genesis_digest", "allocation_entry_count", "allocation_head"},
	metadataAuditScopeType:      {metadataTypeField, auditScopeIDField, "target_node_id", "enable_operation_id", "entry_count", "chain_head"},
	metadataAuditMembershipType: {metadataTypeField, auditScopeIDField, metadataNodeIDField, "baseline_digest"},
	metadataAuditRecordType:     {metadataTypeField, "digest", "record"},
}

var metadataNullableFields = map[string]map[string]bool{
	"node": {
		"parent_id": true, "current_version_id": true, "trashed_at": true,
		"trash_parent": true, "trash_name": true,
	},
	"content_version":      {"mime_type": true, "source_version_id": true},
	metadataProvenanceType: {"original_mtime": true, "supersedes": true},
	"extracted_text":       {"error": true, "text": true},
}

func decodeMetadataRecord(raw json.RawMessage, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("metadata record contains trailing JSON")
	}
	return nil
}

func requireMetadataFields(raw json.RawMessage, required []string, nullable map[string]bool) error {
	fields, err := decodeMetadataFields(raw)
	if err != nil {
		return err
	}
	allowed := make(map[string]bool, len(required))
	for _, field := range required {
		allowed[field] = true
		value, ok := fields[field]
		if !ok {
			return fmt.Errorf("metadata record lacks required field %q", field)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) && !nullable[field] {
			return fmt.Errorf("metadata field %q cannot be null", field)
		}
	}
	for field := range fields {
		if !allowed[field] {
			return fmt.Errorf("metadata record contains unknown or non-canonical field %q", field)
		}
	}
	return nil
}

func decodeMetadataFields(raw json.RawMessage) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	token, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("decoding metadata fields: %w", err)
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return nil, errors.New("metadata record must be a JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for dec.More() {
		nameToken, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("decoding metadata field name: %w", err)
		}
		name, ok := nameToken.(string)
		if !ok {
			return nil, errors.New("metadata field name is not a string")
		}
		if _, exists := fields[name]; exists {
			return nil, fmt.Errorf("metadata record contains duplicate field %q", name)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, fmt.Errorf("decoding metadata field %q: %w", name, err)
		}
		fields[name] = value
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("closing metadata object: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("metadata record contains trailing JSON")
	}
	return fields, nil
}

func validateMetadataJSON(raw json.RawMessage) error {
	return jsontext.Validate(raw, "metadata JSON")
}

func validateUTF8Field(field, value string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("invalid %s: not valid UTF-8", field)
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
	if err := validateUTF8Field("node name", v.Name); err != nil {
		return err
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
		if v.Name != "" || v.Kind != nodeKindDir || v.CurrentVersionID != nil || v.TrashedAt != nil ||
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
	case nodeKindDir:
		if v.CurrentVersionID != nil {
			return errors.New("directory record carries file content")
		}
	case "file":
		if v.CurrentVersionID == nil {
			return errors.New("file record lacks valid content identity")
		}
		if err := validateUUIDv4(*v.CurrentVersionID); err != nil {
			return fmt.Errorf("invalid node current_version_id: %w", err)
		}
	default:
		return fmt.Errorf("invalid node kind %q", v.Kind)
	}
	if v.TrashParent != nil && v.TrashName == nil {
		return errors.New("incomplete node trash coordinates")
	}
	if (v.TrashParent != nil || v.TrashName != nil) && v.TrashedAt == nil {
		return errors.New("incomplete node trash coordinates")
	}
	if v.TrashName != nil {
		if err := validateUTF8Field("node trash_name", *v.TrashName); err != nil {
			return err
		}
		normalizedTrashName, err := NormalizeName(*v.TrashName)
		if err != nil || normalizedTrashName != *v.TrashName {
			return fmt.Errorf("invalid node trash_name %q", *v.TrashName)
		}
	}
	return nil
}

func validateContentVersionRecord(v metadataContentVersion) error {
	if v.Type != "content_version" || v.NodeID <= 0 || v.Size < 0 || v.NodeRevision <= 0 {
		return errors.New("invalid content version record")
	}
	if err := validateUUIDv4(v.VersionID); err != nil {
		return fmt.Errorf("invalid content version ID: %w", err)
	}
	if err := validateUUIDv4(v.IntroducedOperationID); err != nil {
		return fmt.Errorf("invalid content version operation ID: %w", err)
	}
	if _, err := packstore.ParseHash(v.BlobHash); err != nil {
		return fmt.Errorf("invalid content version blob hash: %w", err)
	}
	if v.MIMEType != nil {
		if *v.MIMEType == "" {
			return errors.New("content version mime_type must be null or non-empty")
		}
		if err := validateUTF8Field("content version mime_type", *v.MIMEType); err != nil {
			return err
		}
	}
	switch v.TransitionKind {
	case "content_create", "content_replace":
		if v.SourceVersionID != nil {
			return fmt.Errorf("%s content version has a source version", v.TransitionKind)
		}
	case "content_revert":
		if v.SourceVersionID == nil {
			return errors.New("content_revert version lacks a source version")
		}
		if err := validateUUIDv4(*v.SourceVersionID); err != nil {
			return fmt.Errorf("invalid source content version ID: %w", err)
		}
	default:
		return fmt.Errorf("invalid content transition kind %q", v.TransitionKind)
	}
	if (v.TransitionKind == "content_create") != (v.NodeRevision == 1) {
		return errors.New("content_create is required exactly at node revision one")
	}
	return validateMetadataTime("content version recorded_at", v.RecordedAt)
}

func validateIngestRecord(v metadataIngest) error {
	if v.Type != metadataIngestType || v.SourceKind == "" || v.SourceDesc == "" {
		return errors.New("invalid ingest record")
	}
	if err := validateUUIDv4(v.ID); err != nil {
		return fmt.Errorf("invalid ingest ID: %w", err)
	}
	if err := validateUTF8Field("ingest source_kind", v.SourceKind); err != nil {
		return err
	}
	if err := validateUTF8Field("ingest source_desc", v.SourceDesc); err != nil {
		return err
	}
	return validateMetadataTime("ingest started_at", v.StartedAt)
}

func validateProvenanceRecord(v metadataProvenance) error {
	if v.Identity == "" {
		return errors.New("invalid provenance record")
	}
	if err := validateProvenanceFields(v); err != nil {
		return err
	}
	if _, err := packstore.ParseHash(v.Identity); err != nil {
		return fmt.Errorf("invalid provenance identity: %w", err)
	}
	want, err := provenanceIdentity(v)
	if err != nil {
		return fmt.Errorf("computing provenance identity: %w", err)
	}
	if v.Identity != want {
		return errors.New("provenance identity does not match its immutable fields")
	}
	return nil
}

func validateProvenanceFields(v metadataProvenance) error {
	if v.Type != metadataProvenanceType || v.NodeID <= 0 || v.OriginalPath == "" {
		return errors.New("invalid provenance record")
	}
	if err := validateUUIDv4(v.IngestID); err != nil {
		return fmt.Errorf("invalid provenance ingest ID: %w", err)
	}
	if err := validateUTF8Field("provenance original_path", v.OriginalPath); err != nil {
		return err
	}
	if v.OriginalMTime != nil {
		if err := validateProvenanceTime(*v.OriginalMTime); err != nil {
			return err
		}
	}
	if v.Supersedes != nil {
		if _, err := packstore.ParseHash(*v.Supersedes); err != nil {
			return fmt.Errorf("invalid superseded provenance identity: %w", err)
		}
	}
	return nil
}

func validateWatchSourceRecord(v metadataWatchSource) error {
	if v.Type != metadataWatchSourceType || v.WatchName == "" ||
		v.SourceRef == "" || v.NodeID <= 0 || v.Size < 0 {
		return errors.New("invalid watched source record")
	}
	if err := validateUTF8Field("watched source name", v.WatchName); err != nil {
		return err
	}
	if err := validateUTF8Field("watched source reference", v.SourceRef); err != nil {
		return err
	}
	if v.SourceRef == "." || path.IsAbs(v.SourceRef) || v.SourceRef == ".." ||
		strings.HasPrefix(v.SourceRef, "../") || path.Clean(v.SourceRef) != v.SourceRef {
		return fmt.Errorf("invalid watched source reference %q", v.SourceRef)
	}
	if _, err := packstore.ParseHash(v.BlobHash); err != nil {
		return fmt.Errorf("invalid watched source blob hash: %w", err)
	}
	return nil
}

func validateTagRecord(v metadataTag) error {
	if v.Type != "tag" || v.Name == "" || v.Revision < 1 {
		return errors.New("invalid tag record")
	}
	if err := validateUUIDv4(v.ID); err != nil {
		return fmt.Errorf("invalid tag ID: %w", err)
	}
	normalized, err := NormalizeTagName(v.Name)
	if err != nil {
		return err
	}
	if normalized != v.Name {
		return errors.New("tag name is not canonical NFC")
	}
	return nil
}

func validateNodeTagRecord(v metadataNodeTag) error {
	if v.Type != "node_tag" || v.NodeID <= 0 {
		return errors.New("invalid node tag record")
	}
	if err := validateUUIDv4(v.TagID); err != nil {
		return fmt.Errorf("invalid node tag tag ID: %w", err)
	}
	return nil
}

func validateExtractedTextRecord(v metadataExtractedText) error {
	if v.Type != "extracted_text" || v.Extractor == "" || v.ExtractorVersion < 0 || v.Attempts < 0 {
		return errors.New("invalid extracted text record")
	}
	if v.Status != "ok" && v.Status != "failed" {
		return fmt.Errorf("invalid extraction status %q", v.Status)
	}
	if v.Status == ExtractionOK && (v.Text == nil || v.Error != nil) {
		return errors.New("successful extraction requires text and no error")
	}
	if v.Status == ExtractionFailed && (v.Error == nil || v.Text != nil) {
		return errors.New("failed extraction requires an error and no text")
	}
	if _, err := packstore.ParseHash(v.BlobHash); err != nil {
		return fmt.Errorf("invalid extracted text blob hash: %w", err)
	}
	if err := validateUTF8Field("extracted text extractor", v.Extractor); err != nil {
		return err
	}
	if v.Error != nil {
		if err := validateUTF8Field("extracted text error", *v.Error); err != nil {
			return err
		}
	}
	if v.Text != nil {
		if err := validateUTF8Field("extracted text value", *v.Text); err != nil {
			return err
		}
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

func validateProvenanceTime(value string) error {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return fmt.Errorf("invalid provenance original_mtime: %w", err)
	}
	if value != parsed.UTC().Format(time.RFC3339Nano) {
		return errors.New("invalid provenance original_mtime: timestamp is not canonical UTC RFC3339Nano")
	}
	return nil
}

func validateMetadataState(ctx context.Context, tx metadataQuerier, nodeSequence int64) error {
	var vaultID string
	if err := tx.QueryRowContext(ctx,
		`SELECT vault_id FROM vault_metadata WHERE singleton = 1`,
	).Scan(&vaultID); err != nil {
		return fmt.Errorf("reading vault identity: %w", err)
	}
	if err := validateUUIDv4(vaultID); err != nil {
		return fmt.Errorf("invalid vault identity: %w", err)
	}
	if err := validateMetadataRelations(ctx, tx); err != nil {
		return err
	}
	if err := validateWatchSourceRelations(ctx, tx); err != nil {
		return err
	}
	topology, err := loadAuditTopologyRows(ctx, tx)
	if err != nil {
		return err
	}
	if err := validateAuditTrashOrigins(topology); err != nil {
		return err
	}
	if err := validateAuditAuthority(ctx, tx, vaultID, nodeSequence); err != nil {
		return err
	}
	var maxNodeID int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM nodes`).Scan(&maxNodeID); err != nil {
		return fmt.Errorf("reading maximum node ID: %w", err)
	}
	if nodeSequence < maxNodeID {
		return fmt.Errorf("node ID high-water mark %d is below maximum node ID %d", nodeSequence, maxNodeID)
	}
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("checking metadata foreign keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		var table string
		var rowID, parent any
		var fk int64
		if err := rows.Scan(&table, &rowID, &parent, &fk); err != nil {
			return fmt.Errorf("reading metadata foreign-key failure: %w", err)
		}
		return fmt.Errorf("metadata violates foreign key %s[%v] constraint %d", table, rowID, fk)
	}
	return rows.Err()
}

func validateMetadataRelations(ctx context.Context, tx metadataQuerier) error {
	checks := []struct {
		name  string
		query string
	}{
		{"tree does not have exactly one root", `
			SELECT (SELECT COUNT(*) FROM nodes WHERE parent_id IS NULL) != 1`},
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
		{"trash root is not detached beneath the tree root", `
			SELECT EXISTS(
			  SELECT 1 FROM nodes n
			  WHERE n.trash_name IS NOT NULL
			    AND n.parent_id != (SELECT id FROM nodes WHERE parent_id IS NULL)
			)`},
		{"trash parent points inside its subtree", `
			WITH RECURSIVE trash_subtree(trash_root, id) AS (
			  SELECT id, id FROM nodes WHERE trash_name IS NOT NULL
			  UNION
			  SELECT s.trash_root, n.id
			  FROM trash_subtree s JOIN nodes n ON n.parent_id = s.id
			)
			SELECT EXISTS(
			  SELECT 1 FROM nodes root
			  JOIN trash_subtree s
			    ON s.trash_root = root.id AND s.id = root.trash_parent
			  WHERE root.trash_parent IS NOT NULL
			)`},
		{"trashed node does not belong to exactly one trash root", `
			WITH RECURSIVE trash_subtree(trash_root, id) AS (
			  SELECT id, id FROM nodes WHERE trash_name IS NOT NULL
			  UNION ALL
			  SELECT s.trash_root, n.id
			  FROM trash_subtree s JOIN nodes n ON n.parent_id = s.id
			)
			SELECT EXISTS(
			  SELECT 1 FROM nodes n
			  LEFT JOIN trash_subtree s ON s.id = n.id
			  WHERE n.trashed_at IS NOT NULL
			  GROUP BY n.id
			  HAVING COUNT(s.trash_root) != 1
			)`},
		{"trash subtree contains live node or mismatched timestamp", `
			WITH RECURSIVE trash_subtree(trash_root, root_stamp, id) AS (
			  SELECT id, trashed_at, id FROM nodes WHERE trash_name IS NOT NULL
			  UNION ALL
			  SELECT s.trash_root, s.root_stamp, n.id
			  FROM trash_subtree s JOIN nodes n ON n.parent_id = s.id
			)
			SELECT EXISTS(
			  SELECT 1 FROM trash_subtree s JOIN nodes n ON n.id = s.id
			  WHERE n.trashed_at IS NULL OR n.trashed_at != s.root_stamp
			)`},
		{"content version belongs to a directory", `
			SELECT EXISTS(SELECT 1 FROM content_versions v JOIN nodes n ON n.id=v.node_id WHERE n.kind != 'file')`},
		{"node current version does not belong to that node", `
			SELECT EXISTS(
			  SELECT 1 FROM nodes n LEFT JOIN content_versions v ON v.version_id=n.current_version_id
			  WHERE n.kind='file' AND (v.version_id IS NULL OR v.node_id != n.id)
			)`},
		{"content version revision exceeds its node revision", `
			SELECT EXISTS(SELECT 1 FROM content_versions v JOIN nodes n ON n.id=v.node_id
			 WHERE v.node_revision > n.revision)`},
		{"source content version belongs to another node", `
			SELECT EXISTS(
			  SELECT 1 FROM content_versions v JOIN content_versions source ON source.version_id=v.source_version_id
			  WHERE source.node_id != v.node_id
			)`},
		{"source content version is not older than its revert", `
			SELECT EXISTS(
			  SELECT 1 FROM content_versions v JOIN content_versions source ON source.version_id=v.source_version_id
			  WHERE source.node_revision >= v.node_revision
			)`},
		{"revert source content differs from new version", `
			SELECT EXISTS(
			  SELECT 1 FROM content_versions v JOIN content_versions source ON source.version_id=v.source_version_id
			  WHERE v.transition_kind='content_revert'
			    AND (source.blob_hash != v.blob_hash OR source.size != v.size
			         OR source.mime_type IS NOT v.mime_type)
			)`},
		{"content version size differs from blob authority", `
			SELECT EXISTS(SELECT 1 FROM content_versions v JOIN blobs b ON b.hash=v.blob_hash WHERE v.size != b.size)`},
		// Explicit version pruning may remove the revision-one create. UUID
		// identities are never reused and the node allocator/revision remain at
		// their high-water marks, so retained history may begin at any revision.
		{"content_create time differs from node creation", `
			SELECT EXISTS(
			  SELECT 1 FROM content_versions v JOIN nodes n ON n.id=v.node_id
			  WHERE v.transition_kind='content_create' AND v.recorded_at != n.created_at
			)`},
		{"node current version is not its newest content version", `
			SELECT EXISTS(
			  SELECT 1 FROM nodes n JOIN content_versions current ON current.version_id=n.current_version_id
			  WHERE EXISTS(SELECT 1 FROM content_versions newer
			    WHERE newer.node_id=n.id AND newer.node_revision > current.node_revision)
			)`},
		{"extracted text references missing blob authority", `
			SELECT EXISTS(SELECT 1 FROM extracted_text e LEFT JOIN blobs b ON b.hash=e.blob_hash WHERE b.hash IS NULL)`},
		{"provenance supersedes a missing fact", `
			SELECT EXISTS(
			  SELECT 1 FROM provenance p LEFT JOIN provenance prior ON prior.identity=p.supersedes
			  WHERE p.supersedes IS NOT NULL AND prior.identity IS NULL
			)`},
		{"provenance supersedes a fact on another node", `
			SELECT EXISTS(
			  SELECT 1 FROM provenance p JOIN provenance prior ON prior.identity=p.supersedes
			  WHERE prior.node_id != p.node_id
			)`},
		{"provenance supersession graph contains a cycle", `
			WITH RECURSIVE reachable(identity) AS (
			  SELECT identity FROM provenance WHERE supersedes IS NULL
			  UNION ALL
			  SELECT p.identity FROM provenance p JOIN reachable r ON p.supersedes=r.identity
			)
			SELECT (SELECT COUNT(*) FROM reachable) != (SELECT COUNT(*) FROM provenance)`},
	}
	for _, check := range checks {
		var failed bool
		if err := tx.QueryRowContext(ctx, check.query).Scan(&failed); err != nil {
			return fmt.Errorf("validating metadata (%s): %w", check.name, err)
		}
		if failed {
			return errors.New(check.name)
		}
	}
	return nil
}

type watchSourceKey struct {
	watchName string
	sourceRef string
}

func validateWatchSourceRelations(ctx context.Context, tx metadataQuerier) error {
	nodeKinds, err := loadMetadataNodeKinds(ctx, tx)
	if err != nil {
		return err
	}
	cursors, err := loadWatchSourceNodes(ctx, tx, "cursor", `
		SELECT watch_name, source_ref, node_id FROM watch_sources`)
	if err != nil {
		return err
	}
	if err := requireWatchSourceFiles("cursor", cursors, nodeKinds); err != nil {
		return err
	}
	provenance, err := loadWatchSourceNodes(ctx, tx, "provenance", `
		SELECT i.source_desc, p.original_path, p.node_id
		FROM provenance p JOIN ingests i ON i.id = p.ingest_id
		WHERE i.source_kind = 'watch'`)
	if err != nil {
		return err
	}
	if err := requireWatchSourceFiles("provenance", provenance, nodeKinds); err != nil {
		return err
	}
	if len(cursors) != len(provenance) {
		return errors.New("watched source cursors do not match provenance")
	}
	for key, nodeID := range provenance {
		if cursors[key] != nodeID {
			return fmt.Errorf("watched source cursor %q/%q does not match provenance",
				key.watchName, key.sourceRef)
		}
	}
	return nil
}

func loadMetadataNodeKinds(ctx context.Context, tx metadataQuerier) (map[int64]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, kind FROM nodes`)
	if err != nil {
		return nil, fmt.Errorf("reading metadata node kinds: %w", err)
	}
	defer func() { _ = rows.Close() }()
	kinds := make(map[int64]string)
	for rows.Next() {
		var id int64
		var kind string
		if err := rows.Scan(&id, &kind); err != nil {
			return nil, fmt.Errorf("scanning metadata node kind: %w", err)
		}
		kinds[id] = kind
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating metadata node kinds: %w", err)
	}
	return kinds, nil
}

func requireWatchSourceFiles(
	kind string, sources map[watchSourceKey]int64, nodeKinds map[int64]string,
) error {
	claimed := make(map[int64]watchSourceKey, len(sources))
	for key, nodeID := range sources {
		if nodeKinds[nodeID] != nodeKindFile {
			return fmt.Errorf("watched source %s %q/%q references non-file node %d",
				kind, key.watchName, key.sourceRef, nodeID)
		}
		if prior, exists := claimed[nodeID]; exists {
			return fmt.Errorf(
				"watched source %s %q/%q and %q/%q reference the same node %d",
				kind, prior.watchName, prior.sourceRef, key.watchName, key.sourceRef, nodeID,
			)
		}
		claimed[nodeID] = key
	}
	return nil
}

func loadWatchSourceNodes(
	ctx context.Context, tx metadataQuerier, kind, query string,
) (map[watchSourceKey]int64, error) {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("reading watched source %s: %w", kind, err)
	}
	defer func() { _ = rows.Close() }()
	result := make(map[watchSourceKey]int64)
	for rows.Next() {
		var key watchSourceKey
		var nodeID int64
		if err := rows.Scan(&key.watchName, &key.sourceRef, &nodeID); err != nil {
			return nil, fmt.Errorf("scanning watched source %s: %w", kind, err)
		}
		if priorNode, exists := result[key]; exists && priorNode != nodeID {
			return nil, fmt.Errorf("watched source %q/%q identifies multiple nodes",
				key.watchName, key.sourceRef)
		}
		result[key] = nodeID
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating watched source %s: %w", kind, err)
	}
	return result, nil
}
