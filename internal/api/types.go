package api

import "go.kenn.io/docbank/internal/store"

const (
	// BlobHashHeader carries docbank's canonical lowercase SHA-256 identity.
	BlobHashHeader = "X-Docbank-Blob-Hash"
	// BlobSizeHeader carries the catalog's expected raw byte length. Content
	// streams use it instead of Content-Length so HTTP/1.1 can carry a digest
	// trailer computed while streaming without a second physical read.
	BlobSizeHeader = "X-Docbank-Blob-Size"
)

// Node is the wire representation of a store.Node. Path is only populated on
// single-node responses; list responses omit it.
type Node struct {
	ID         int64  `json:"id"`
	ParentID   *int64 `json:"parent_id,omitempty"`
	Name       string `json:"name"`
	Kind       string `json:"kind" enum:"dir,file"`
	BlobHash   string `json:"blob_hash,omitempty" pattern:"^[0-9a-f]{64}$"`
	Size       int64  `json:"size"`
	MimeType   string `json:"mime_type,omitempty"`
	Revision   int64  `json:"revision"`
	CreatedAt  string `json:"created_at"`
	ModifiedAt string `json:"modified_at"`
	TrashedAt  string `json:"trashed_at,omitempty"`
	Path       string `json:"path,omitempty"` // set on single-node responses only
}

// ContentVerification binds a fresh physical read to the exact node revision
// the caller inspected. BlobHash and Size are catalog identity; ComputedHash
// and ComputedSize describe the bytes read through the mixed store.
type ContentVerification struct {
	NodeID       int64  `json:"node_id"`
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
	Added   int             `json:"added"`
	Skipped int             `json:"skipped"`
	Failed  []IngestFailure `json:"failed,omitempty"`
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

func fromStoreNode(n store.Node) Node {
	out := Node{
		ID: n.ID, ParentID: n.ParentID, Name: n.Name, Kind: n.Kind,
		BlobHash: n.BlobHash, Size: n.Size, MimeType: n.MimeType, Revision: n.Revision,
		CreatedAt: n.CreatedAt, ModifiedAt: n.ModifiedAt,
	}
	if n.TrashedAt != nil {
		out.TrashedAt = *n.TrashedAt
	}
	return out
}
