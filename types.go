package docbank

import (
	"io"
	"time"

	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

const (
	// MaxDocumentSourceFenceIDs bounds the stable content-version authority a
	// consumer may supply to one search or coverage request.
	MaxDocumentSourceFenceIDs = 4096
	// DefaultDocumentSearchLimit is used when DocumentSearchRequest.Limit is zero.
	DefaultDocumentSearchLimit = 20
	// MaxDocumentSearchLimit bounds one public document-search response.
	MaxDocumentSearchLimit = 100
	// MaxRenditionBytes bounds one embedded rendition read.
	MaxRenditionBytes = int64(64 << 20)
)

// ProcessingOptions binds named portable profiles to process-local provider
// implementations. Secrets remain inside the provider values; profiles and
// plans contain only immutable non-secret identity.
type ProcessingOptions struct {
	Profiles       map[string]ProcessingProfileConfig
	SpoolDirectory string
}

type EmbeddingFailureClass string

const (
	EmbeddingFailureTransient EmbeddingFailureClass = "transient"
	EmbeddingFailurePermanent EmbeddingFailureClass = "permanent"
	EmbeddingFailureCapacity  EmbeddingFailureClass = "capacity"
)

// EmbeddingErrorClassifier lets an embedded provider preserve its retry and
// capacity semantics without exposing provider-specific errors to core.
type EmbeddingErrorClassifier func(error) (EmbeddingFailureClass, time.Duration)

// ProcessingProfileConfig supplies the exact providers executable for one
// named portable profile. EmbeddingProviders is keyed by binding name.
type ProcessingProfileConfig struct {
	Profile              document.ProcessingProfileV1
	RenditionProvider    document.RenditionProvider
	EmbeddingProviders   map[string]document.EmbeddingProvider
	EmbeddingClassifiers map[string]EmbeddingErrorClassifier
	Tokenizers           map[string]document.Tokenizer
}

// ProcessingSelector binds work to one stable node, immutable content
// version, and named processing profile.
type ProcessingSelector struct {
	NodeID           int64  `json:"node_id"`
	ContentVersionID string `json:"content_version_id"`
	Profile          string `json:"profile"`
}

type ProcessingPlanRequest struct {
	Selector ProcessingSelector `json:"selector"`
}

type ProcessingFlowHop struct {
	Capability    string   `json:"capability"`
	ProviderID    string   `json:"provider_id"`
	TrustBoundary string   `json:"trust_boundary"`
	InputClasses  []string `json:"input_classes"`
}

type ProcessingEstimate struct {
	SourceBytes   int64 `json:"source_bytes"`
	ProviderCalls int   `json:"provider_calls"`
	VectorSpaces  int   `json:"vector_spaces"`
}

// ProcessingPlan is the complete bounded disclosure reviewed before work is
// enqueued. Fingerprint covers every other field.
type ProcessingPlan struct {
	Fingerprint        string              `json:"fingerprint"`
	VaultUID           string              `json:"vault_uid"`
	Selector           ProcessingSelector  `json:"selector"`
	ProfileFingerprint string              `json:"profile_fingerprint"`
	Flow               []ProcessingFlowHop `json:"flow"`
	DisclosedClasses   []string            `json:"disclosed_classes"`
	RetainedClasses    []string            `json:"retained_classes"`
	Estimate           ProcessingEstimate  `json:"estimate"`
	ConsentRequired    bool                `json:"consent_required"`
	BackupConsequence  string              `json:"backup_consequence"`
}

type StartProcessingRequest struct {
	PlanRequest     ProcessingPlanRequest `json:"plan_request"`
	PlanFingerprint string                `json:"plan_fingerprint"`
	Consent         bool                  `json:"consent"`
}

type ProcessingJob struct {
	ID                 string   `json:"id"`
	RenditionJobID     string   `json:"rendition_job_id,omitzero"`
	AttachmentID       string   `json:"attachment_id,omitzero"`
	EmbeddingJobIDs    []string `json:"embedding_job_ids"`
	ProfileFingerprint string   `json:"profile_fingerprint"`
	ContentVersionID   string   `json:"content_version_id"`
}

type ProcessingStatusRequest struct {
	JobID string `json:"job_id"`
}

type ProcessingStatus struct {
	JobID             string   `json:"job_id"`
	State             string   `json:"state"`
	Phase             string   `json:"phase"`
	FailureCode       string   `json:"failure_code,omitzero"`
	EmbeddingJobIDs   []string `json:"embedding_job_ids"`
	CompletedBindings int      `json:"completed_bindings"`
}

type RenditionRequest struct {
	Selector ProcessingSelector `json:"selector"`
	MaxBytes int64              `json:"max_bytes,omitzero"`
}

// RenditionContent exposes one exact active sanitized-Markdown artifact. The
// reader holds a vault lifecycle lease until Close.
type RenditionContent struct {
	VaultUID           string
	NodeID             int64
	ContentVersionID   string
	ProfileFingerprint string
	AttachmentID       string
	BuildID            string
	ArtifactID         string
	SHA256             string
	Size               int64
	Completeness       string
	Warnings           []string
	Reader             VerifiedReadCloser
}

type DocumentSourceFence struct {
	VaultUID          string   `json:"vault_uid"`
	ContentVersionIDs []string `json:"content_version_ids"`
}

type CoverageRequest struct {
	Profile string              `json:"profile"`
	Fence   DocumentSourceFence `json:"fence"`
}

type CoverageClass struct {
	Name        string `json:"name"`
	Required    bool   `json:"required"`
	State       string `json:"state"`
	Complete    int    `json:"complete"`
	Unavailable int    `json:"unavailable"`
	Stale       int    `json:"stale"`
	Ineligible  int    `json:"ineligible"`
	Total       int    `json:"total"`
}

type CoverageReport struct {
	VaultUID           string          `json:"vault_uid"`
	ProfileFingerprint string          `json:"profile_fingerprint"`
	State              string          `json:"state"`
	Renditions         CoverageClass   `json:"renditions"`
	Embeddings         []CoverageClass `json:"embeddings"`
}

type DocumentSearchMode string

const (
	DocumentSearchAuto     DocumentSearchMode = "auto"
	DocumentSearchLexical  DocumentSearchMode = "lexical"
	DocumentSearchSemantic DocumentSearchMode = "semantic"
	DocumentSearchHybrid   DocumentSearchMode = "hybrid"
)

type DocumentSearchRequest struct {
	Query     string              `json:"query"`
	Mode      DocumentSearchMode  `json:"mode"`
	Limit     int                 `json:"limit,omitzero"`
	Profile   string              `json:"profile"`
	BindingID string              `json:"binding_id,omitzero"`
	Fence     DocumentSourceFence `json:"fence"`
	Explain   bool                `json:"explain,omitzero"`
}

type DocumentEvidenceReference struct {
	Kind                   string `json:"kind"`
	BuildID                string `json:"build_id,omitzero"`
	SegmentID              string `json:"segment_id,omitzero"`
	VectorSpaceID          string `json:"vector_space_id,omitzero"`
	EmbeddingSetID         string `json:"embedding_set_id,omitzero"`
	InputGenerationID      string `json:"input_generation_id,omitzero"`
	InputID                string `json:"input_id,omitzero"`
	InputKind              string `json:"input_kind,omitzero"`
	SourceManifestChecksum string `json:"source_manifest_checksum,omitzero"`
}

type DocumentSearchTrace struct {
	Code  string `json:"code"`
	Count int    `json:"count"`
}

type DocumentSearchResult struct {
	VaultUID         string                      `json:"vault_uid"`
	NodeID           int64                       `json:"node_id"`
	ContentVersionID string                      `json:"content_version_id"`
	Rank             int                         `json:"rank"`
	Score            float64                     `json:"score"`
	Path             string                      `json:"path"`
	Excerpt          string                      `json:"excerpt,omitzero"`
	LexicalRank      int                         `json:"lexical_rank,omitzero"`
	SemanticRank     int                         `json:"semantic_rank,omitzero"`
	Evidence         []DocumentEvidenceReference `json:"evidence"`
}

type DocumentSearchCoverage struct {
	BindingRequired   bool   `json:"binding_required"`
	ScopedDocuments   int    `json:"scoped_documents"`
	CompleteDocuments int    `json:"complete_documents"`
	State             string `json:"state"`
}

type DocumentSearchReport struct {
	RequestedMode DocumentSearchMode     `json:"requested_mode"`
	ActualMode    DocumentSearchMode     `json:"actual_mode"`
	Coverage      DocumentSearchCoverage `json:"coverage"`
	Degradations  []string               `json:"degradations"`
	Results       []DocumentSearchResult `json:"results"`
	Truncated     bool                   `json:"truncated"`
	Trace         []DocumentSearchTrace  `json:"trace"`
}

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
	CurrentVersionID string  `json:"current_version_id,omitzero"`
	BlobHash         string  `json:"blob_hash,omitzero"`
	Size             int64   `json:"size"`
	MediaType        string  `json:"media_type,omitzero"`
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
	MediaType             string  `json:"media_type,omitzero"`
	RecordedAt            string  `json:"recorded_at"`
	NodeRevision          int64   `json:"node_revision"`
	IntroducedOperationID string  `json:"introduced_operation_id"`
	TransitionKind        string  `json:"transition_kind"`
	SourceVersionID       *string `json:"source_version_id,omitempty"`
}

