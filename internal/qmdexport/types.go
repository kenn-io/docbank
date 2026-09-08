// Package qmdexport builds disposable QMD-compatible Markdown collections
// from catalog-authorized Docbank rendition artifacts.
package qmdexport

import (
	"context"

	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

const (
	ManifestFormatV1        = "docbank-qmd-export/v1"
	defaultMaxDocuments     = 100_000
	defaultMaxDocumentBytes = int64(64 << 20)
	defaultMaxTotalBytes    = int64(4 << 30)
	maxManifestBytes        = int64(256 << 20)
)

// Source is one catalog-authorized active sanitized-Markdown artifact.
type Source = store.QMDExportSource

// Entry maps one QMD URI back to exact Docbank authority.
type Entry struct {
	URI                          string `json:"uri"`
	RelativePath                 string `json:"relative_path"`
	VaultUID                     string `json:"vault_uid"`
	NodeID                       int64  `json:"node_id"`
	ContentVersionID             string `json:"content_version_id"`
	ProcessingProfileFingerprint string `json:"processing_profile_fingerprint"`
	AttachmentID                 string `json:"attachment_id"`
	BuildID                      string `json:"build_id"`
	ArtifactID                   string `json:"artifact_id"`
	BlobSHA256                   string `json:"blob_sha256"`
	BlobSize                     int64  `json:"blob_size"`
	ArtifactChecksum             string `json:"artifact_checksum"`
	MarkdownChecksum             string `json:"markdown_checksum"`
	ExportedMarkdownSHA256       string `json:"exported_markdown_sha256"`
	Frontmatter                  string `json:"frontmatter,omitempty"`
}

// Manifest is the deterministic identity map for one complete collection.
type Manifest struct {
	Format     string  `json:"format"`
	Collection string  `json:"collection"`
	Entries    []Entry `json:"entries"`
	Checksum   string  `json:"checksum"`
}

// Options bounds source discovery before any retained blob is opened.
type Options struct {
	MaxDocuments     int
	MaxDocumentBytes int64
	MaxTotalBytes    int64
	MaxManifestBytes int64
}

// Generation is a complete validated in-memory export ready for publication.
type Generation struct {
	ID        string
	Manifest  Manifest
	documents map[string][]byte
}

// BlobReader opens catalog-authorized loose or packed rendition bytes.
type BlobReader interface {
	OpenStreamContext(ctx context.Context, hash string) (packstore.VerifiedReadCloser, int64, error)
}

// SourceCatalog lists one bounded snapshot of active export authority.
type SourceCatalog interface {
	QMDExportSources(ctx context.Context, limit int) ([]Source, error)
}

// Receipt identifies the generation actually selected by CURRENT, including
// when a subsequent confirmation, cleanup, or release operation fails.
type Receipt struct {
	GenerationID   string
	CollectionPath string
	Manifest       Manifest
}

// CleanupStatus counts observed results; exhausted scans report lower bounds.
type CleanupStatus struct {
	RemovedGenerations  int
	PreservedCandidates int
	UnknownEntries      int
	ScanLimitReached    bool
}

// PostPublicationError means CURRENT was committed. Its fixed diagnostic never
// exposes filesystem names or nested causes; Unwrap preserves cause matching.
type PostPublicationError struct {
	Code    string
	Cleanup CleanupStatus
	cause   error
}

func (e *PostPublicationError) Error() string {
	return "qmd export post-publication operation incomplete"
}
func (e *PostPublicationError) Unwrap() error { return e.cause }
