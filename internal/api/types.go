package api

import "go.kenn.io/docbank/internal/store"

const openAPIStringType = "string"

const (
	// BlobHashHeader carries docbank's canonical lowercase SHA-256 identity.
	BlobHashHeader = "X-Docbank-Blob-Hash"
	// BlobSizeHeader carries the catalog's expected raw byte length. Content
	// streams use it instead of Content-Length so HTTP/1.1 can carry a digest
	// trailer computed while streaming without a second physical read.
	BlobSizeHeader = "X-Docbank-Blob-Size"
	// ContentVersionHeader carries the stable version identity whose immutable
	// bytes are being streamed.
	ContentVersionHeader = "X-Docbank-Content-Version"
)

// Node is the wire representation of a store.Node. Path is only populated on
// single-node responses; list responses omit it.
type Node struct {
	ID               int64  `json:"id"`
	ParentID         *int64 `json:"parent_id,omitempty"`
	Name             string `json:"name"`
	Kind             string `json:"kind" enum:"dir,file"`
	CurrentVersionID string `json:"current_version_id,omitempty" format:"uuid"`
	BlobHash         string `json:"blob_hash,omitempty" pattern:"^[0-9a-f]{64}$"`
	Size             int64  `json:"size"`
	MimeType         string `json:"mime_type,omitempty"`
	Revision         int64  `json:"revision"`
	CreatedAt        string `json:"created_at"`
	ModifiedAt       string `json:"modified_at"`
	TrashedAt        string `json:"trashed_at,omitempty"`
	Path             string `json:"path,omitempty"` // set on single-node responses only
}

// ContentVersion is the wire representation of an immutable version record.
type ContentVersion struct {
	ID                    string  `json:"id" format:"uuid"`
	NodeID                int64   `json:"node_id"`
	BlobHash              string  `json:"blob_hash" pattern:"^[0-9a-f]{64}$"`
	Size                  int64   `json:"size" minimum:"0"`
	MimeType              string  `json:"mime_type,omitempty"`
	RecordedAt            string  `json:"recorded_at"`
	NodeRevision          int64   `json:"node_revision" minimum:"1"`
	IntroducedOperationID string  `json:"introduced_operation_id" format:"uuid"`
	TransitionKind        string  `json:"transition_kind" enum:"content_create,content_replace,content_revert"`
	SourceVersionID       *string `json:"source_version_id,omitempty" format:"uuid"`
}

// ContentVersionPage is one bounded newest-first version listing.
type ContentVersionPage struct {
	Items  []ContentVersion `json:"items"`
	Total  int              `json:"total"`
	Limit  int              `json:"limit"`
	Offset int              `json:"offset"`
}

// VersionPruneRequest selects one explicit history-pruning policy. Exactly one
// of VersionIDs, KeepNewest, OlderThan, or AllPrior must be set.
type VersionPruneRequest struct {
	VersionIDs []string `json:"version_ids,omitempty" format:"uuid" minItems:"1" maxItems:"1000" uniqueItems:"true"`
	KeepNewest int      `json:"keep_newest,omitempty" minimum:"1"`
	OlderThan  string   `json:"older_than,omitempty" example:"90d"`
	AllPrior   bool     `json:"all_prior,omitempty"`
	Run        bool     `json:"run,omitempty" default:"false"`
}

// VersionPruneReport distinguishes released logical history from physical
// bytes that only a later GC/repack can reclaim.
type VersionPruneReport struct {
	Node                     Node             `json:"node"`
	Candidates               []ContentVersion `json:"candidates"`
	DependencyRetained       []ContentVersion `json:"dependency_retained"`
	Checkpoint               *ContentVersion  `json:"checkpoint,omitempty"`
	Cutoff                   string           `json:"cutoff,omitempty"`
	LogicalBytes             int64            `json:"logical_bytes" minimum:"0"`
	UniqueBlobs              int              `json:"unique_blobs" minimum:"0"`
	SharedBlobs              int              `json:"shared_blobs" minimum:"0"`
	ReleasableBlobs          int              `json:"releasable_blobs" minimum:"0"`
	ReleasableBytes          int64            `json:"releasable_bytes" minimum:"0"`
	LooseBlobsPendingGC      int              `json:"loose_blobs_pending_gc" minimum:"0"`
	LooseBytesPendingGC      int64            `json:"loose_bytes_pending_gc" minimum:"0"`
	PackedBlobsPendingRepack int              `json:"packed_blobs_pending_repack" minimum:"0"`
	PackedBytesPendingRepack int64            `json:"packed_bytes_pending_repack" minimum:"0"`
	DeletedVersions          int              `json:"deleted_versions" minimum:"0"`
	CheckpointRequired       bool             `json:"checkpoint_required"`
	Changed                  bool             `json:"changed"`
	Run                      bool             `json:"run"`
}

