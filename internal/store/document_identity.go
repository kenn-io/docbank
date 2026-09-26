package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/docbank/document"
)

var (
	// ErrDocumentIdentityUnavailable means the persistent identity authority
	// has not been installed or does not contain the requested mapping.
	ErrDocumentIdentityUnavailable = errors.New("document identity is unavailable")
	// ErrPassageAuthorityUnavailable means an exact historical tuple is absent
	// or no longer retained. Callers must never replace it with a current head.
	ErrPassageAuthorityUnavailable = errors.New("passage authority is unavailable")
)

// DocumentIdentity binds one stable public UID to one vault-local node.
type DocumentIdentity struct {
	DocumentUID string
	NodeID      int64
}

// PassageAuthority is the complete retained catalog tuple needed before any
// passage bytes may be opened.
type PassageAuthority struct {
	Identity   DocumentIdentity
	Node       Node
	Path       string
	Version    ContentVersion
	Attachment RenditionAttachmentRecord
	Build      RenditionBuildRecord
	Artifact   RenditionArtifactRecord
	Fresh      bool
}

type adoptedPassageTuple struct {
	DocumentUID  string
	VersionID    string
	BuildID      string
	AttachmentID string
}

// EnsureDocumentIdentity returns a node's stable standalone identity,
// allocating it exactly once. The central schema owns the backing tables.
func (s *Store) EnsureDocumentIdentity(ctx context.Context, nodeID int64) (DocumentIdentity, error) {
	var identity DocumentIdentity
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		node, err := nodeByIDTx(tx, nodeID)
		if err != nil || node.IsDir() || node.TrashedAt != nil {
			return ErrDocumentIdentityUnavailable
		}
		if err := tx.QueryRowContext(ctx,
			`SELECT document_uid,node_id FROM document_identities WHERE node_id=?`, nodeID,
		).Scan(&identity.DocumentUID, &identity.NodeID); err == nil {
			return nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return documentIdentitySchemaError(err)
		}
		uid, err := newUUIDv4()
		if err != nil {
			return fmt.Errorf("allocating document identity: %w", err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO document_identities(
			document_uid,node_id,created_at) VALUES(?,?,?)`, uid, nodeID, nowRFC3339()); err != nil {
			return documentIdentitySchemaError(err)
		}
		if err = tx.QueryRowContext(ctx,
			`SELECT document_uid,node_id FROM document_identities WHERE node_id=?`, nodeID,
		).Scan(&identity.DocumentUID, &identity.NodeID); err != nil {
			return documentIdentitySchemaError(err)
		}
		return nil
	})
	if err != nil {
		return DocumentIdentity{}, err
	}
	return identity, nil
}

// PutDocumentIdentityAlias maps an adopted federation identity to an existing
// standalone identity. Repeating an exact mapping is idempotent; retargeting
// an existing alias fails.
func (s *Store) PutDocumentIdentityAlias(
	ctx context.Context, domainUID, sourceVaultUID, sourceDocumentUID, localDocumentUID string,
) error {
	for name, value := range map[string]string{
		"domain UID": domainUID, "source vault UID": sourceVaultUID,
		"source document UID": sourceDocumentUID, "local document UID": localDocumentUID,
	} {
		if validateUUIDv4(value) != nil {
			return fmt.Errorf("document identity alias %s is invalid", name)
		}
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM document_identities WHERE document_uid=?)`, localDocumentUID).Scan(&exists); err != nil {
			return documentIdentitySchemaError(err)
		}
		if !exists {
			return ErrDocumentIdentityUnavailable
		}
		result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO document_identity_aliases(
			domain_uid,source_vault_uid,source_document_uid,local_document_uid,mapped_at
		) VALUES(?,?,?,?,?)`, domainUID, sourceVaultUID, sourceDocumentUID, localDocumentUID, nowRFC3339())
		if err != nil {
			return documentIdentitySchemaError(err)
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if inserted == 1 {
			return nil
		}
		var stored string
		if err := tx.QueryRowContext(ctx, `SELECT local_document_uid
			FROM document_identity_aliases WHERE domain_uid=? AND source_vault_uid=? AND source_document_uid=?`,
			domainUID, sourceVaultUID, sourceDocumentUID).Scan(&stored); err != nil {
			return documentIdentitySchemaError(err)
		}
		if stored != localDocumentUID {
			return errors.New("document identity alias already maps to another document")
		}
		return nil
	})
}

// PutAdoptedPassageAuthority records an explicit, immutable adoption of one
// origin rendition tuple. It does not create an enrollment or grant a caller
// access to a document; the corresponding document alias must already exist.
func (s *Store) PutAdoptedPassageAuthority(ctx context.Context, domainUID string,
	ref document.PassageRefV1, localDocumentUID, localVersionID, localBuildID, localAttachmentID string,
) error {
	if validateUUIDv4(domainUID) != nil || validateUUIDv4(localDocumentUID) != nil ||
		validateUUIDv4(localVersionID) != nil || document.ValidatePassageIdentityV1(ref) != nil ||
		ref.VaultUID == s.vaultID ||
		(ref.FederationDomainUID != "" && ref.FederationDomainUID != domainUID) ||
		validateCatalogSHA256(localBuildID, "local rendition build ID") != nil ||
		validateCatalogSHA256(localAttachmentID, "local rendition attachment ID") != nil {
		return ErrPassageAuthorityUnavailable
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var exists bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM document_identity_aliases da
			JOIN document_identities di ON di.document_uid=da.local_document_uid
			JOIN nodes n ON n.id=di.node_id AND n.kind='file' AND n.trashed_at IS NULL
			JOIN content_versions v ON v.node_id=n.id
			JOIN rendition_attachments a ON a.content_version_id=v.version_id
			JOIN rendition_builds b ON b.build_id=a.build_id AND b.vault_uid=a.vault_uid
			JOIN rendition_artifacts artifact ON artifact.build_id=b.build_id
			WHERE da.domain_uid=? AND da.source_vault_uid=? AND da.source_document_uid=?
			AND da.local_document_uid=? AND v.version_id=? AND v.blob_hash=?
			AND a.attachment_id=? AND a.vault_uid=? AND b.build_id=? AND b.source_sha256=?
			AND artifact.role=? AND artifact.blob_hash=b.markdown_checksum
			AND artifact.checksum=artifact.blob_hash
		)`, domainUID, ref.VaultUID, ref.DocumentUID, localDocumentUID,
			localVersionID, ref.SourceSHA256, localAttachmentID, s.vaultID, localBuildID,
			ref.SourceSHA256, catalogArtifactSanitizedMarkdown).Scan(&exists)
		if err != nil {
			return passageAuthorityError(err)
		}
		if !exists {
			return ErrPassageAuthorityUnavailable
		}
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO adopted_passage_authorities(
			domain_uid,source_vault_uid,source_document_uid,source_content_version_id,
			source_build_id,source_attachment_id,local_document_uid,local_content_version_id,
			local_build_id,local_attachment_id,mapped_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			domainUID, ref.VaultUID, ref.DocumentUID, ref.ContentVersionID, ref.RenditionBuildID,
			ref.AttachmentID, localDocumentUID, localVersionID, localBuildID, localAttachmentID, nowRFC3339())
		if err != nil {
			return passageAuthorityError(err)
		}
		var stored adoptedPassageTuple
		err = tx.QueryRowContext(ctx, `SELECT local_document_uid,local_content_version_id,
			local_build_id,local_attachment_id FROM adopted_passage_authorities
			WHERE domain_uid=? AND source_vault_uid=? AND source_document_uid=?
			AND source_content_version_id=? AND source_build_id=? AND source_attachment_id=?`,
			domainUID, ref.VaultUID, ref.DocumentUID, ref.ContentVersionID,
			ref.RenditionBuildID, ref.AttachmentID).Scan(&stored.DocumentUID,
			&stored.VersionID, &stored.BuildID, &stored.AttachmentID)
		if err != nil {
			return passageAuthorityError(err)
		}
		if stored != (adoptedPassageTuple{localDocumentUID, localVersionID, localBuildID, localAttachmentID}) {
			return ErrPassageAuthorityUnavailable
		}
		return nil
	})
}

