package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrRenditionTextStale means one caller-supplied source, head, or generation
// identity no longer matches the exact catalog snapshot.
var ErrRenditionTextStale = errors.New("rendition text authority is stale")

// ErrRenditionTextUnavailable means a readable rendition has no retained,
// verified sanitized-text artifact.
var ErrRenditionTextUnavailable = errors.New("rendition text artifact is unavailable")

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

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return RenditionTextView{}, fmt.Errorf("starting rendition text snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	node, err := nodeByIDTx(tx, binding.NodeID)
	if err != nil {
		return RenditionTextView{}, err
	}
	if node.IsDir() || node.TrashedAt != nil {
		return RenditionTextView{}, ErrNotFound
	}
	version, err := scanContentVersion(tx.QueryRowContext(ctx,
		`SELECT `+contentVersionCols+` FROM content_versions WHERE node_id=? AND version_id=?`,
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
		WHERE content_version_id=? AND profile_fingerprint=?`,
		binding.ContentVersionID, binding.ProfileFingerprint,
	).Scan(&rendition.Head.AttachmentID, &rendition.Head.PublishedAt)
	if errors.Is(err, sql.ErrNoRows) {
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
			SELECT 1 FROM rendition_lexical_generation_builds WHERE generation_id=? AND build_id=?
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
		// Existing publication rejects normalized evidence without readable
		// text. Keep the wire state available for imported/future compatible
		// authorities, but never let omitted lexical rows hide Markdown.
		const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
		if view.Artifact.Size != 0 || view.Artifact.BlobHash != emptySHA256 {
			return RenditionTextView{}, ErrRenditionTextUnavailable
		}
		view.Empty = true
	}
	if err := tx.Commit(); err != nil {
		return RenditionTextView{}, fmt.Errorf("closing rendition text snapshot: %w", err)
	}
	return view, nil
}
