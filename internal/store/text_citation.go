package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/docbank/document"
)

// RetainedTextCitation checks a published historical attachment in one snapshot.
// The caller must hold physical capture protection until it finishes reading.
func (s *Store) RetainedTextCitation(
	ctx context.Context, citation document.TextCitation,
) (RenditionArtifactRecord, error) {
	if err := document.ValidateTextCitation(citation); err != nil {
		return RenditionArtifactRecord{}, err
	}
	if citation.VaultUID.String() != s.vaultID {
		return RenditionArtifactRecord{}, document.ErrCitationUnavailable
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return RenditionArtifactRecord{}, fmt.Errorf("starting citation snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	artifact, err := retainedTextCitation(ctx, tx, citation)
	if errors.Is(err, ErrNotFound) {
		return RenditionArtifactRecord{}, document.ErrCitationUnavailable
	}
	if err != nil {
		return RenditionArtifactRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return RenditionArtifactRecord{}, fmt.Errorf("closing citation snapshot: %w", err)
	}
	return artifact, nil
}

func retainedTextCitation(
	ctx context.Context, tx *sql.Tx, citation document.TextCitation,
) (RenditionArtifactRecord, error) {
	node, err := nodeByIDQuery(ctx, tx, citation.NodeID)
	if err != nil {
		return RenditionArtifactRecord{}, err
	}
	if node.Kind != nodeKindFile || node.TrashedAt != nil {
		return RenditionArtifactRecord{}, document.ErrCitationUnavailable
	}
	version, err := scanContentVersion(tx.QueryRowContext(ctx,
		`SELECT `+contentVersionCols+` FROM content_versions WHERE version_id=? AND node_id=?`,
		citation.ContentVersionID.String(), citation.NodeID))
	if err != nil {
		return RenditionArtifactRecord{}, err
	}
	attachment, err := loadRenditionAttachment(ctx, tx, citation.RenditionAttachmentID)
	if err != nil {
		return RenditionArtifactRecord{}, err
	}
	if version.BlobHash != citation.ContentSHA256 || attachment.ContentVersionID != version.ID ||
		attachment.VaultID != citation.VaultUID.String() || attachment.BuildID != citation.BuildID {
		return RenditionArtifactRecord{}, document.ErrCitationUnavailable
	}
	suppressed, err := derivativeAttachmentSuppressedTx(ctx, tx, version.BlobHash,
		version.ID, attachment.Profile.Fingerprint, attachment.BuildID)
	if err != nil {
		return RenditionArtifactRecord{}, err
	}
	if suppressed {
		return RenditionArtifactRecord{}, document.ErrCitationUnavailable
	}
	build, err := loadRenditionBuildView(ctx, tx, attachment.BuildID, true)
	if err != nil {
		return RenditionArtifactRecord{}, err
	}
	if build.VaultID != attachment.VaultID || build.SourceSHA256 != version.BlobHash ||
		build.RenditionRequestFingerprint != attachment.Profile.RenditionRequestFingerprint ||
		build.EvidenceLexicalFingerprint != attachment.Profile.EvidenceLexicalFingerprint ||
		build.MarkdownChecksum != citation.RenditionSHA256 {
		return RenditionArtifactRecord{}, document.ErrCitationUnavailable
	}
	if err := validateRenditionArtifactRolesForProfile(attachment.Profile, build); err != nil {
		return RenditionArtifactRecord{}, err
	}
	var result RenditionArtifactRecord
	for _, artifact := range build.Artifacts {
		if artifact.Role != catalogArtifactSanitizedMarkdown {
			continue
		}
		if result.ID != "" || artifact.BlobHash != citation.RenditionSHA256 ||
			artifact.Checksum != artifact.BlobHash || artifact.Size < 0 {
			return RenditionArtifactRecord{}, document.ErrCitationUnavailable
		}
		result = artifact
	}
	if result.ID == "" {
		return RenditionArtifactRecord{}, document.ErrCitationUnavailable
	}
	return result, nil
}
