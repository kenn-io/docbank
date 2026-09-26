package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"fmt"
)

const (
	metadataDocumentIdentityType        = "document_identity"
	metadataDocumentIdentityAliasType   = "document_identity_alias"
	metadataAdoptedPassageAuthorityType = "adopted_passage_authority"
)

type metadataDocumentIdentity struct {
	Type        string `json:"type"`
	DocumentUID string `json:"document_uid"`
	NodeID      int64  `json:"node_id"`
	CreatedAt   string `json:"created_at"`
}

type metadataDocumentIdentityAlias struct {
	Type              string `json:"type"`
	DomainUID         string `json:"domain_uid"`
	SourceVaultUID    string `json:"source_vault_uid"`
	SourceDocumentUID string `json:"source_document_uid"`
	LocalDocumentUID  string `json:"local_document_uid"`
	MappedAt          string `json:"mapped_at"`
}

type metadataAdoptedPassageAuthority struct {
	Type                   string `json:"type"`
	DomainUID              string `json:"domain_uid"`
	SourceVaultUID         string `json:"source_vault_uid"`
	SourceDocumentUID      string `json:"source_document_uid"`
	SourceContentVersionID string `json:"source_content_version_id"`
	SourceBuildID          string `json:"source_build_id"`
	SourceAttachmentID     string `json:"source_attachment_id"`
	LocalDocumentUID       string `json:"local_document_uid"`
	LocalContentVersionID  string `json:"local_content_version_id"`
	LocalBuildID           string `json:"local_build_id"`
	LocalAttachmentID      string `json:"local_attachment_id"`
	MappedAt               string `json:"mapped_at"`
}

