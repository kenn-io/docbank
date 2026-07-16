package docbank

import (
	"io"

	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/store"
)

// ContentIdentity is the canonical identity of uncompressed document bytes.
type ContentIdentity struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Node is the current projection of one stable virtual-tree identity.
type Node struct {
	ID               int64   `json:"id"`
	ParentID         *int64  `json:"parent_id,omitempty"`
	Name             string  `json:"name"`
	Kind             string  `json:"kind"`
	CurrentVersionID string  `json:"current_version_id,omitempty"`
	BlobHash         string  `json:"blob_hash,omitempty"`
	Size             int64   `json:"size"`
	MediaType        string  `json:"media_type,omitempty"`
	Revision         int64   `json:"revision"`
	CreatedAt        string  `json:"created_at"`
	ModifiedAt       string  `json:"modified_at"`
	TrashedAt        *string `json:"trashed_at,omitempty"`
}

// ContentVersion is one immutable byte identity in a stable file's history.
type ContentVersion struct {
	ID                    string  `json:"id"`
	NodeID                int64   `json:"node_id"`
	BlobHash              string  `json:"blob_hash"`
	Size                  int64   `json:"size"`
	MediaType             string  `json:"media_type,omitempty"`
	RecordedAt            string  `json:"recorded_at"`
	NodeRevision          int64   `json:"node_revision"`
	IntroducedOperationID string  `json:"introduced_operation_id"`
	TransitionKind        string  `json:"transition_kind"`
	SourceVersionID       *string `json:"source_version_id,omitempty"`
}

// PutReceipt proves the computed identity and resulting logical authority.
type PutReceipt struct {
	Node     Node            `json:"node"`
	Version  ContentVersion  `json:"version"`
	Computed ContentIdentity `json:"computed"`
	Created  bool            `json:"created"`
	Replaced bool            `json:"replaced"`
}

// ChildrenOptions selects one bounded page of a directory's live children.
// A zero Limit uses DefaultChildrenLimit. Offset must not be negative.
type ChildrenOptions struct {
	Limit  int
	Offset int
}

// ChildrenPage is one bounded dirs-first, name-sorted child listing.
type ChildrenPage struct {
	Items  []Node `json:"items"`
	Total  int    `json:"total"`
	Limit  int    `json:"limit"`
	Offset int    `json:"offset"`
}

// PackOptions bounds one explicit embedded packing pass. MaxBytes is a soft
// committed raw-byte budget; zero is unlimited and negative values are
// rejected.
type PackOptions struct {
	MaxBytes int64
}

// PackReport summarizes one explicit packing and repair pass.
type PackReport struct {
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

// VerifiedReadCloser is a bounded-memory content reader. A caller must reach
// terminal io.EOF or call Verify successfully before treating bytes as valid;
// an early Close reports incomplete verification and never drains implicitly.
type VerifiedReadCloser interface {
	io.ReadCloser
	Verify() error
}

// Content binds a verified current-byte stream to its stable node projection.
type Content struct {
	Node   Node
	Reader VerifiedReadCloser
}

func fromStoreNode(node store.Node) Node {
	return Node{
		ID: node.ID, ParentID: node.ParentID, Name: node.Name, Kind: node.Kind,
		CurrentVersionID: node.CurrentVersionID, BlobHash: node.BlobHash,
		Size: node.Size, MediaType: node.MimeType, Revision: node.Revision,
		CreatedAt: node.CreatedAt, ModifiedAt: node.ModifiedAt, TrashedAt: node.TrashedAt,
	}
}

func fromStoreVersion(version store.ContentVersion) ContentVersion {
	return ContentVersion{
		ID: version.ID, NodeID: version.NodeID, BlobHash: version.BlobHash,
		Size: version.Size, MediaType: version.MimeType, RecordedAt: version.RecordedAt,
		NodeRevision:          version.NodeRevision,
		IntroducedOperationID: version.IntroducedOperationID,
		TransitionKind:        version.TransitionKind, SourceVersionID: version.SourceVersionID,
	}
}

func fromPackStats(stats packstore.PackStats) PackReport {
	return PackReport{
		PacksSealed: stats.PacksSealed, BlobsPacked: stats.BlobsPacked,
		BytesPacked: stats.BytesPacked, PacksAdopted: stats.PacksAdopted,
		PacksRemoved: stats.PacksRemoved, PacksQuarantined: stats.PacksQuarantined,
		PacksUnreadable: stats.PacksUnreadable, RecordsDropped: stats.RecordsDropped,
		MappingsPruned: stats.MappingsPruned, BlobsMissing: stats.BlobsMissing,
		BlobsCorrupt: stats.BlobsCorrupt, BlobsDeferredOversized: stats.BlobsDeferredOversized,
		PacksDeferredOversized: stats.PacksDeferredOversized, LooseSwept: stats.LooseSwept,
		LooseOrphansRemoved:        stats.LooseOrphansRemoved,
		LooseOrphanSweepSuppressed: stats.LooseOrphanSweepSuppressed,
		BudgetExhausted:            stats.BudgetExhausted,
	}
}