// ResolvePassageAuthority resolves only the exact retained tuple named by ref.
// It deliberately ignores rendition heads and a node's current version.
func (s *Store) ResolvePassageAuthority(
	ctx context.Context, ref document.PassageRefV1,
) (PassageAuthority, error) {
	if err := document.ValidatePassageAddressV1(ref); err != nil {
		return PassageAuthority{}, ErrPassageAuthorityUnavailable
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PassageAuthority{}, fmt.Errorf("starting passage authority snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var result PassageAuthority
	target := adoptedPassageTuple{VersionID: ref.ContentVersionID, BuildID: ref.RenditionBuildID,
		AttachmentID: ref.AttachmentID}
	if ref.FederationDomainUID == "" && ref.VaultUID == s.vaultID {
		err = tx.QueryRowContext(ctx, `SELECT document_uid,node_id FROM document_identities
			WHERE document_uid=?`, ref.DocumentUID).
			Scan(&result.Identity.DocumentUID, &result.Identity.NodeID)
	} else {
		target, err = adoptedPassageAuthorityTx(ctx, tx, ref)
		if errors.Is(err, sql.ErrNoRows) {
			return PassageAuthority{}, ErrPassageAuthorityUnavailable
		}
		if err != nil {
			return PassageAuthority{}, passageAuthorityError(err)
		}
		err = tx.QueryRowContext(ctx, `SELECT i.document_uid,i.node_id FROM document_identities i
			WHERE i.document_uid=?`, target.DocumentUID).
			Scan(&result.Identity.DocumentUID, &result.Identity.NodeID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return PassageAuthority{}, ErrPassageAuthorityUnavailable
	}
	if err != nil {
		return PassageAuthority{}, documentIdentitySchemaError(err)
	}
	result.Node, err = nodeByIDTx(tx, result.Identity.NodeID)
	if err != nil || result.Node.IsDir() || result.Node.TrashedAt != nil {
		return PassageAuthority{}, ErrPassageAuthorityUnavailable
	}
	if err := document.ValidatePassageIdentityV1(ref); err != nil {
		return PassageAuthority{}, ErrPassageAuthorityUnavailable
	}
	result.Path, err = pathOf(ctx, tx, result.Node.ID)
	if err != nil {
		return PassageAuthority{}, passageAuthorityError(err)
	}
	result.Version, err = scanContentVersion(tx.QueryRowContext(ctx, `SELECT `+contentVersionCols+`
		FROM content_versions WHERE version_id=? AND node_id=?`, target.VersionID, result.Node.ID))
	if err != nil || result.Version.BlobHash != ref.SourceSHA256 {
		return PassageAuthority{}, ErrPassageAuthorityUnavailable
	}
	result.Attachment, err = loadRenditionAttachment(ctx, tx, target.AttachmentID)
	if err != nil || result.Attachment.VaultID != s.vaultID ||
		result.Attachment.ContentVersionID != target.VersionID ||
		result.Attachment.BuildID != target.BuildID {
		return PassageAuthority{}, ErrPassageAuthorityUnavailable
	}
	result.Build, err = loadRenditionBuild(ctx, tx, target.BuildID)
	if err != nil || result.Build.VaultID != s.vaultID ||
		result.Build.VaultID != result.Attachment.VaultID ||
		result.Build.SourceSHA256 != ref.SourceSHA256 {
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
	if result.Node.CurrentVersionID == target.VersionID {
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM rendition_heads
			WHERE content_version_id=? AND attachment_id=?)`,
			target.VersionID, target.AttachmentID).Scan(&result.Fresh); err != nil {
			return PassageAuthority{}, passageAuthorityError(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return PassageAuthority{}, fmt.Errorf("closing passage authority snapshot: %w", err)
	}
	return result, nil
}

func adoptedPassageAuthorityTx(ctx context.Context, tx *sql.Tx,
	ref document.PassageRefV1,
) (adoptedPassageTuple, error) {
	query := `SELECT pa.local_document_uid,pa.local_content_version_id,pa.local_build_id,
		pa.local_attachment_id FROM adopted_passage_authorities pa
		JOIN document_identity_aliases da ON da.domain_uid=pa.domain_uid
		AND da.source_vault_uid=pa.source_vault_uid AND da.source_document_uid=pa.source_document_uid
		AND da.local_document_uid=pa.local_document_uid
		WHERE pa.source_vault_uid=? AND pa.source_document_uid=?
		AND pa.source_content_version_id=? AND pa.source_build_id=?
		AND pa.source_attachment_id=?`
	args := []any{ref.VaultUID, ref.DocumentUID, ref.ContentVersionID,
		ref.RenditionBuildID, ref.AttachmentID}
	if ref.FederationDomainUID != "" {
		query += ` AND pa.domain_uid=?`
		args = append(args, ref.FederationDomainUID)
	}
	query += ` LIMIT 2`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return adoptedPassageTuple{}, err
	}
	defer func() { _ = rows.Close() }()
	var target adoptedPassageTuple
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return adoptedPassageTuple{}, err
		}
		return adoptedPassageTuple{}, sql.ErrNoRows
	}
	if err := rows.Scan(&target.DocumentUID, &target.VersionID,
		&target.BuildID, &target.AttachmentID); err != nil {
		return adoptedPassageTuple{}, err
	}
	if rows.Next() {
		return adoptedPassageTuple{}, ErrPassageAuthorityUnavailable
	}
	return target, rows.Err()
}

func documentIdentitySchemaError(err error) error {
	return fmt.Errorf("%w: persistent document identity schema: %w", ErrDocumentIdentityUnavailable, err)
}

func passageAuthorityError(err error) error {
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound) {
		return ErrPassageAuthorityUnavailable
	}
	return fmt.Errorf("reading passage authority: %w", err)
}