// ProvenanceSource describes an application-neutral origin for one immutable
// document creation. Reference may be a URI, archive key, filesystem path, or
// another stable source-local identifier; Docbank does not interpret it.
type ProvenanceSource struct {
	Kind        string     `json:"kind"`
	Description string     `json:"description"`
	Reference   string     `json:"reference"`
	ModifiedAt  *time.Time `json:"modified_at,omitempty"`
}

// ProvenanceFact is one immutable statement about where a document came from.
// Superseded facts remain visible; Active marks the current unsuperseded facts.
type ProvenanceFact struct {
	Identity          string  `json:"identity"`
	NodeID            int64   `json:"node_id"`
	IngestID          string  `json:"ingest_id"`
	RecordedAt        string  `json:"recorded_at"`
	SourceKind        string  `json:"source_kind"`
	SourceDescription string  `json:"source_description"`
	SourceReference   string  `json:"source_reference"`
	SourceModifiedAt  *string `json:"source_modified_at,omitempty"`
	Supersedes        *string `json:"supersedes,omitempty"`
	Active            bool    `json:"active"`
}

const (
	DefaultProvenanceLimit = 100
	MaxProvenanceLimit     = store.MaxProvenancePageSize
)

// ProvenanceOptions selects one bounded newest-first provenance page.
type ProvenanceOptions struct {
	Limit  int
	Offset int
}

