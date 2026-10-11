package store

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/document"
	docsqlite "go.kenn.io/docbank/sqlite"
)

const metadataFormatVersion = 1

const (
	metadataCreatedAtField        = "created_at"
	metadataAttachmentIDField     = "attachment_id"
	metadataGenerationIDField     = "generation_id"
	metadataContentVersionIDField = "content_version_id"
	metadataIngestIDField         = "ingest_id"
	metadataRevisionField         = "revision"
	metadataCanonicalJSONField    = "canonical_json"
	metadataUpdatedAtField        = "updated_at"
)

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

// ExportBackup writes the deterministic logical authority carried by a backup
// snapshot. It omits blob and extraction-cache rows outside retained document
// or rendition authority, including uncommitted provider staging.
func (s *MetadataSnapshot) ExportBackup(ctx context.Context, w io.Writer) error {
	return exportBackupMetadataSnapshot(ctx, s, w)
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
	Hash      string `json:"hash" db:"hash"`
	Size      int64  `json:"size" db:"size"`
	CreatedAt string `json:"created_at" db:"created_at"`
}

type metadataBlobChecksum struct {
	Type       string `json:"type"`
	BlobSHA256 string `json:"blob_sha256" db:"blob_sha256"`
	MD5        string `json:"md5" db:"md5"`
}

type metadataSourceMetadataGeneration struct {
	Type                 string `json:"type"`
	GenerationID         string `json:"generation_id" db:"generation_id"`
	SourceSHA256         string `json:"source_sha256" db:"source_sha256"`
	ContractVersion      string `json:"contract_version" db:"contract_version"`
	ExtractorFingerprint string `json:"extractor_fingerprint" db:"extractor_fingerprint"`
	CanonicalJSON        []byte `json:"canonical_json" format:"byte" db:"canonical_json"`
	Checksum             string `json:"checksum" db:"checksum"`
	CreatedAt            string `json:"created_at" db:"created_at"`
}

type metadataSourceMetadataHead struct {
	Type         string `json:"type"`
	SourceSHA256 string `json:"source_sha256" db:"source_sha256"`
	GenerationID string `json:"generation_id" db:"generation_id"`
	PublishedAt  string `json:"published_at" db:"published_at"`
}

type metadataVisualPreviewGeneration struct {
	Type              string `json:"type"`
	GenerationID      string `json:"generation_id"`
	VaultID           string `json:"vault_id"`
	ContentVersionID  string `json:"content_version_id"`
	SourceSHA256      string `json:"source_sha256"`
	ContractVersion   string `json:"contract_version"`
	RecipeFingerprint string `json:"recipe_fingerprint"`
	CanonicalResult   []byte `json:"canonical_result" format:"byte"`
	Checksum          string `json:"checksum"`
	CreatedAt         string `json:"created_at"`
}

type metadataVisualPreviewHead struct {
	Type             string `json:"type"`
	ContentVersionID string `json:"content_version_id" db:"content_version_id"`
	GenerationID     string `json:"generation_id" db:"generation_id"`
	PublishedAt      string `json:"published_at" db:"published_at"`
}

type metadataNode struct {
	Type             string  `json:"type"`
	ID               int64   `json:"id" db:"id"`
	ParentID         *int64  `json:"parent_id" db:"parent_id"`
	Name             string  `json:"name" db:"name"`
	Kind             string  `json:"kind" db:"kind"`
	CurrentVersionID *string `json:"current_version_id" db:"current_version_id"`
	Revision         int64   `json:"revision" db:"revision"`
	CreatedAt        string  `json:"created_at" db:"created_at"`
	ModifiedAt       string  `json:"modified_at" db:"modified_at"`
	TrashedAt        *string `json:"trashed_at" db:"trashed_at"`
	TrashParent      *int64  `json:"trash_parent" db:"trash_parent"`
	TrashName        *string `json:"trash_name" db:"trash_name"`
}

type metadataContentVersion struct {
	Type                  string  `json:"type"`
	VersionID             string  `json:"version_id" db:"version_id"`
	NodeID                int64   `json:"node_id" db:"node_id"`
	BlobHash              string  `json:"blob_hash" db:"blob_hash"`
	Size                  int64   `json:"size" db:"size"`
	MIMEType              *string `json:"mime_type" db:"mime_type"`
	RecordedAt            string  `json:"recorded_at" db:"recorded_at"`
	NodeRevision          int64   `json:"node_revision" db:"node_revision"`
	IntroducedOperationID string  `json:"introduced_operation_id" db:"introduced_operation_id"`
	TransitionKind        string  `json:"transition_kind" db:"transition_kind"`
	SourceVersionID       *string `json:"source_version_id" db:"source_version_id"`
}

type metadataIngest struct {
	Type       string `json:"type"`
	ID         string `json:"ingest_id" db:"id"`
	StartedAt  string `json:"started_at" db:"started_at"`
	SourceKind string `json:"source_kind" db:"source_kind"`
	SourceDesc string `json:"source_desc" db:"source_desc"`
}

type metadataProvenance struct {
	Type          string  `json:"type"`
	Identity      string  `json:"identity" db:"identity"`
	NodeID        int64   `json:"node_id" db:"node_id"`
	IngestID      string  `json:"ingest_id" db:"ingest_id"`
	OriginalPath  string  `json:"original_path" db:"original_path"`
	OriginalMTime *string `json:"original_mtime" db:"original_mtime"`
	Supersedes    *string `json:"supersedes" db:"supersedes"`
}

type metadataWatchSource struct {
	Type      string `json:"type"`
	WatchName string `json:"watch_name" db:"watch_name"`
	SourceRef string `json:"source_ref" db:"source_ref"`
	NodeID    int64  `json:"node_id" db:"node_id"`
	BlobHash  string `json:"blob_hash" db:"blob_hash"`
	Size      int64  `json:"size" db:"size"`
}