// ContentReference identifies one stable node/version pair that retains a
// requested content hash. Path is present only while the node is live.
type ContentReference struct {
	Version   ContentVersion `json:"version"`
	Node      Node           `json:"node"`
	Path      string         `json:"path,omitempty"`
	IsCurrent bool           `json:"is_current"`
}

// ContentReferencePage is one bounded content-hash lookup page.
type ContentReferencePage struct {
	Items  []ContentReference `json:"items"`
	Total  int                `json:"total"`
	Limit  int                `json:"limit"`
	Offset int                `json:"offset"`
}

// Tag is one stable organization label. Names may change; IDs never do.
type Tag struct {
	ID              string `json:"id" format:"uuid"`
	Name            string `json:"name"`
	Revision        int64  `json:"revision" minimum:"1"`
	AssignmentCount int    `json:"assignment_count" minimum:"0"`
}

// TagPage is one bounded name-sorted tag listing.
type TagPage struct {
	Items  []Tag `json:"items"`
	Total  int   `json:"total"`
	Limit  int   `json:"limit"`
	Offset int   `json:"offset"`
}

// TaggedNode pairs a tagged node with its live path. Trashed nodes deliberately
// omit Path because their former display coordinate is not resolvable.
type TaggedNode struct {
	Node Node   `json:"node"`
	Path string `json:"path,omitempty"`
}

// TaggedNodePage is one bounded stable-ID-sorted reverse tag lookup.
type TaggedNodePage struct {
	Items  []TaggedNode `json:"items"`
	Total  int          `json:"total"`
	Limit  int          `json:"limit"`
	Offset int          `json:"offset"`
}

// TagAssignmentReceipt records whether an idempotent assignment request
// changed authority and returns the resulting tag and node projections.
type TagAssignmentReceipt struct {
	Tag     Tag  `json:"tag"`
	Node    Node `json:"node"`
	Changed bool `json:"changed"`
}

// TagDeletionReceipt reports the removed definition and assignment closure.
type TagDeletionReceipt struct {
	Tag                Tag `json:"tag"`
	RemovedAssignments int `json:"removed_assignments" minimum:"0"`
}

// ContentVerification binds a fresh physical read to the exact node revision
// the caller inspected. BlobHash and Size are catalog identity; ComputedHash
// and ComputedSize describe the bytes read through the mixed store.
type ContentVerification struct {
	NodeID       int64  `json:"node_id"`
	VersionID    string `json:"version_id" format:"uuid"`
	Revision     int64  `json:"revision"`
	BlobHash     string `json:"blob_hash" pattern:"^[0-9a-f]{64}$"`
	Size         int64  `json:"size"`
	ComputedHash string `json:"computed_hash,omitempty" pattern:"^[0-9a-f]{64}$"`
	ComputedSize int64  `json:"computed_size"`
	Verified     bool   `json:"verified"`
	Problem      string `json:"problem,omitempty" enum:"missing,corrupt,unreadable"`
}

// UploadReceipt proves which bytes the daemon computed and which stable node
// now names them. Status is "added" for a new node and "skipped" for an
// idempotent retry that converged on an existing node.
type UploadReceipt struct {
	Status       string `json:"status" enum:"added,skipped"`
	Node         Node   `json:"node"`
	ComputedHash string `json:"computed_hash" pattern:"^[0-9a-f]{64}$"`
	ComputedSize int64  `json:"computed_size"`
}