// ProvenancePage binds origin history to a transactionally consistent node.
// Path is empty when the node is in trash.
type ProvenancePage struct {
	Node   Node             `json:"node"`
	Path   string           `json:"path,omitzero"`
	Items  []ProvenanceFact `json:"items"`
	Total  int              `json:"total"`
	Limit  int              `json:"limit"`
	Offset int              `json:"offset"`
}

// PutReceipt proves the computed identity and resulting logical authority.
type PutReceipt struct {
	Node            Node            `json:"node"`
	Version         ContentVersion  `json:"version"`
	Computed        ContentIdentity `json:"computed"`
	Physical        PhysicalContent `json:"physical"`
	Created         bool            `json:"created"`
	PhysicalCreated bool            `json:"physical_created"`
	Replaced        bool            `json:"replaced"`
}

// RepairReceipt proves the replacement bytes and reports the resulting
// physical authority without changing any logical node or version.
type RepairReceipt struct {
	Computed            ContentIdentity `json:"computed"`
	Physical            PhysicalContent `json:"physical"`
	ReferencesPreserved int64           `json:"references_preserved"`
}

// RevisionOptions applies an optional lost-update guard to one tree mutation.
// Zero is unconditional; a positive value must match the node's revision.
type RevisionOptions struct {
	IfRevision int64 `json:"if_revision,omitzero"`
}

// MutationReceipt binds the resulting node projection to the canonical path
// captured in the same metadata transaction.
type MutationReceipt struct {
	Node Node   `json:"node"`
	Path string `json:"path"`
}

// BatchMoveItem identifies one live source either by SourcePath or by stable
// NodeID plus IfRevision. DestinationPath is an exact final coordinate
// whose parent is resolved in the batch's planned final tree.
type BatchMoveItem struct {
	SourcePath      string `json:"source_path,omitzero"`
	NodeID          int64  `json:"node_id,omitzero"`
	IfRevision      int64  `json:"if_revision,omitzero"`
	DestinationPath string `json:"destination_path"`
}