type metadataPushSource struct {
	Type               string `json:"type"`
	PushName           string `json:"push_name" db:"push_name"`
	SourceRef          string `json:"source_ref" db:"source_ref"`
	NodeID             int64  `json:"node_id" db:"node_id"`
	ProvenanceIdentity string `json:"provenance_identity" db:"provenance_identity"`
	BlobHash           string `json:"blob_hash" db:"blob_hash"`
	Size               int64  `json:"size" db:"size"`
	AcceptedAt         string `json:"accepted_at" db:"accepted_at"`
}

type metadataTag struct {
	Type     string `json:"type"`
	ID       string `json:"tag_id" db:"id"`
	Name     string `json:"name" db:"name"`
	Revision int64  `json:"revision" db:"revision"`
}

type metadataNodeTag struct {
	Type   string `json:"type"`
	NodeID int64  `json:"node_id" db:"node_id"`
	TagID  string `json:"tag_id" db:"tag_id"`
}

type metadataExtractedText struct {
	Type             string  `json:"type"`
	BlobHash         string  `json:"blob_hash" db:"blob_hash"`
	Extractor        string  `json:"extractor" db:"extractor"`
	ExtractorVersion int64   `json:"extractor_version" db:"extractor_version"`
	Status           string  `json:"status" db:"status"`
	Error            *string `json:"error" db:"error"`
	Attempts         int64   `json:"attempts" db:"attempts"`
	Text             *string `json:"text" db:"text"`
	ExtractedAt      string  `json:"extracted_at" db:"extracted_at"`
}

type metadataAuditRecord struct {
	Type   string         `json:"type"`
	Digest string         `json:"digest"`
	Record jsontext.Value `json:"record"`
}

type metadataAuditAuthority struct {
	Type                       string `json:"type"`
	LineageID                  string `json:"lineage_id" db:"lineage_id"`
	OperationSequenceHighWater uint64 `json:"operation_sequence_high_water" db:"operation_sequence_high_water"`
	AllocationGenesisDigest    string `json:"allocation_genesis_digest" db:"allocation_genesis_digest"`
	AllocationEntryCount       uint64 `json:"allocation_entry_count" db:"allocation_entry_count"`
	AllocationHead             string `json:"allocation_head" db:"allocation_head"`
	Singleton                  int    `json:"-" db:"singleton"`
}

type metadataAuditScope struct {
	Type              string `json:"type"`
	ScopeID           string `json:"scope_id" db:"scope_id"`
	TargetNodeID      uint64 `json:"target_node_id" db:"target_node_id"`
	EnableOperationID string `json:"enable_operation_id" db:"enable_operation_id"`
	EntryCount        uint64 `json:"entry_count" db:"entry_count"`
	ChainHead         string `json:"chain_head" db:"chain_head"`
}

type metadataAuditMembership struct {
	Type           string `json:"type"`
	ScopeID        string `json:"scope_id" db:"scope_id"`
	NodeID         uint64 `json:"node_id" db:"node_id"`
	BaselineDigest string `json:"baseline_digest" db:"baseline_digest"`
}

var (
	nodeMetadata = newMetadataTable(metadataTable[metadataNode]{record: metadataNode{Type: "node"},
		table: "nodes", suffix: "ORDER BY id", validate: validateNodeRecord, checkExport: true})
	contentVersionMetadata = newMetadataTable(metadataTable[metadataContentVersion]{
		record: metadataContentVersion{Type: "content_version"}, table: "content_versions",
		suffix: "ORDER BY node_id, node_revision, version_id", validate: validateContentVersionRecord, checkExport: true})
	ingestMetadata = newMetadataTable(metadataTable[metadataIngest]{record: metadataIngest{Type: metadataIngestType},
		table: "ingests", suffix: "ORDER BY id", validate: validateIngestRecord, checkExport: true})
	provenanceMetadata = newMetadataTable(metadataTable[metadataProvenance]{record: metadataProvenance{Type: metadataProvenanceType},
		table: "provenance", suffix: "ORDER BY identity", validate: validateProvenanceRecord, checkExport: true})
	watchSourceMetadata = newMetadataTable(metadataTable[metadataWatchSource]{record: metadataWatchSource{Type: metadataWatchSourceType},
		table: "watch_sources", suffix: "ORDER BY watch_name, source_ref", validate: validateWatchSourceRecord, checkExport: true})
	pushSourceMetadata = newMetadataTable(metadataTable[metadataPushSource]{record: metadataPushSource{Type: metadataPushSourceType},
		table: "push_sources", suffix: "ORDER BY push_name, source_ref", validate: validatePushSourceRecord, checkExport: true})
	tagMetadata = newMetadataTable(metadataTable[metadataTag]{record: metadataTag{Type: "tag"},
		table: "tags", suffix: "ORDER BY id", validate: validateTagRecord, checkExport: true})
	nodeTagMetadata = newMetadataTable(metadataTable[metadataNodeTag]{record: metadataNodeTag{Type: "node_tag"},
		table: "node_tags", suffix: "ORDER BY node_id, tag_id", validate: validateNodeTagRecord, checkExport: true})
	visualPreviewHeadMetadata = newMetadataTable(metadataTable[metadataVisualPreviewHead]{
		record: metadataVisualPreviewHead{Type: metadataVisualPreviewHeadType}, table: "visual_preview_heads",
		suffix: "ORDER BY content_version_id", validate: validateVisualPreviewHeadRecord})
)