// ContentReplacementReceipt proves which bytes the daemon received and which
// immutable head the optimistic replacement installed.
type ContentReplacementReceipt struct {
	Node         Node           `json:"node"`
	Version      ContentVersion `json:"version"`
	ComputedHash string         `json:"computed_hash" pattern:"^[0-9a-f]{64}$"`
	ComputedSize int64          `json:"computed_size" minimum:"0"`
}

// ContentReversionReceipt proves which immutable source authority was adopted
// and which new history row became the node's current head.
type ContentReversionReceipt struct {
	Node          Node           `json:"node"`
	Version       ContentVersion `json:"version"`
	SourceVersion ContentVersion `json:"source_version"`
}

// SearchHit pairs a matched node with its display path.
type SearchHit struct {
	Node Node   `json:"node"`
	Path string `json:"path"`
}

// SearchReport is one bounded search result page.
type SearchReport struct {
	Hits      []SearchHit `json:"hits"`
	Limit     int         `json:"limit"`
	Truncated bool        `json:"truncated"`
}

// IngestFailure records one source path that failed to import.
type IngestFailure struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// IngestReport summarizes an ingest run.
type IngestReport struct {
	Added    int             `json:"added"`
	Skipped  int             `json:"skipped"`
	Excluded int             `json:"excluded"`
	Failed   []IngestFailure `json:"failed,omitempty"`
}

// IngestProgress is one structured update from a server-side import. Scan
// establishes totals without opening content; ingest counts bytes actually
// read and files whose individual import attempt has completed.
type IngestProgress struct {
	Stage      string `json:"stage" enum:"scan,ingest"`
	Done       int64  `json:"done"`
	Total      int64  `json:"total"`
	BytesDone  int64  `json:"bytes_done"`
	BytesTotal int64  `json:"bytes_total"`
	Added      int    `json:"added"`
	Skipped    int    `json:"skipped"`
	Excluded   int    `json:"excluded"`
	Failed     int    `json:"failed"`
	Final      bool   `json:"final"`
}

// IngestEvent is one line of the ingest NDJSON stream. A report or error is
// terminal; progress may appear zero or more times before it.
type IngestEvent struct {
	Type     string          `json:"type" enum:"progress,result,error"`
	Progress *IngestProgress `json:"progress,omitempty"`
	Report   *IngestReport   `json:"report,omitempty"`
	Error    *Error          `json:"error,omitempty"`
}

// IngestSizeClass summarizes files in one storage-policy outcome.
type IngestSizeClass struct {
	Files int64 `json:"files"`
	Bytes int64 `json:"bytes"`
}

// IngestFileType summarizes regular files by lowercase extension. Extension
// is empty for names without an extension.
type IngestFileType struct {
	Extension string `json:"extension"`
	Files     int64  `json:"files"`
	Bytes     int64  `json:"bytes"`
}

// IngestPreflightFinding is one bounded sample from a source scan.
type IngestPreflightFinding struct {
	Path   string `json:"path"`
	Kind   string `json:"kind" enum:"excluded,skipped,error"`
	Detail string `json:"detail"`
}

// IngestPreflightReport inventories a prospective server-side import without
// opening file content or mutating the vault.
type IngestPreflightReport struct {
	Files        int64 `json:"files"`
	Directories  int64 `json:"directories"`
	LogicalBytes int64 `json:"logical_bytes"`

	PackEligible IngestSizeClass `json:"pack_eligible"`
	LooseOnly    IngestSizeClass `json:"loose_only"`
	Rejected     IngestSizeClass `json:"rejected"`

	Excluded int64 `json:"excluded"`
	Skipped  int64 `json:"skipped"`
	Errors   int64 `json:"errors"`

	FileTypes          []IngestFileType         `json:"file_types"`
	OtherFileTypes     IngestSizeClass          `json:"other_file_types"`
	FileTypesTruncated bool                     `json:"file_types_truncated"`
	Findings           []IngestPreflightFinding `json:"findings"`
	FindingsTruncated  bool                     `json:"findings_truncated"`
}

// TrashEmptyReport summarizes a trash-empty dry run or execution.
type TrashEmptyReport struct {
	CandidateRoots int64 `json:"candidate_roots"`
	Deleted        int64 `json:"deleted"`
	Run            bool  `json:"run"`
}