func exportDocumentIdentityMetadata(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	rows, err := q.QueryContext(ctx, `SELECT document_uid,node_id,created_at FROM document_identities ORDER BY document_uid`)
	if err != nil {
		return fmt.Errorf("exporting document identities: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		r := metadataDocumentIdentity{Type: metadataDocumentIdentityType}
		if err := rows.Scan(&r.DocumentUID, &r.NodeID, &r.CreatedAt); err != nil {
			return err
		}
		if err := validateMetadataDocumentIdentity(r); err != nil {
			return err
		}
		if err := write(r); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	aliasRows, err := q.QueryContext(ctx, `SELECT domain_uid,source_vault_uid,source_document_uid,local_document_uid,mapped_at FROM document_identity_aliases ORDER BY domain_uid,source_vault_uid,source_document_uid`)
	if err != nil {
		return fmt.Errorf("exporting document identity aliases: %w", err)
	}
	defer func() { _ = aliasRows.Close() }()
	for aliasRows.Next() {
		r := metadataDocumentIdentityAlias{Type: metadataDocumentIdentityAliasType}
		if err := aliasRows.Scan(&r.DomainUID, &r.SourceVaultUID, &r.SourceDocumentUID, &r.LocalDocumentUID, &r.MappedAt); err != nil {
			return err
		}
		if err := validateMetadataDocumentIdentityAlias(r); err != nil {
			return err
		}
		if err := write(r); err != nil {
			return err
		}
	}
	return aliasRows.Err()
}

func validateMetadataDocumentIdentity(r metadataDocumentIdentity) error {
	if r.Type != metadataDocumentIdentityType || validateUUIDv4(r.DocumentUID) != nil || r.NodeID < 1 {
		return errors.New("invalid document identity metadata")
	}
	return validateMetadataTime("document identity created_at", r.CreatedAt)
}

func validateMetadataDocumentIdentityAlias(r metadataDocumentIdentityAlias) error {
	if r.Type != metadataDocumentIdentityAliasType || validateUUIDv4(r.DomainUID) != nil || validateUUIDv4(r.SourceVaultUID) != nil || validateUUIDv4(r.SourceDocumentUID) != nil || validateUUIDv4(r.LocalDocumentUID) != nil {
		return errors.New("invalid document identity alias metadata")
	}
	return validateMetadataTime("document identity alias mapped_at", r.MappedAt)
}

func exportAdoptedPassageAuthorityMetadata(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	rows, err := q.QueryContext(ctx, `SELECT domain_uid,source_vault_uid,source_document_uid,
		source_content_version_id,source_build_id,source_attachment_id,local_document_uid,
		local_content_version_id,local_build_id,local_attachment_id,mapped_at
		FROM adopted_passage_authorities ORDER BY domain_uid,source_vault_uid,source_document_uid,
		source_content_version_id,source_build_id,source_attachment_id`)
	if err != nil {
		return fmt.Errorf("exporting adopted passage authorities: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		r := metadataAdoptedPassageAuthority{Type: metadataAdoptedPassageAuthorityType}
		if err := rows.Scan(&r.DomainUID, &r.SourceVaultUID, &r.SourceDocumentUID,
			&r.SourceContentVersionID, &r.SourceBuildID, &r.SourceAttachmentID,
			&r.LocalDocumentUID, &r.LocalContentVersionID, &r.LocalBuildID,
			&r.LocalAttachmentID, &r.MappedAt); err != nil {
			return err
		}
		if err := validateMetadataAdoptedPassageAuthority(r); err != nil {
			return err
		}
		if err := write(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

func validateMetadataAdoptedPassageAuthority(r metadataAdoptedPassageAuthority) error {
	if r.Type != metadataAdoptedPassageAuthorityType ||
		validateUUIDv4(r.DomainUID) != nil || validateUUIDv4(r.SourceVaultUID) != nil ||
		validateUUIDv4(r.SourceDocumentUID) != nil || validateUUIDv4(r.SourceContentVersionID) != nil ||
		validateUUIDv4(r.LocalDocumentUID) != nil || validateUUIDv4(r.LocalContentVersionID) != nil ||
		validateCatalogSHA256(r.SourceBuildID, "origin build ID") != nil ||
		validateCatalogSHA256(r.SourceAttachmentID, "origin attachment ID") != nil ||
		validateCatalogSHA256(r.LocalBuildID, "local build ID") != nil ||
		validateCatalogSHA256(r.LocalAttachmentID, "local attachment ID") != nil {
		return errors.New("invalid adopted passage authority metadata")
	}
	return validateMetadataTime("adopted passage authority mapped_at", r.MappedAt)
}

func importDocumentIdentityMetadataRecord(ctx context.Context, tx *sql.Tx, kind string, raw jsontext.Value) error {
	switch kind {
	case metadataDocumentIdentityType:
		var r metadataDocumentIdentity
		if err := decodeMetadataRecord(raw, &r); err != nil {
			return err
		}
		if err := validateMetadataDocumentIdentity(r); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO document_identities(document_uid,node_id,created_at) VALUES(?,?,?)`, r.DocumentUID, r.NodeID, r.CreatedAt)
		return err
	case metadataDocumentIdentityAliasType:
		var r metadataDocumentIdentityAlias
		if err := decodeMetadataRecord(raw, &r); err != nil {
			return err
		}
		if err := validateMetadataDocumentIdentityAlias(r); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO document_identity_aliases(domain_uid,source_vault_uid,source_document_uid,local_document_uid,mapped_at) VALUES(?,?,?,?,?)`, r.DomainUID, r.SourceVaultUID, r.SourceDocumentUID, r.LocalDocumentUID, r.MappedAt)
		return err
	case metadataAdoptedPassageAuthorityType:
		var r metadataAdoptedPassageAuthority
		if err := decodeMetadataRecord(raw, &r); err != nil {
			return err
		}
		if err := validateMetadataAdoptedPassageAuthority(r); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO adopted_passage_authorities(
			domain_uid,source_vault_uid,source_document_uid,source_content_version_id,
			source_build_id,source_attachment_id,local_document_uid,local_content_version_id,
			local_build_id,local_attachment_id,mapped_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			r.DomainUID, r.SourceVaultUID, r.SourceDocumentUID, r.SourceContentVersionID,
			r.SourceBuildID, r.SourceAttachmentID, r.LocalDocumentUID, r.LocalContentVersionID,
			r.LocalBuildID, r.LocalAttachmentID, r.MappedAt)
		return err
	default:
		return fmt.Errorf("unsupported document identity metadata type %q", kind)
	}
}