// coreMetadataTables registers the records of metadata.go and the one-record
// files. Blobs, checksums, source metadata, visual preview generations and
// extracted text keep their backup-scoped exporters.
var coreMetadataTables = []metadataRecordCodec{
	nodeMetadata, contentVersionMetadata, ingestMetadata, provenanceMetadata, watchSourceMetadata, pushSourceMetadata,
	tagMetadata, nodeTagMetadata, visualPreviewHeadMetadata, collectionLabelMetadata,
	provenanceVersionBindingMetadata, savedQueryMetadata, savedQueryRunImportMetadata,
	termReportHistoryImportMetadata, batchTagReceiptMetadata,
	newMetadataTable(metadataTable[metadataBlob]{record: metadataBlob{Type: "blob"}, table: "blobs",
		validate: validateBlobRecord, insert: importBlobRecord}),
	newMetadataTable(metadataTable[metadataBlobChecksum]{
		record: metadataBlobChecksum{Type: metadataBlobChecksumType}, table: "blob_checksums",
		validate: func(v metadataBlobChecksum) error {
			return validateBlobChecksumRecord(BlobChecksumRecord{BlobSHA256: v.BlobSHA256, MD5: v.MD5})
		},
		insert: func(_ context.Context, tx *sql.Tx, v metadataBlobChecksum) error {
			return ensureBlobChecksumTx(tx, BlobChecksumRecord{BlobSHA256: v.BlobSHA256, MD5: v.MD5})
		}}),
	newMetadataTable(metadataTable[metadataSourceMetadataGeneration]{
		record: metadataSourceMetadataGeneration{Type: metadataSourceMetadataGenerationType},
		table:  "source_metadata_generations", validate: validateSourceMetadataGenerationRecord}),
	newMetadataTable(metadataTable[metadataSourceMetadataHead]{
		record: metadataSourceMetadataHead{Type: metadataSourceMetadataHeadType}, table: "source_metadata_heads"}),
	newMetadataTable(metadataTable[metadataVisualPreviewGeneration]{
		record: metadataVisualPreviewGeneration{Type: metadataVisualPreviewGenerationType},
		table:  "visual_preview_generations", validate: validateVisualPreviewGenerationRecord,
		insert: importVisualPreviewGeneration}),
	newMetadataTable(metadataTable[metadataExtractedText]{record: metadataExtractedText{Type: "extracted_text"},
		table: "extracted_text", validate: validateExtractedTextRecord, insert: importExtractedText}),
}