// GCReport separates immediate loose-file reclamation from immutable pack
// space made logically dead and pending a later repack.
type GCReport struct {
	CandidateBlobs     int   `json:"candidate_blobs"`
	UntrackedFiles     int   `json:"untracked_files"`
	ReclaimableBytes   int64 `json:"reclaimable_bytes"`
	PendingPackedBlobs int   `json:"pending_packed_blobs"`
	PendingPackedBytes int64 `json:"pending_packed_bytes"`
	ReclaimedFiles     int   `json:"reclaimed_files"`
	RemovedBlobs       int   `json:"removed_blobs"`
	Removed            int   `json:"removed"` // total removed records/files; retained for wire compatibility
	Run                bool  `json:"run"`
}

// StorageStatus reports physical loose inventory and catalog-authorized pack
// usage. PackStoredBytes includes both live and logically dead payload bytes.
type StorageStatus struct {
	LooseBlobs        int   `json:"loose_blobs"`
	LooseBytes        int64 `json:"loose_bytes"`
	Packs             int   `json:"packs"`
	PackStoredBytes   int64 `json:"pack_stored_bytes"`
	PackedBlobs       int64 `json:"packed_blobs"`
	PackedRawBytes    int64 `json:"packed_raw_bytes"`
	PackedStoredBytes int64 `json:"packed_stored_bytes"`
	DeadPackedBytes   int64 `json:"dead_packed_bytes"`
}

// StoragePackReport summarizes one explicit Kit packing and repair pass.
type StoragePackReport struct {
	PacksSealed                int   `json:"packs_sealed"`
	BlobsPacked                int   `json:"blobs_packed"`
	BytesPacked                int64 `json:"bytes_packed"`
	PacksAdopted               int   `json:"packs_adopted"`
	PacksRemoved               int   `json:"packs_removed"`
	PacksQuarantined           int   `json:"packs_quarantined"`
	PacksUnreadable            int   `json:"packs_unreadable"`
	RecordsDropped             int   `json:"records_dropped"`
	MappingsPruned             int64 `json:"mappings_pruned"`
	BlobsMissing               int   `json:"blobs_missing"`
	BlobsCorrupt               int   `json:"blobs_corrupt"`
	BlobsDeferredOversized     int   `json:"blobs_deferred_oversized"`
	PacksDeferredOversized     int   `json:"packs_deferred_oversized"`
	LooseSwept                 int   `json:"loose_swept"`
	LooseOrphansRemoved        int   `json:"loose_orphans_removed"`
	LooseOrphanSweepSuppressed bool  `json:"loose_orphan_sweep_suppressed"`
	BudgetExhausted            bool  `json:"budget_exhausted"`
}

// StorageRepackReport summarizes sparse-pack selection, rewriting, and
// reader-safe retirement. BytesRepacked is live raw content rewritten, not a
// measurement of filesystem bytes reclaimed.
type StorageRepackReport struct {
	MappingsPruned         int64 `json:"mappings_pruned"`
	PacksSelected          int   `json:"packs_selected"`
	PacksRewritten         int   `json:"packs_rewritten"`
	PacksSealed            int   `json:"packs_sealed"`
	PacksRemoved           int   `json:"packs_removed"`
	PacksDeferredOversized int   `json:"packs_deferred_oversized"`
	BlobsRepacked          int   `json:"blobs_repacked"`
	BytesRepacked          int64 `json:"bytes_repacked"`
	BudgetExhausted        bool  `json:"budget_exhausted"`
}

// VerifyProblem flags one blob whose content didn't check out.
type VerifyProblem struct {
	Hash    string `json:"hash"`
	Problem string `json:"problem" enum:"missing,corrupt,unreadable"`
}

// VerifyReport summarizes a full blob verification pass.
type VerifyReport struct {
	OK       int             `json:"ok"`
	Problems []VerifyProblem `json:"problems,omitempty"`
}

// Job is the observable state of one daemon-owned background task. Names are
// stable within a daemon run and terminal records remain visible until restart.
type Job struct {
	Name       string `json:"name"`
	Status     string `json:"status" enum:"running,completed,failed,cancelled"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`
	Error      string `json:"error,omitempty"`
}

