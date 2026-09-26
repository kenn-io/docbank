package store

import (
	"context"
	"database/sql"
	"fmt"
)

// PassageIdentityClaim is the verified tuple presented after the caller has
// checked the artifact bytes. The final catalog and media-visibility fence is
// checked in the same write transaction that allocates its document UID.
type PassageIdentityClaim struct {
	NodeID           int64
	ContentVersionID string
	RenditionBuildID string
	AttachmentID     string
	ArtifactHash     string
	InputID          string
	Principal        string
}

// PassageCreationAuthority finds a retained local tuple without requiring or
// allocating a standalone document UID. A caller must verify its bytes before
// calling EnsureDocumentIdentity.
func (s *Store) PassageCreationAuthority(ctx context.Context, nodeID int64,
	versionID, buildID, attachmentID string,
) (PassageAuthority, error) {
	if nodeID < 1 || validateUUIDv4(versionID) != nil ||
		validateCatalogSHA256(buildID, "rendition build ID") != nil ||
		validateCatalogSHA256(attachmentID, "rendition attachment ID") != nil {
		return PassageAuthority{}, ErrPassageAuthorityUnavailable
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PassageAuthority{}, fmt.Errorf("starting passage creation snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var result PassageAuthority
	result.Node, err = nodeByIDTx(tx, nodeID)
	if err != nil || result.Node.IsDir() || result.Node.TrashedAt != nil {
		return PassageAuthority{}, ErrPassageAuthorityUnavailable
	}
	result.Path, err = pathOf(ctx, tx, nodeID)
	if err != nil {
		return PassageAuthority{}, passageAuthorityError(err)
	}
	result.Version, err = scanContentVersion(tx.QueryRowContext(ctx, `SELECT `+contentVersionCols+`
		FROM content_versions WHERE version_id=? AND node_id=?`, versionID, nodeID))
	if err != nil {
		return PassageAuthority{}, ErrPassageAuthorityUnavailable
	}
	result.Attachment, err = loadRenditionAttachment(ctx, tx, attachmentID)
	if err != nil || result.Attachment.VaultID != s.vaultID ||
		result.Attachment.ContentVersionID != versionID || result.Attachment.BuildID != buildID {
		return PassageAuthority{}, ErrPassageAuthorityUnavailable
	}
	result.Build, err = loadRenditionBuild(ctx, tx, buildID)
	if err != nil || result.Build.VaultID != s.vaultID ||
		result.Build.SourceSHA256 != result.Version.BlobHash {
		return PassageAuthority{}, ErrPassageAuthorityUnavailable
	}
	for _, artifact := range result.Build.Artifacts {
		if artifact.Role == catalogArtifactSanitizedMarkdown &&
			artifact.State == RenditionArtifactVerified && artifact.BlobHash == artifact.Checksum &&
			artifact.BlobHash == result.Build.MarkdownChecksum {
			result.Artifact = artifact
			break
		}
	}
	if result.Artifact.ID == "" {
		return PassageAuthority{}, ErrPassageAuthorityUnavailable
	}
	if result.Node.CurrentVersionID == versionID {
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM rendition_heads
			WHERE content_version_id=? AND attachment_id=?)`, versionID, attachmentID).Scan(&result.Fresh); err != nil {
			return PassageAuthority{}, passageAuthorityError(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return PassageAuthority{}, fmt.Errorf("closing passage creation snapshot: %w", err)
	}
	return result, nil
}

// EnsurePassageDocumentIdentity allocates only while the exact verified tuple
// and any selected media input are still visible. It does not read content.
func (s *Store) EnsurePassageDocumentIdentity(ctx context.Context,
	claim PassageIdentityClaim,
) (DocumentIdentity, error) {
	if claim.NodeID < 1 || validateUUIDv4(claim.ContentVersionID) != nil ||
		validateCatalogSHA256(claim.RenditionBuildID, "rendition build ID") != nil ||
		validateCatalogSHA256(claim.AttachmentID, "rendition attachment ID") != nil ||
		validateCatalogSHA256(claim.ArtifactHash, "artifact hash") != nil {
		return DocumentIdentity{}, ErrPassageAuthorityUnavailable
	}
	var identity DocumentIdentity
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var admitted bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM nodes n
			JOIN content_versions v ON v.node_id=n.id
			JOIN rendition_attachments a ON a.content_version_id=v.version_id
			JOIN rendition_builds b ON b.build_id=a.build_id AND b.vault_uid=a.vault_uid
			JOIN rendition_artifacts artifact ON artifact.build_id=b.build_id
			WHERE n.id=? AND n.kind='file' AND n.trashed_at IS NULL
			AND v.version_id=? AND a.attachment_id=? AND a.vault_uid=?
			AND b.build_id=? AND b.source_sha256=v.blob_hash
			AND artifact.role=? AND artifact.blob_hash=?
			AND artifact.checksum=artifact.blob_hash AND b.markdown_checksum=artifact.blob_hash
		)`, claim.NodeID, claim.ContentVersionID, claim.AttachmentID, s.vaultID,
			claim.RenditionBuildID, catalogArtifactSanitizedMarkdown,
			claim.ArtifactHash).Scan(&admitted)
		if err != nil {
			return passageAuthorityError(err)
		}
		if !admitted {
			return ErrPassageAuthorityUnavailable
		}
		if claim.InputID != "" {
			if claim.Principal == "" {
				return ErrPassageAuthorityUnavailable
			}
			err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM media_input_artifacts i
				JOIN media_occurrences o ON o.occurrence_id=i.occurrence_id
				JOIN media_source_versions sv ON sv.source_version_id=i.source_version_id
				WHERE i.input_id=? AND sv.content_version_id=?
				AND o.source_id=i.source_id AND o.source_version_id=i.source_version_id
				AND o.caller_principal=? AND o.visible=1
			)`, claim.InputID, claim.ContentVersionID, claim.Principal).Scan(&admitted)
			if err != nil {
				return passageAuthorityError(err)
			}
			if !admitted {
				return ErrPassageAuthorityUnavailable
			}
		}
		identity, err = ensureDocumentIdentityTx(ctx, tx, claim.NodeID)
		return err
	})
	if err != nil {
		return DocumentIdentity{}, err
	}
	return identity, nil
}