// BatchMoveReceipt binds one request to its stable node and transactional
// pre/post coordinates.
type BatchMoveReceipt struct {
	Node     Node   `json:"node"`
	FromPath string `json:"from_path"`
	Path     string `json:"path"`
}

// MaxBatchMoves is the largest all-or-nothing reorganization accepted by one
// embedded or daemon operation.
const MaxBatchMoves = store.MaxBatchMoves

// TrashEmptyOptions bounds one trash-empty preview or execution. A zero
// MaxRoots uses DefaultTrashEmptyMaxRoots. DryRun never deletes candidates.
type TrashEmptyOptions struct {
	OlderThan time.Duration `json:"older_than"`
	MaxRoots  int           `json:"max_roots,omitzero"`
	DryRun    bool          `json:"dry_run"`
}

// TrashEmptyReport summarizes one bounded batch of eligible trash roots.
type TrashEmptyReport struct {
	Candidates int64 `json:"candidates"`
	Deleted    int64 `json:"deleted"`
	More       bool  `json:"more"`
	DryRun     bool  `json:"dry_run"`
}

// PhysicalContent describes the representation with current catalog
// authority. Logical identity is always SHA-256 over decoded bytes.
type PhysicalContent struct {
	Kind         string `json:"kind"`
	Encoding     string `json:"encoding"`
	LogicalBytes int64  `json:"logical_bytes"`
	StoredBytes  int64  `json:"stored_bytes"`
	PackEligible bool   `json:"pack_eligible"`
}

// LooseBacklog summarizes loose content eligible for explicit packing.
type LooseBacklog struct {
	EligibleObjects     int64 `json:"eligible_objects"`
	EligibleBytes       int64 `json:"eligible_bytes"`
	EligibleStoredBytes int64 `json:"eligible_stored_bytes"`
	RawObjects          int64 `json:"raw_objects"`
	CompressedObjects   int64 `json:"compressed_objects"`
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

const (
	DefaultVersionsLimit = 100
	MaxVersionsLimit     = 1000
)

type VersionsOptions struct {
	Limit  int
	Offset int
}

type VersionsPage struct {
	Items  []ContentVersion `json:"items"`
	Total  int              `json:"total"`
	Limit  int              `json:"limit"`
	Offset int              `json:"offset"`
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
	More                       bool  `json:"more"`
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

// VersionContent binds a verified byte stream to one immutable content version.
type VersionContent struct {
	Version ContentVersion
	Reader  VerifiedReadCloser
}

// ContentRangeOptions selects a non-empty decoded logical byte range.
type ContentRangeOptions struct {
	Offset int64
	Length int64
}

// VersionContentRange binds a bounded logical byte stream to one immutable
// content version. Reader must be closed to release its vault lease.
type VersionContentRange struct {
	Version ContentVersion
	Offset  int64
	Length  int64
	Reader  io.ReadCloser
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

func fromStoreProvenance(fact store.ProvenanceFact) ProvenanceFact {
	return ProvenanceFact{
		Identity: fact.Identity, NodeID: fact.NodeID, IngestID: fact.IngestID,
		RecordedAt: fact.IngestStartedAt, SourceKind: fact.SourceKind,
		SourceDescription: fact.SourceDescription, SourceReference: fact.OriginalPath,
		SourceModifiedAt: fact.OriginalMTime, Supersedes: fact.Supersedes,
		Active: fact.Active,
	}
}

func fromStorePhysical(physical store.PhysicalContent) PhysicalContent {
	return PhysicalContent{
		Kind: physical.Kind, Encoding: physical.Encoding,
		LogicalBytes: physical.LogicalBytes, StoredBytes: physical.StoredBytes,
		PackEligible: physical.PackEligible,
	}
}

func fromStoreLooseBacklog(backlog store.LooseBacklog) LooseBacklog {
	return LooseBacklog{
		EligibleObjects: backlog.EligibleObjects, EligibleBytes: backlog.EligibleBytes,
		EligibleStoredBytes: backlog.EligibleStoredBytes,
		RawObjects:          backlog.RawObjects, CompressedObjects: backlog.CompressedObjects,
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