// JobList is returned as an object so the contract can gain aggregate state
// without changing a top-level JSON array.
type JobList struct {
	Items []Job `json:"items"`
}

// BackupRepository identifies an initialized Kit snapshot repository.
type BackupRepository struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

// BackupSnapshot is the stable API summary of one Docbank snapshot manifest.
// It deliberately omits Kit's physical pack/index details.
type BackupSnapshot struct {
	ID              string  `json:"id"`
	ParentID        string  `json:"parent_id,omitempty"`
	CreatedAt       string  `json:"created_at"`
	Tag             string  `json:"tag,omitempty"`
	MetadataFormat  string  `json:"metadata_format"`
	Nodes           int64   `json:"nodes"`
	Files           int64   `json:"files"`
	Blobs           int64   `json:"blobs"`
	BlobBytes       int64   `json:"blob_bytes"`
	PacksAdded      int     `json:"packs_added"`
	BytesAdded      int64   `json:"bytes_added"`
	DurationSeconds float64 `json:"duration_seconds"`
}

// BackupSnapshotList is returned as an object so later pagination can be
// added without changing a top-level JSON array contract.
type BackupSnapshotList struct {
	Items []BackupSnapshot `json:"items"`
}

// BackupProgress is one structured update from a long-running backup
// operation. Totals are zero when Kit cannot know them in advance.
type BackupProgress struct {
	Stage      string `json:"stage"`
	Done       int64  `json:"done"`
	Total      int64  `json:"total"`
	BytesDone  int64  `json:"bytes_done"`
	BytesTotal int64  `json:"bytes_total"`
	Final      bool   `json:"final"`
}

// BackupCreateEvent is one line of the backup-create NDJSON stream. A result
// or error is terminal; progress may appear zero or more times before it.
type BackupCreateEvent struct {
	Type     string          `json:"type" enum:"progress,result,error"`
	Progress *BackupProgress `json:"progress,omitempty"`
	Snapshot *BackupSnapshot `json:"snapshot,omitempty"`
	Error    *Error          `json:"error,omitempty"`
}

// BackupVerifyProblem identifies one repository-integrity failure and the
// snapshot whose logical state exposed it.
type BackupVerifyProblem struct {
	SnapshotID string `json:"snapshot_id"`
	Detail     string `json:"detail"`
}

// BackupVerifyReport summarizes one completed repository verification pass.
// Problems are findings rather than transport failures, so the API returns the
// complete report and lets callers decide how to present a failed proof.
type BackupVerifyReport struct {
	Snapshots    []string              `json:"snapshots"`
	BlobsChecked int64                 `json:"blobs_checked"`
	BytesRead    int64                 `json:"bytes_read"`
	Problems     []BackupVerifyProblem `json:"problems"`
}

// BackupVerifyEvent is one line of the backup-verify NDJSON stream. A report
// or error is terminal; progress may appear zero or more times before it.
type BackupVerifyEvent struct {
	Type     string              `json:"type" enum:"progress,result,error"`
	Progress *BackupProgress     `json:"progress,omitempty"`
	Report   *BackupVerifyReport `json:"report,omitempty"`
	Error    *Error              `json:"error,omitempty"`
}

// BackupRestoreFallback summarizes content that could not retain its source
// pack representation and was verified and published loose instead.
type BackupRestoreFallback struct {
	Reason string `json:"reason" enum:"pack_container_limit,pack_footer_limit,pack_entry_count_limit,pack_encoding,pack_publication,blob_limit"`
	Count  int    `json:"count" minimum:"1"`
}

// BackupRestoreProof makes the successful restore contract explicit to API
// clients. A restore report is returned only after all three checks pass.
type BackupRestoreProof struct {
	ContentVerified bool `json:"content_verified"`
	SQLiteIntegrity bool `json:"sqlite_integrity"`
	ManifestStats   bool `json:"manifest_stats"`
}