func importBlobRecord(ctx context.Context, tx *sql.Tx, v metadataBlob) error {
	if err := insertMetadataRecord(ctx, tx, "blobs", v); err != nil {
		return err
	}
	generation, err := blobLocationGeneration()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO blob_locations(
			blob_hash, store_id, generation, kind, encoding, stored_size, pack_eligible
		)
		SELECT ?, store_id, ?, ?, ?, ?, CASE WHEN ? <= ? THEN 1 ELSE 0 END
		FROM blob_stores WHERE role = ?`,
		v.Hash, generation, blobLocationKindLoose, looseEncodingRaw, v.Size,
		v.Size, maxPackEligibleBytes, blobStoreRolePrimary,
	)
	return err
}

func importVisualPreviewGeneration(ctx context.Context, tx *sql.Tx, v metadataVisualPreviewGeneration) error {
	preview, _, err := document.DecodeVisualPreviewV1(v.CanonicalResult)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO visual_preview_generations(
		generation_id,vault_uid,content_version_id,source_sha256,contract_version,
		recipe_fingerprint,canonical_result,checksum,state,output_blob_hash,output_size,
		output_media_type,output_width,output_height,failure_code,failure_detail,created_at
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, v.GenerationID, v.VaultID,
		v.ContentVersionID, v.SourceSHA256, v.ContractVersion, v.RecipeFingerprint,
		v.CanonicalResult, v.Checksum, preview.State, previewOutputHash(preview),
		previewOutputSize(preview), previewOutputMediaType(preview), previewOutputWidth(preview),
		previewOutputHeight(preview), previewFailureCode(preview), previewFailureDetail(preview),
		v.CreatedAt)
	return err
}

func importExtractedText(ctx context.Context, tx *sql.Tx, v metadataExtractedText) error {
	if err := insertMetadataRecord(ctx, tx, "extracted_text", v); err != nil {
		return err
	}
	var text any
	if v.Status == ExtractionOK && v.Text != nil {
		text = *v.Text
	}
	return replaceContentFTSTx(ctx, tx, v.BlobHash, v.Extractor, text)
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
// metadataSourceLayout names the released storage schema that a metadata
// export reads from. Export and validation select record kinds by this
// version; they never inspect sqlite_schema to guess which tables exist.
type metadataSourceLayout struct {
	schemaVersion int
	backupScoped  bool
}

func currentMetadataLayout() metadataSourceLayout {
	return metadataSourceLayout{schemaVersion: currentStorageSchemaVersion}
}

// legacyV090 reports the one layout recognized by structure rather than by a
// recorded schema version.
func (layout metadataSourceLayout) legacyV090() bool {
	return layout.schemaVersion == 1
}

// hasPostV3Metadata separates the new metadata from the released layouts used
// through v0.14.0. Intermediate development layouts have no source readers.
func (layout metadataSourceLayout) hasPostV3Metadata() bool {
	return layout.schemaVersion > 3
}

func (layout metadataSourceLayout) hasPersons() bool {
	return layout.schemaVersion >= peopleStorageSchemaVersion
}

func (layout metadataSourceLayout) hasPushSources() bool {
	return layout.schemaVersion >= pushSourcesStorageSchemaVersion
}

func exportMetadataSnapshot(ctx context.Context, tx metadataQuerier, w io.Writer) error {
	return exportMetadataSnapshotWithVaultIdentity(ctx, tx, w, currentMetadataLayout())
}

func exportBackupMetadataSnapshot(ctx context.Context, tx metadataQuerier, w io.Writer) error {
	return exportMetadataSnapshotWithVaultIdentity(ctx, tx, w,
		metadataSourceLayout{schemaVersion: currentStorageSchemaVersion, backupScoped: true})
}

// exportReleasedMetadataSnapshot exports the logical authority of a database
// that still uses an earlier released storage schema.
func exportReleasedMetadataSnapshot(
	ctx context.Context, tx metadataQuerier, w io.Writer, schemaVersion int,
) error {
	return exportMetadataSnapshotWithVaultIdentity(ctx, tx, w,
		metadataSourceLayout{schemaVersion: schemaVersion})
}

func exportMetadataSnapshotWithVaultIdentity(
	ctx context.Context, tx metadataQuerier, w io.Writer, layout metadataSourceLayout,
) error {
	if tx == nil {
		return errors.New("exporting metadata: nil transaction")
	}
	vaultID, err := readVaultIdentity(ctx, tx, layout.legacyV090())
	if err != nil {
		return fmt.Errorf("reading vault identity: %w", err)
	}
	var nodeSequence int64
	if err := tx.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name = 'nodes'`).Scan(&nodeSequence); err != nil {
		return fmt.Errorf("reading node ID high-water mark: %w", err)
	}
	if err := validateMetadataStateWithVaultIdentity(ctx, tx, nodeSequence, layout); err != nil {
		return fmt.Errorf("validating metadata snapshot: %w", err)
	}
	backupScoped := layout.backupScoped
	write := newMetadataJSONWriter(w)
	if err := write(metadataHeader{
		Type: "meta", Format: "docbank-metadata", Version: metadataFormatVersion,
		VaultID: vaultID, NodeSequence: nodeSequence,
	}); err != nil {
		return err
	}
	if err := exportBlobs(ctx, tx, write, backupScoped); err != nil {
		return err
	}
	if layout.hasPostV3Metadata() {
		if err := exportBlobChecksums(ctx, tx, write, backupScoped); err != nil {
			return err
		}
		if err := exportSourceMetadata(ctx, tx, write, backupScoped); err != nil {
			return err
		}
	}
	if err := exportNodes(ctx, tx, write); err != nil {
		return err
	}
	if err := exportIngests(ctx, tx, write); err != nil {
		return err
	}
	if layout.hasPostV3Metadata() {
		if err := collectionLabelMetadata.export(ctx, tx, write); err != nil {
			return err
		}
	}
	if err := exportContentVersions(ctx, tx, write); err != nil {
		return err
	}
	if layout.hasPersons() {
		if err := exportMetadataTables(ctx, tx, write, personMetadataTables); err != nil {
			return err
		}
	}
	if layout.hasPostV3Metadata() {
		if err := exportMediaMetadata(ctx, tx, write); err != nil {
			return err
		}
		if err := exportVisualPreviews(ctx, tx, write); err != nil {
			return err
		}
	}
	if err := exportProvenance(ctx, tx, write); err != nil {
		return err
	}
	if layout.hasPostV3Metadata() {
		if err := provenanceVersionBindingMetadata.export(ctx, tx, write); err != nil {
			return err
		}
	}
	if layout.schemaVersion >= 17 {
		if err := exportPageMetadata(ctx, tx, write); err != nil {
			return err
		}
	}
	if layout.schemaVersion >= 18 {
		if err := exportBundleMetadata(ctx, tx, write); err != nil {
			return err
		}
	}
	if layout.schemaVersion >= 22 {
		if err := exportPackageMetadata(ctx, tx, write); err != nil {
			return err
		}
		if err := exportPackageImportMetadata(ctx, tx, write); err != nil {
			return err
		}
	}
	if layout.schemaVersion >= 26 {
		if err := exportBatesMetadata(ctx, tx, write); err != nil {
			return err
		}
		if err := exportBatesArtifactMetadata(ctx, tx, write); err != nil {
			return err
		}
	}
	if err := exportWatchSources(ctx, tx, write); err != nil {
		return err
	}
	if layout.hasPushSources() {
		if err := exportPushSources(ctx, tx, write); err != nil {
			return err
		}
	}
	if err := tagMetadata.export(ctx, tx, write); err != nil {
		return err
	}
	if layout.hasPostV3Metadata() {
		if err := savedQueryMetadata.export(ctx, tx, write); err != nil {
			return err
		}
		if err := exportSavedQueryRuns(ctx, tx, write); err != nil {
			return err
		}
	}
	if layout.schemaVersion >= 21 {
		if err := exportTermReportHistory(ctx, tx, write); err != nil {
			return err
		}
	}
	if err := nodeTagMetadata.export(ctx, tx, write); err != nil {
		return err
	}
	if layout.hasPostV3Metadata() {
		if err := batchTagReceiptMetadata.export(ctx, tx, write); err != nil {
			return err
		}
	}
	if err := exportExtractedText(ctx, tx, write, backupScoped); err != nil {
		return err
	}
	if err := exportAuditMetadata(ctx, tx, write); err != nil {
		return err
	}
	if !layout.hasPostV3Metadata() {
		return nil
	}
	if err := exportMetadataTables(ctx, tx, write, processingMetadataTables); err != nil {
		return err
	}
	if err := exportMetadataTables(ctx, tx, write, embeddingMetadataTables); err != nil {
		return err
	}
	if err := currentRenditionRootMetadata.export(ctx, tx, write); err != nil {
		return err
	}
	if err := exportMetadataTables(ctx, tx, write, emailMetadataTables); err != nil {
		return err
	}
	if layout.schemaVersion >= 13 {
		if err := exportEmailDocumentMetadata(ctx, tx, write); err != nil {
			return err
		}
	}
	if layout.schemaVersion >= 19 {
		if err := exportMailboxMetadata(ctx, tx, write); err != nil {
			return err
		}
	}
	if layout.schemaVersion >= 25 {
		if err := exportMetadataTables(ctx, tx, write, photoMetadataTablesForSchema(layout.schemaVersion)); err != nil {
			return err
		}
	}
	return derivativePurgeSuppressionMetadata.export(ctx, tx, write)
}

type metadataWrite func(any) error

// newMetadataJSONWriter is the single JSONL encoding boundary used by exports
// and exact projected-size calculations. Keeping both on this encoder prevents
// previews from estimating a different wire representation than backups use.
func newMetadataJSONWriter(w io.Writer) metadataWrite {
	enc := jsontext.NewEncoder(w)
	return func(v any) error {
		if err := json.MarshalEncode(enc, v, json.Deterministic(true), jsontext.EscapeForJS(true)); err != nil {
			return fmt.Errorf("encoding metadata: %w", err)
		}
		return nil
	}
}

