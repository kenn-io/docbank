package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"mime"

	"go.kenn.io/docbank/document"
)

// ErrRenditionTextStale means one caller-supplied source, head, or generation
// identity no longer matches the exact catalog snapshot.
var ErrRenditionTextStale = errors.New("rendition text authority is stale")

// ErrRenditionTextUnavailable means a readable rendition has no retained,
// verified sanitized-text artifact.
var ErrRenditionTextUnavailable = errors.New("rendition text artifact is unavailable")

// ErrRenditionTextFailed means the latest attempt for the exact version and
// profile failed without a retained rendition head.
var ErrRenditionTextFailed = errors.New("rendition text processing failed")

// RenditionTextBinding fences a text read to one live node, retained source
// version, active rendition head, and optional accepted lexical generation.
type RenditionTextBinding struct {
	NodeID             int64
	NodeRevision       int64
	ContentVersionID   string
	SourceSHA256       string
	SourceSize         int64
	ProfileFingerprint string
	GenerationID       string
	AttachmentID       string
	BuildID            string
}

// RenditionTextView is the exact verified artifact selected in one read
// transaction. Empty is a published zero-segment outcome, not a missing blob.
type RenditionTextView struct {
	Node      Node
	Version   ContentVersion
	Rendition RenditionView
	Artifact  *RenditionArtifactRecord
	Empty     bool
}