// BackupRestoreReport summarizes a completely materialized and proved vault.
type BackupRestoreReport struct {
	SnapshotID      string                  `json:"snapshot_id"`
	Target          string                  `json:"target"`
	DatabasePath    string                  `json:"database_path"`
	DatabaseBytes   int64                   `json:"database_bytes"`
	DocumentBlobs   int64                   `json:"document_blobs"`
	DocumentBytes   int64                   `json:"document_bytes"`
	PackedBlobs     int64                   `json:"packed_blobs"`
	LooseBlobs      int64                   `json:"loose_blobs"`
	Packs           int                     `json:"packs"`
	Fallbacks       []BackupRestoreFallback `json:"fallbacks"`
	ExtrasFiles     int                     `json:"extras_files"`
	DurationSeconds float64                 `json:"duration_seconds"`
	Proof           BackupRestoreProof      `json:"proof"`
}

// BackupRestoreEvent is one line of the backup-restore NDJSON stream. A report
// or error is terminal; progress may appear zero or more times before it.
type BackupRestoreEvent struct {
	Type     string               `json:"type" enum:"progress,result,error"`
	Progress *BackupProgress      `json:"progress,omitempty"`
	Report   *BackupRestoreReport `json:"report,omitempty"`
	Error    *Error               `json:"error,omitempty"`
}

func fromStoreNode(n store.Node) Node {
	out := Node{
		ID: n.ID, ParentID: n.ParentID, Name: n.Name, Kind: n.Kind,
		CurrentVersionID: n.CurrentVersionID, BlobHash: n.BlobHash,
		Size: n.Size, MimeType: n.MimeType, Revision: n.Revision,
		CreatedAt: n.CreatedAt, ModifiedAt: n.ModifiedAt,
	}
	if n.TrashedAt != nil {
		out.TrashedAt = *n.TrashedAt
	}
	return out
}

func fromStoreContentVersion(v store.ContentVersion) ContentVersion {
	return ContentVersion{
		ID: v.ID, NodeID: v.NodeID, BlobHash: v.BlobHash, Size: v.Size,
		MimeType: v.MimeType, RecordedAt: v.RecordedAt, NodeRevision: v.NodeRevision,
		IntroducedOperationID: v.IntroducedOperationID,
		TransitionKind:        v.TransitionKind, SourceVersionID: v.SourceVersionID,
	}
}

func fromStoreVersionPruneResult(result store.VersionPruneResult) VersionPruneReport {
	report := VersionPruneReport{
		Node: fromStoreNode(result.Node), Candidates: []ContentVersion{},
		DependencyRetained: []ContentVersion{}, Cutoff: result.Cutoff,
		LogicalBytes: result.LogicalBytes, UniqueBlobs: result.UniqueBlobs,
		SharedBlobs: result.SharedBlobs, ReleasableBlobs: result.ReleasableBlobs,
		ReleasableBytes:          result.ReleasableBytes,
		LooseBlobsPendingGC:      result.LooseBlobsPendingGC,
		LooseBytesPendingGC:      result.LooseBytesPendingGC,
		PackedBlobsPendingRepack: result.PackedBlobsPendingRepack,
		PackedBytesPendingRepack: result.PackedBytesPendingRepack,
		DeletedVersions:          result.DeletedVersions,
		CheckpointRequired:       result.CheckpointRequired,
		Changed:                  result.Changed, Run: result.Run,
	}
	for _, version := range result.Candidates {
		report.Candidates = append(report.Candidates, fromStoreContentVersion(version))
	}
	for _, version := range result.DependencyRetained {
		report.DependencyRetained = append(
			report.DependencyRetained, fromStoreContentVersion(version),
		)
	}
	if result.Checkpoint != nil {
		checkpoint := fromStoreContentVersion(*result.Checkpoint)
		report.Checkpoint = &checkpoint
	}
	return report
}

func fromStoreContentReference(ref store.ContentReference) ContentReference {
	return ContentReference{
		Version: fromStoreContentVersion(ref.Version), Node: fromStoreNode(ref.Node),
		Path: ref.Path, IsCurrent: ref.IsCurrent,
	}
}

func fromStoreTag(tag store.Tag) Tag {
	return Tag{
		ID: tag.ID, Name: tag.Name, Revision: tag.Revision,
		AssignmentCount: tag.AssignmentCount,
	}
}