func exportBlobs(
	ctx context.Context, tx metadataQuerier, write metadataWrite, backupScoped bool,
) error {
	query := `SELECT hash, size, created_at FROM blobs ORDER BY hash`
	if backupScoped {
		query = BackupBlobAuthorityCTE() + `
SELECT b.hash, b.size, b.created_at
FROM blobs b JOIN backup_authorized_blobs a ON a.hash = b.hash
ORDER BY b.hash`
	}
	rows, err := tx.QueryContext(ctx, query)
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

func exportBlobChecksums(
	ctx context.Context, tx metadataQuerier, write metadataWrite, backupScoped bool,
) error {
	query := `SELECT blob_sha256,md5 FROM blob_checksums ORDER BY blob_sha256`
	if backupScoped {
		query = BackupBlobAuthorityCTE() + `
SELECT c.blob_sha256,c.md5 FROM blob_checksums c
JOIN backup_authorized_blobs a ON a.hash=c.blob_sha256
ORDER BY c.blob_sha256`
	}
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("exporting blob checksums: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := metadataBlobChecksum{Type: metadataBlobChecksumType}
		if err := rows.Scan(&record.BlobSHA256, &record.MD5); err != nil {
			return fmt.Errorf("scanning blob checksum metadata: %w", err)
		}
		if err := validateBlobChecksumRecord(BlobChecksumRecord{
			BlobSHA256: record.BlobSHA256, MD5: record.MD5,
		}); err != nil {
			return fmt.Errorf("validating blob checksum metadata for export: %w", err)
		}
		if err := write(record); err != nil {
			return err
		}
	}
	return rowsError("blob checksum", rows)
}

func exportSourceMetadata(ctx context.Context, tx metadataQuerier, write metadataWrite, backupScoped bool) error {
	query := `SELECT generation_id,source_sha256,contract_version,extractor_fingerprint,
		canonical_json,checksum,created_at FROM source_metadata_generations ORDER BY generation_id`
	if backupScoped {
		query = BackupBlobAuthorityCTE() + `
SELECT g.generation_id,g.source_sha256,g.contract_version,g.extractor_fingerprint,
g.canonical_json,g.checksum,g.created_at FROM source_metadata_generations g
JOIN backup_authorized_blobs a ON a.hash=g.source_sha256 ORDER BY g.generation_id`
	}
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("exporting source metadata: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := metadataSourceMetadataGeneration{Type: metadataSourceMetadataGenerationType}
		if err := rows.Scan(&record.GenerationID, &record.SourceSHA256, &record.ContractVersion,
			&record.ExtractorFingerprint, &record.CanonicalJSON, &record.Checksum, &record.CreatedAt); err != nil {
			return err
		}
		if err := validateSourceMetadataGenerationRecord(record); err != nil {
			return fmt.Errorf("validating source metadata for export: %w", err)
		}
		if err := write(record); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	headQuery := `SELECT source_sha256,generation_id,published_at FROM source_metadata_heads ORDER BY source_sha256`
	if backupScoped {
		headQuery = BackupBlobAuthorityCTE() + `
SELECT h.source_sha256,h.generation_id,h.published_at FROM source_metadata_heads h
JOIN backup_authorized_blobs a ON a.hash=h.source_sha256 ORDER BY h.source_sha256`
	}
	heads, err := tx.QueryContext(ctx, headQuery)
	if err != nil {
		return err
	}
	defer func() { _ = heads.Close() }()
	for heads.Next() {
		record := metadataSourceMetadataHead{Type: metadataSourceMetadataHeadType}
		if err := heads.Scan(&record.SourceSHA256, &record.GenerationID, &record.PublishedAt); err != nil {
			return err
		}
		if err := write(record); err != nil {
			return err
		}
	}
	return heads.Err()
}

func validateSourceMetadataGenerationRecord(record metadataSourceMetadataGeneration) error {
	if record.Type != metadataSourceMetadataGenerationType || record.ContractVersion != document.SourceMetadataContractV1 {
		return errors.New("invalid source metadata generation record")
	}
	metadata, checksum, err := document.DecodeSourceMetadataV1(record.CanonicalJSON)
	if err != nil {
		return err
	}
	if parsed, parseErr := packstore.ParseHash(record.ExtractorFingerprint); parseErr != nil || parsed.String() != record.ExtractorFingerprint {
		return errors.New("source metadata extractor fingerprint is invalid")
	}
	if metadata.ContractVersion != record.ContractVersion || checksum != record.Checksum ||
		sourceMetadataGenerationID(record.SourceSHA256, record.ContractVersion, record.ExtractorFingerprint, record.Checksum) != record.GenerationID {
		return errors.New("source metadata generation identity or checksum does not match canonical evidence")
	}
	return nil
}

func exportVisualPreviews(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `SELECT generation_id,vault_uid,content_version_id,
		source_sha256,contract_version,recipe_fingerprint,canonical_result,checksum,created_at,
		state,output_blob_hash,output_size,output_media_type,output_width,output_height,
		failure_code,failure_detail
		FROM visual_preview_generations ORDER BY generation_id`)
	if err != nil {
		return fmt.Errorf("exporting visual preview generations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := metadataVisualPreviewGeneration{Type: metadataVisualPreviewGenerationType}
		var state string
		var outputHash, outputMediaType, failureCode, failureDetail sql.NullString
		var outputSize, outputWidth, outputHeight sql.NullInt64
		if err := rows.Scan(&record.GenerationID, &record.VaultID, &record.ContentVersionID,
			&record.SourceSHA256, &record.ContractVersion, &record.RecipeFingerprint,
			&record.CanonicalResult, &record.Checksum, &record.CreatedAt, &state,
			&outputHash, &outputSize, &outputMediaType, &outputWidth, &outputHeight,
			&failureCode, &failureDetail); err != nil {
			return fmt.Errorf("scanning visual preview generation: %w", err)
		}
		if err := validateVisualPreviewGenerationRecord(record); err != nil {
			return fmt.Errorf("validating visual preview generation for export: %w", err)
		}
		preview, _, err := document.DecodeVisualPreviewV1(record.CanonicalResult)
		if err != nil {
			return fmt.Errorf("validating visual preview generation for export: %w", err)
		}
		if err := validateVisualPreviewStorage(preview, record.SourceSHA256,
			record.ContractVersion, state, outputHash, outputSize, outputMediaType,
			outputWidth, outputHeight, failureCode, failureDetail); err != nil {
			return fmt.Errorf("validating visual preview generation for export: %w", err)
		}
		if err := write(record); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("exporting visual preview generations: %w", err)
	}
	return visualPreviewHeadMetadata.export(ctx, tx, write)
}

func validateVisualPreviewHeadRecord(v metadataVisualPreviewHead) error {
	if err := validateUUIDv4(v.ContentVersionID); err != nil {
		return fmt.Errorf("invalid visual preview head content version: %w", err)
	}
	if err := validateCatalogSHA256(v.GenerationID, "visual preview head generation ID"); err != nil {
		return err
	}
	return validateMetadataTime("visual preview head published_at", v.PublishedAt)
}

func validateVisualPreviewGenerationRecord(record metadataVisualPreviewGeneration) error {
	if record.Type != metadataVisualPreviewGenerationType ||
		record.ContractVersion != document.VisualPreviewContractV1 {
		return errors.New("invalid visual preview generation record")
	}
	if err := validateUUIDv4(record.VaultID); err != nil {
		return fmt.Errorf("invalid visual preview vault identity: %w", err)
	}
	if err := validateUUIDv4(record.ContentVersionID); err != nil {
		return fmt.Errorf("invalid visual preview content version: %w", err)
	}
	if err := validateMetadataTime("visual preview created_at", record.CreatedAt); err != nil {
		return err
	}
	preview, checksum, err := document.DecodeVisualPreviewV1(record.CanonicalResult)
	if err != nil {
		return err
	}
	_, recipeFingerprint, err := document.MarshalVisualPreviewRecipeV1(preview.Recipe)
	if err != nil {
		return err
	}
	if preview.SourceSHA256 != record.SourceSHA256 || checksum != record.Checksum ||
		recipeFingerprint != record.RecipeFingerprint ||
		visualPreviewGenerationID(record.ContentVersionID, recipeFingerprint, checksum) != record.GenerationID {
		return errors.New("visual preview generation identity does not match canonical result")
	}
	return nil
}

func exportNodes(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	return nodeMetadata.export(ctx, tx, write)
}

func exportContentVersions(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	return contentVersionMetadata.export(ctx, tx, write)
}

func exportIngests(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	return ingestMetadata.export(ctx, tx, write)
}

func exportProvenance(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	return provenanceMetadata.export(ctx, tx, write)
}

func exportWatchSources(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	return watchSourceMetadata.export(ctx, tx, write)
}

func exportPushSources(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	return pushSourceMetadata.export(ctx, tx, write)
}

func exportExtractedText(
	ctx context.Context, tx metadataQuerier, write metadataWrite, backupScoped bool,
) error {
	query := `
		SELECT blob_hash, extractor, extractor_version, status, error, attempts, text, extracted_at
		FROM extracted_text ORDER BY blob_hash, extractor`
	if backupScoped {
		query = BackupBlobAuthorityCTE() + `
		SELECT e.blob_hash, e.extractor, e.extractor_version, e.status,
		       e.error, e.attempts, e.text, e.extracted_at
		FROM extracted_text e
		JOIN backup_authorized_blobs a ON a.hash = e.blob_hash
		ORDER BY e.blob_hash, e.extractor`
	}
	rows, err := tx.QueryContext(ctx, query)
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

// ImportMetadata imports one deterministic logical metadata snapshot into a
// pristine target. It proves relationships and identities, not physical
// bytes: a restore must call VerifyRenditionBlobBytes after every loose or
// packed blob is available and before publishing the target.
func (s *Store) ImportMetadata(ctx context.Context, r io.Reader) error {
	return s.importMetadata(ctx, r)
}

func (s *Store) importMetadata(ctx context.Context, r io.Reader) error {
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
		header, err := s.importMetadataLines(ctx, tx, r)
		if err != nil {
			return err
		}
		if err := refreshPhotoTechnicalMetadataTx(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE vault_metadata SET vault_uid = ? WHERE singleton = 1`, header.VaultID,
		); err != nil {
			return fmt.Errorf("restoring vault identity: %w", err)
		}
		if err := validateMetadataState(ctx, tx, header.NodeSequence); err != nil {
			return err
		}
		if err := rebuildImportedTextExtractionStateTx(ctx, tx); err != nil {
			return err
		}
		if err := markImportedSupersededLexicalGenerationsTx(ctx, tx); err != nil {
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

// metadataPristineStateTables lists the tables a restore target must have empty
// that no backup record restores into: rebuildable projections, process state,
// the side tables custom inserts write, and the media tables until #733. A
// record's own table comes from its registration instead.
var metadataPristineStateTables = []string{
	"export_jobs", "document_event_state", "document_event_generations", "document_event_heads",
	"document_events", "document_event_actors", "document_event_primaries", "document_event_builds",
	"document_event_dirty", "document_event_attempts", "text_extraction_queue", "text_searchable_versions",
	"vector_index_generations", "vector_index_heads", "vector_index_build_jobs", "vector_index_reader_leases",
	"vector_index_unavailable_coverage", "rendition_blob_staging", "derivative_blob_purge_pending",
	"derivative_pack_purge_pending", "document_people_generations", "document_people_heads",
	"document_people_builds", "document_people", "package_preflights", "media_sources",
	"media_source_versions", "media_occurrences", "media_input_artifacts", "media_operations",
	"audit_baselines", "email_document_relations", "mailbox_chunks", "page_frames", "content_fts",
	"export_plans",
}

var pristineMetadataTables = pristineMetadataTableSum(metadataCodecs)

func requirePristineMetadataTarget(ctx context.Context, tx *sql.Tx) error {
	var nodes, other, packs int64
	if err := tx.QueryRowContext(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM nodes),
		  `+pristineMetadataTables+`
		    + ABS((SELECT COUNT(*) FROM document_people_state) - 1)
		    + (SELECT COUNT(*) FROM document_people_state
		       WHERE singleton != 1 OR binding_epoch != 1 OR publication_epoch != 1 OR resolver_fingerprint != '')
		    + (SELECT COUNT(*) FROM processing_incarnations
		       WHERE incarnation_id != (SELECT incarnation_id
		         FROM current_processing_incarnation WHERE singleton=1)),
		  (SELECT COUNT(*) FROM blob_locations)
		    + (SELECT COUNT(*) FROM blob_packs)
		    + (SELECT COUNT(*) FROM blob_pack_entries)
		    + (SELECT COUNT(*) FROM gc_loose_retirements)
	`).Scan(&nodes, &other, &packs); err != nil {
		return fmt.Errorf("checking metadata import target: %w", err)
	}
	if nodes != 1 || other != 0 || packs != 0 {
		return fmt.Errorf("metadata import target is not pristine: nodes=%d logical_rows=%d pack_rows=%d",
			nodes, other, packs)
	}
	for _, table := range []struct {
		name  string
		count string
	}{
		{"rendition_lexical_generations", `SELECT COUNT(*) FROM rendition_lexical_generations`},
		{"rendition_lexical_generation_manifests", `SELECT COUNT(*) FROM rendition_lexical_generation_manifests`},
		{"rendition_lexical_generation_builds", `SELECT COUNT(*) FROM rendition_lexical_generation_builds`},
		{"rendition_lexical_index", `SELECT COUNT(*) FROM rendition_lexical_index`},
		{"rendition_lexical_heads", `SELECT COUNT(*) FROM rendition_lexical_heads`},
	} {
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name=?)`, table.name,
		).Scan(&exists); err != nil {
			return fmt.Errorf("checking metadata import target for %s: %w", table.name, err)
		}
		if !exists {
			continue
		}
		var rows int64
		if err := tx.QueryRowContext(ctx, table.count).Scan(&rows); err != nil {
			return fmt.Errorf("counting metadata import target table %s: %w", table.name, err)
		}
		if rows != 0 {
			return fmt.Errorf("metadata import target is not pristine: %s_rows=%d", table.name, rows)
		}
	}
	return nil
}

func (s *Store) importMetadataLines(
	ctx context.Context, tx *sql.Tx, r io.Reader,
) (metadataHeader, error) {
	dec := jsontext.NewDecoder(bufio.NewReader(r))
	rawHeader, err := dec.ReadValue()
	if err != nil {
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
		raw, err := dec.ReadValue()
		if errors.Is(err, io.EOF) {
			return header, nil
		}
		if err != nil {
			return metadataHeader{}, fmt.Errorf("decoding metadata record %d: %w", record, err)
		}
		var kind struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &kind); err != nil {
			return metadataHeader{}, fmt.Errorf("decoding metadata record %d type: %w", record, err)
		}
		if err := s.importMetadataRecord(ctx, tx, kind.Type, raw); err != nil {
			return metadataHeader{}, fmt.Errorf("importing metadata record %d (%s): %w", record, kind.Type, err)
		}
	}
}

func (s *Store) importMetadataRecord(
	ctx context.Context, tx *sql.Tx, kind string, raw jsontext.Value,
) error {
	required, ok := metadataRequiredFields[kind]
	if !ok {
		return fmt.Errorf("unknown record type %q", kind)
	}
	if err := requireMetadataFields(raw, required, metadataNullableFields[kind]); err != nil {
		return err
	}
	if codec, ok := metadataCodecs[kind]; ok {
		return codec.importRecord(ctx, tx, raw)
	}
	if isMediaMetadataType(kind) {
		return s.importMediaMetadataRecord(ctx, tx, kind, raw)
	}
	return fmt.Errorf("unknown record type %q", kind)
}

const (
	metadataTypeField                     = "type"
	metadataNodeIDField                   = "node_id"
	metadataSizeField                     = "size"
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
	metadataProvenanceVersionBindingType  = "provenance_version_binding"
	metadataWatchSourceType               = "watch_source"
	metadataPushSourceType                = "push_source"
	metadataTagRecordType                 = "tag"
	metadataSavedQueryType                = "saved_query"
	metadataSavedQueryRunType             = "saved_query_run"
	metadataAuditAuthorityType            = "audit_authority"
	metadataAuditScopeType                = "audit_scope"
	metadataAuditMembershipType           = "audit_membership"
	metadataAuditRecordType               = "audit_record"
	metadataBlobChecksumType              = "blob_checksum"
	metadataSourceMetadataGenerationType  = "source_metadata_generation"
	metadataSourceMetadataHeadType        = "source_metadata_head"
	metadataVisualPreviewGenerationType   = "visual_preview_generation"
	metadataVisualPreviewHeadType         = "visual_preview_head"
	metadataPhotoAssetType                = "photo_asset"
	metadataPhotoFileType                 = "photo_file"
	metadataPhotoSettingsType             = "photo_library_settings"
	metadataPhotoReceiptType              = "photo_change_receipt"
)

var metadataHeaderFields = []string{metadataTypeField, "format", "version", auditVaultIDField, "node_sequence"}

func decodeMetadataRecord(raw jsontext.Value, dst any) error {
	return json.Unmarshal(raw, dst, json.RejectUnknownMembers(true))
}

func requireMetadataFields(raw jsontext.Value, required []string, nullable map[string]bool) error {
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
	// Nullable keys outside required come only from omitempty pointer fields, which may be absent.
	for field := range fields {
		if !allowed[field] && !nullable[field] {
			return fmt.Errorf("metadata record contains unknown or non-canonical field %q", field)
		}
	}
	return nil
}

func decodeMetadataFields(raw jsontext.Value) (map[string]jsontext.Value, error) {
	if raw.Kind() != jsontext.KindBeginObject {
		return nil, errors.New("metadata record must be a JSON object")
	}
	fields := make(map[string]jsontext.Value)
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("decoding metadata fields: %w", err)
	}
	return fields, nil
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
		return fmt.Errorf("%w: %w", ErrInvalidProvenanceTime, err)
	}
	if value != parsed.UTC().Format(time.RFC3339Nano) {
		return fmt.Errorf("%w: timestamp is not canonical UTC RFC3339Nano", ErrInvalidProvenanceTime)
	}
	return nil
}

func validateMetadataState(ctx context.Context, tx metadataQuerier, nodeSequence int64) error {
	return validateMetadataStateWithVaultIdentity(ctx, tx, nodeSequence, currentMetadataLayout())
}

func validateMetadataStateWithVaultIdentity(
	ctx context.Context, tx metadataQuerier, nodeSequence int64, layout metadataSourceLayout,
) error {
	vaultID, err := readVaultIdentity(ctx, tx, layout.legacyV090())
	if err != nil {
		return fmt.Errorf("reading vault identity: %w", err)
	}
	if err := validateUUIDv4(vaultID); err != nil {
		return fmt.Errorf("invalid vault identity: %w", err)
	}
	if err := validateMetadataRelations(ctx, tx); err != nil {
		return err
	}
	if layout.hasPostV3Metadata() {
		if err := validateProvenanceVersionBindingRelations(ctx, tx); err != nil {
			return err
		}
	}
	if layout.hasPushSources() {
		if err := validatePushSourceRelations(ctx, tx); err != nil {
			return err
		}
	}
	if err := validateWatchSourceRelations(ctx, tx); err != nil {
		return err
	}
	if layout.hasPostV3Metadata() {
		if err := validateCollectionLabelMetadataState(ctx, tx); err != nil {
			return err
		}
		if err := validateSavedQueryRunMetadataState(ctx, tx); err != nil {
			return err
		}
		if layout.schemaVersion >= 21 {
			if err := exportTermReportHistory(ctx, tx, func(any) error { return nil }); err != nil {
				return err
			}
		}
		if err := validateBatchTagReceiptMetadataState(ctx, tx); err != nil {
			return err
		}
		if err := validateMediaMetadataState(ctx, tx); err != nil {
			return err
		}
		if err := validateProcessingMetadataState(ctx, tx); err != nil {
			return err
		}
		if err := validateEmbeddingMetadataState(ctx, tx); err != nil {
			return err
		}
		if err := validateEmailMetadataState(ctx, tx); err != nil {
			return err
		}
		if err := validateVisualPreviewMetadataState(ctx, tx, vaultID); err != nil {
			return err
		}
		if layout.hasPersons() {
			if err := validatePersonMetadataState(ctx, tx); err != nil {
				return err
			}
		}
		if layout.schemaVersion >= 13 {
			if err := exportEmailDocumentMetadata(ctx, tx, func(any) error { return nil }); err != nil {
				return err
			}
		}
		if layout.schemaVersion >= 19 {
			if err := exportMailboxMetadata(ctx, tx, func(any) error { return nil }); err != nil {
				return err
			}
		}
		if layout.schemaVersion >= 25 {
			if err := validatePhotoMetadataState(ctx, tx, layout.schemaVersion); err != nil {
				return err
			}
		}
	}
	if layout.schemaVersion >= 17 {
		if err := exportPageMetadata(ctx, tx, func(any) error { return nil }); err != nil {
			return err
		}
	}
	if layout.schemaVersion >= 18 {
		if err := exportBundleMetadata(ctx, tx, func(any) error { return nil }); err != nil {
			return err
		}
	}
	if layout.schemaVersion >= 22 {
		if err := validatePackageMetadataState(ctx, tx, vaultID); err != nil {
			return err
		}
		if err := validatePackageImportMetadataState(ctx, tx); err != nil {
			return err
		}
	}
	if layout.schemaVersion >= 26 {
		if err := validateBatesMetadataState(ctx, tx); err != nil {
			return err
		}
		if err := validateBatesArtifactState(ctx, tx); err != nil {
			return err
		}
	}
	topology, err := loadAuditTopologyRows(ctx, tx)
	if err != nil {
		return err
	}
	if err := validateAuditTrashOrigins(topology); err != nil {
		return err
	}
	if err := validateAuditAuthorityForLayout(ctx, tx, vaultID, nodeSequence, layout); err != nil {
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

func validateVisualPreviewMetadataState(
	ctx context.Context, tx metadataQuerier, vaultID string,
) error {
	var mismatch bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM visual_preview_generations g
		JOIN content_versions v ON v.version_id=g.content_version_id
		WHERE g.vault_uid<>? OR g.source_sha256<>v.blob_hash
	)`, vaultID).Scan(&mismatch); err != nil {
		return fmt.Errorf("validating visual preview attachment: %w", err)
	}
	if mismatch {
		return errors.New("visual preview generation does not match its vault or content version")
	}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM visual_preview_generations g
		LEFT JOIN blobs b ON b.hash=g.output_blob_hash
		WHERE g.state='ready' AND (b.hash IS NULL OR b.size<>g.output_size)
	)`).Scan(&mismatch); err != nil {
		return fmt.Errorf("validating visual preview output blob: %w", err)
	}
	if mismatch {
		return errors.New("visual preview output size does not match its cataloged blob")
	}
	if err := exportVisualPreviews(ctx, tx, func(any) error { return nil }); err != nil {
		return fmt.Errorf("validating visual preview metadata: %w", err)
	}
	return nil
}

func readVaultIdentity(ctx context.Context, tx metadataQuerier, legacyV090 bool) (string, error) {
	query := `SELECT vault_uid FROM vault_metadata WHERE singleton = 1`
	if legacyV090 {
		query = `SELECT vault_id FROM vault_metadata WHERE singleton = 1`
	}
	var vaultID string
	if err := tx.QueryRowContext(ctx, query).Scan(&vaultID); err != nil {
		return "", err
	}
	return vaultID, nil
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

// markImportedSupersededLexicalGenerationsTx restores the collection markers
// that metadata JSONL does not carry. Export keeps a lexical generation only
// while the head, a rendition job, or a durable root needs it. Every imported
// generation other than the head is therefore superseded, and collection still
// skips it while a job or an active root holds it.
func markImportedSupersededLexicalGenerationsTx(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO rendition_lexical_superseded(generation_id)
		SELECT g.generation_id FROM rendition_lexical_generations g
		WHERE NOT EXISTS (SELECT 1 FROM rendition_lexical_heads h WHERE h.generation_id=g.generation_id)
		ON CONFLICT(generation_id) DO NOTHING`); err != nil {
		return fmt.Errorf("marking imported superseded lexical generations: %w", err)
	}
	return nil
}
