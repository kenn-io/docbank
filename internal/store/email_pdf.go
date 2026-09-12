package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/docbank/document"
)

type EmailPDFReceipt = document.EmailPDFReceiptV1

// EmailPDFReceipts discovers retained PDFs even when their renderer is absent
// or has changed. Every entry is revalidated against its exact source.
func (s *Store) EmailPDFReceipts(ctx context.Context, versionID string) ([]EmailPDFReceipt, error) {
	if _, err := s.ContentVersionByID(ctx, versionID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT a.profile_fingerprint FROM rendition_attachments a JOIN rendition_artifacts r ON r.build_id=a.build_id WHERE a.content_version_id=? AND r.role=? ORDER BY a.profile_fingerprint`, versionID, string(document.EvidenceArtifactPDF))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var profiles []string
	for rows.Next() {
		var profile string
		if err := rows.Scan(&profile); err != nil {
			_ = rows.Close()
			return nil, err
		}
		profiles = append(profiles, profile)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	out := make([]EmailPDFReceipt, 0, len(profiles))
	for _, profile := range profiles {
		r, err := s.EmailPDFReceipt(ctx, versionID, profile)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func emailPDFBinding(p ProcessingProfileRecord) (*document.EmailPDFBindingV1, error) {
	var profile document.ProcessingProfileV1
	if err := json.Unmarshal(p.CanonicalProfile, &profile, json.RejectUnknownMembers(true)); err != nil {
		return nil, err
	}
	if profile.Rendition == nil {
		return nil, nil //nolint:nilnil // Profiles without this optional typed binding are not PDF renditions.
	}
	return profile.Rendition.EmailPDF, nil
}

func validateEmailPDFBuild(b RenditionBuildRecord) (*document.EmailPDFOutputV1, error) {
	var pdf *RenditionArtifactRecord
	for i := range b.Artifacts {
		if b.Artifacts[i].Role == string(document.EvidenceArtifactPDF) {
			if pdf != nil {
				return nil, ErrEmailCorrupt
			}
			pdf = &b.Artifacts[i]
		}
	}
	if pdf == nil {
		return nil, nil //nolint:nilnil // Other rendition types deliberately have no PDF output.
	}
	var receipt document.RenditionReceipt
	if err := json.Unmarshal(b.ProviderReceipt, &receipt, json.RejectUnknownMembers(true)); err != nil {
		return nil, ErrEmailCorrupt
	}
	o := receipt.EmailPDF
	if receipt.ProviderID != document.EmailPDFContract || receipt.SourceSHA256 != b.SourceSHA256 || receipt.RenditionRequestFingerprint != b.RenditionRequestFingerprint || o == nil || o.PDFSHA256 != pdf.BlobHash || o.PDFSize != pdf.Size || o.PDFSize < 1 || o.PDFSize > 256<<20 || o.Pages < 1 || o.Pages > 1000 || o.BodySize < 1 || o.BodySize > 16<<20 || b.Completeness != document.EvidenceDegradedProvenance || b.PartialSuccess || b.Truncated {
		return nil, ErrEmailCorrupt
	}
	if err := document.ValidateEmailPartPath(o.BodyPath); err != nil {
		return nil, ErrEmailCorrupt
	}
	if err := validateCatalogSHA256(o.BodySHA256, "email PDF body hash"); err != nil {
		return nil, err
	}
	return o, nil
}

func validateEmailPDFPublication(ctx context.Context, q metadataQuerier, a RenditionAttachmentRecord, b RenditionBuildRecord) (*EmailPDFReceipt, error) {
	binding, err := emailPDFBinding(a.Profile)
	if err != nil {
		return nil, err
	}
	o, err := validateEmailPDFBuild(b)
	if err != nil {
		return nil, err
	}
	if binding == nil {
		if o != nil {
			return nil, ErrEmailCorrupt
		}
		return nil, nil //nolint:nilnil // Non-PDF rendition attachments intentionally have no typed PDF receipt.
	}
	if o == nil {
		return nil, ErrEmailCorrupt
	}
	v, err := emailVersion(ctx, q, a.ContentVersionID)
	if err != nil {
		return nil, err
	}
	e, err := emailMetadataView(ctx, q, v, binding.GenerationID)
	if err != nil {
		return nil, err
	}
	path, body := emailSelectedBody(e.Evidence)
	if path == nil || body == nil || *path != o.BodyPath || body.SHA256 != o.BodySHA256 || body.Size != o.BodySize || e.Generation.Checksum != binding.GenerationChecksum || v.BlobHash != b.SourceSHA256 {
		return nil, ErrEmailCorrupt
	}
	return &EmailPDFReceipt{Source: document.EmailDocumentIdentity{NodeID: v.NodeID, VersionID: v.ID, SHA256: v.BlobHash, Size: v.Size}, AttachmentID: a.ID, BuildID: b.ID, ProfileFingerprint: a.Profile.Fingerprint, Binding: *binding, Output: *o}, nil
}

// EmailPDFReceipt resolves only an exact retained attachment. A PDF has no
// rendition serving head: it cannot replace body/FTS authority.
func (s *Store) EmailPDFReceipt(ctx context.Context, versionID, profileFingerprint string) (EmailPDFReceipt, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return EmailPDFReceipt{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT attachment_id FROM rendition_attachments WHERE content_version_id=? AND profile_fingerprint=? ORDER BY attached_at,attachment_id LIMIT 1`, versionID, profileFingerprint).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return EmailPDFReceipt{}, ErrNotFound
	}
	if err != nil {
		return EmailPDFReceipt{}, err
	}
	a, err := loadRenditionAttachment(ctx, tx, id)
	if err != nil {
		return EmailPDFReceipt{}, err
	}
	b, err := loadRenditionBuild(ctx, tx, a.BuildID)
	if err != nil {
		return EmailPDFReceipt{}, err
	}
	if err := validateRenditionArtifactRolesForProfile(a.Profile, b); err != nil {
		return EmailPDFReceipt{}, err
	}
	r, err := validateEmailPDFPublication(ctx, tx, a, b)
	if err != nil {
		return EmailPDFReceipt{}, err
	}
	if r == nil {
		return EmailPDFReceipt{}, ErrNotFound
	}
	return *r, nil
}