// ResolveRenditionText revalidates all supplied source and derivative
// identities together. It never substitutes a current version, new head, or
// different generation for the caller's accepted selection.
func (s *Store) ResolveRenditionText(
	ctx context.Context, binding RenditionTextBinding,
) (RenditionTextView, error) {
	if binding.NodeID < 1 || binding.NodeRevision < 1 || binding.SourceSize < 0 ||
		validateUUIDv4(binding.ContentVersionID) != nil ||
		validateCatalogSHA256(binding.SourceSHA256, "rendition source") != nil ||
		validateCatalogSHA256(binding.ProfileFingerprint, "rendition profile") != nil {
		return RenditionTextView{}, ErrRenditionTextStale
	}
	for _, expected := range []struct{ value, name string }{
		{binding.GenerationID, "rendition generation"},
		{binding.AttachmentID, "rendition attachment"},
		{binding.BuildID, "rendition build"},
	} {
		if expected.value != "" && validateCatalogSHA256(expected.value, expected.name) != nil {
			return RenditionTextView{}, ErrRenditionTextStale
		}
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return RenditionTextView{}, fmt.Errorf("starting rendition text snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	node, err := nodeByIDTx(ctx, tx, binding.NodeID)
	if err != nil {
		return RenditionTextView{}, err
	}
	if node.IsDir() || node.TrashedAt != nil {
		return RenditionTextView{}, ErrNotFound
	}
	version, err := scanContentVersion(tx.QueryRowContext(ctx,
		`SELECT `+contentVersionCols+` FROM content_versions WHERE node_id=$1 AND version_id=$2`,
		binding.NodeID, binding.ContentVersionID))
	if err != nil {
		return RenditionTextView{}, err
	}
	if node.Revision != binding.NodeRevision || version.BlobHash != binding.SourceSHA256 ||
		version.Size != binding.SourceSize {
		return RenditionTextView{}, ErrRenditionTextStale
	}

	rendition := RenditionView{Head: RenditionHeadRecord{
		ContentVersionID:             binding.ContentVersionID,
		ProcessingProfileFingerprint: binding.ProfileFingerprint,
	}}
	err = tx.QueryRowContext(ctx, `SELECT attachment_id,published_at FROM rendition_heads
		WHERE content_version_id=$1 AND profile_fingerprint=$2`,
		binding.ContentVersionID, binding.ProfileFingerprint,
	).Scan(&rendition.Head.AttachmentID, &rendition.Head.PublishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		var failed bool
		dialect := metadataJSONDialect{s.postgresSchema != ""}
		err = tx.QueryRowContext(ctx, `SELECT w.state='rejected' OR j.state IN ('failed','operator_required')
			FROM rendition_job_waiters w JOIN rendition_jobs j ON j.job_id=w.job_id
			WHERE w.content_version_id=$1 AND w.profile_fingerprint=$2 AND NOT EXISTS(SELECT 1 FROM `+dialect.arrayValues("w.input_classes_json")+` WHERE value='derived_upload')
			ORDER BY `+dialect.maximum()+`(w.updated_at,j.updated_at) DESC,w.updated_at DESC,j.updated_at DESC,
			 w.waiter_id DESC,j.job_id DESC LIMIT 1`,
			binding.ContentVersionID, binding.ProfileFingerprint).Scan(&failed)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return RenditionTextView{}, fmt.Errorf("reading rendition text attempt: %w", err)
		}
		if failed {
			return RenditionTextView{}, ErrRenditionTextFailed
		}
		return RenditionTextView{}, ErrNotFound
	}
	if err != nil {
		return RenditionTextView{}, fmt.Errorf("reading rendition text head: %w", err)
	}
	rendition.Attachment, err = loadRenditionAttachment(ctx, tx, rendition.Head.AttachmentID)
	if err != nil {
		return RenditionTextView{}, fmt.Errorf("reading rendition text attachment: %w", err)
	}
	rendition.Build, err = loadRenditionBuild(ctx, tx, rendition.Attachment.BuildID)
	if err != nil {
		return RenditionTextView{}, fmt.Errorf("reading rendition text build: %w", err)
	}
	if rendition.Attachment.ContentVersionID != version.ID ||
		rendition.Attachment.Profile.Fingerprint != binding.ProfileFingerprint ||
		rendition.Build.SourceSHA256 != version.BlobHash ||
		binding.AttachmentID != "" && binding.AttachmentID != rendition.Attachment.ID ||
		binding.BuildID != "" && binding.BuildID != rendition.Build.ID {
		return RenditionTextView{}, ErrRenditionTextStale
	}
	if binding.GenerationID != "" {
		var contained bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM rendition_lexical_generation_builds WHERE generation_id=$1 AND build_id=$2
		)`, binding.GenerationID, rendition.Build.ID).Scan(&contained); err != nil {
			return RenditionTextView{}, fmt.Errorf("checking rendition text generation: %w", err)
		}
		if !contained {
			return RenditionTextView{}, ErrRenditionTextStale
		}
	}

	view := RenditionTextView{Node: node, Version: version, Rendition: rendition}
	for index := range rendition.Build.Artifacts {
		artifact := &rendition.Build.Artifacts[index]
		if artifact.Role != catalogArtifactSanitizedMarkdown {
			continue
		}
		if artifact.State != RenditionArtifactVerified || artifact.BlobHash != artifact.Checksum ||
			artifact.BlobHash != rendition.Build.MarkdownChecksum {
			return RenditionTextView{}, ErrRenditionTextUnavailable
		}
		artifactCopy := *artifact
		view.Artifact = &artifactCopy
		break
	}
	if view.Artifact == nil {
		return RenditionTextView{}, ErrRenditionTextUnavailable
	}
	if len(rendition.Build.LexicalSegments) == 0 {
		const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
		valid := view.Artifact.Size == 0 && view.Artifact.BlobHash == emptySHA256
		mediaType, _, _ := mime.ParseMediaType(version.MimeType)
		if rendition.Build.Completeness == document.EvidencePartial && len(rendition.Build.Units) == 0 && mediaType == "application/pdf" {
			empty, _, err := document.EnvelopeRenditionV1(document.RenditionV1{Completeness: document.EvidencePartial, MarkdownChecksum: emptySHA256}, document.RenditionEnvelopeV1{
				BuildID: rendition.Build.ID, SourceSHA256: version.BlobHash, SourceFormat: "pdf", SourceMediaType: "application/pdf",
				RenditionRequestFingerprint: rendition.Build.RenditionRequestFingerprint, EvidenceLexicalFingerprint: rendition.Build.EvidenceLexicalFingerprint,
				NormalizedEvidenceContract: document.NormalizedEvidenceContractV1, UnitKind: document.EvidenceUnitPage,
			})
			valid = err == nil && view.Artifact.Size == int64(len(empty.Markdown)) && view.Artifact.BlobHash == empty.MarkdownChecksum
		}
		if !valid {
			return RenditionTextView{}, ErrRenditionTextUnavailable
		}
		view.Empty = true
	}
	if err := tx.Commit(); err != nil {
		return RenditionTextView{}, fmt.Errorf("closing rendition text snapshot: %w", err)
	}
	return view, nil
}
