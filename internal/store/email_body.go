package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"reflect"
	"slices"

	"go.kenn.io/docbank/document"
)

func emailBodyProfileRecord(recipe document.EmailRecipeV1) (ProcessingProfileRecord, error) {
	profile, err := document.EmailBodyProfileV1(recipe)
	if err != nil {
		return ProcessingProfileRecord{}, err
	}
	canonical, fp, err := document.CanonicalProfile(profile)
	if err != nil {
		return ProcessingProfileRecord{}, err
	}
	return ProcessingProfileRecord{Fingerprint: fp.Profile, CanonicalProfile: jsontext.Value(canonical), RenditionRequestFingerprint: fp.RenditionRequest, EvidenceLexicalFingerprint: fp.EvidenceLexical, RetentionDisclosureFingerprint: fp.RetentionDisclosure, AttachmentPolicyFingerprint: profile.RetentionDisclosure.AttachmentPolicyFingerprint, ConsentFingerprint: profile.RetentionDisclosure.ConsentFingerprint, RenditionDisclosureFingerprint: profile.Rendition.DisclosureFingerprint, TrustBoundary: profile.RetentionDisclosure.TrustBoundary}, nil
}
func emailSelectedBody(v document.EmailV1) (*string, *document.EmailArtifactRefV1) {
	if v.Inventory == nil {
		return nil, nil
	}
	for _, m := range v.Inventory.Messages {
		if m.Path != "1" || m.SelectedBodyPath == nil {
			continue
		}
		for _, p := range v.Inventory.Parts {
			if p.Path == *m.SelectedBodyPath {
				return m.SelectedBodyPath, p.BodyUTF8
			}
		}
	}
	return nil, nil
}
func loadEmailBodyResult(ctx context.Context, q metadataQuerier, id string) (metadataEmailBodyResult, error) {
	var r metadataEmailBodyResult
	r.Type = "email_body_result"
	err := q.QueryRowContext(ctx, `SELECT email_attachment_id,body_recipe_fingerprint,state,part_path,rendition_attachment_id,reason FROM email_body_results WHERE email_attachment_id=?`, id).Scan(&r.EmailAttachmentID, &r.BodyRecipeFingerprint, &r.State, &r.PartPath, &r.RenditionAttachmentID, &r.Reason)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}
func emailBodySuppressed(ctx context.Context, q metadataQuerier, v EmailMetadataView) (bool, error) {
	profile, err := emailBodyProfileRecord(v.Evidence.Recipe)
	if err != nil {
		return false, err
	}
	return emailBodyProfileSuppressed(ctx, q, v.Version, profile.Fingerprint)
}

func emailBodyProfileSuppressed(
	ctx context.Context, q metadataQuerier, version ContentVersion, profileFingerprint string,
) (bool, error) {
	var yes bool
	// The version/profile fence survives deleting the body association and build.
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM derivative_purge_suppressions WHERE source_sha256=? AND profile_fingerprint=? AND active=1)`, version.BlobHash, derivativeAttachmentSuppressionScope(version.ID, profileFingerprint)).Scan(&yes)
	return yes, err
}
func validateEmailBodyShape(v EmailMetadataView, r metadataEmailBodyResult) error {
	recipe, err := document.EmailBodyRecipeFingerprint(v.Evidence.Recipe)
	if err != nil {
		return err
	}
	if r.EmailAttachmentID != v.Attachment.ID || r.BodyRecipeFingerprint != recipe {
		return fmt.Errorf("%w: body attachment or recipe mismatch", ErrEmailCorrupt)
	}
	selected, body := emailSelectedBody(v.Evidence)
	switch r.State {
	case "available":
		if selected == nil || body == nil || r.PartPath == nil || *r.PartPath != *selected || r.RenditionAttachmentID == nil || r.Reason != nil {
			return fmt.Errorf("%w: available body selection", ErrEmailCorrupt)
		}
	case "unavailable":
		if r.RenditionAttachmentID != nil || r.Reason == nil {
			return fmt.Errorf("%w: unavailable body shape", ErrEmailCorrupt)
		}
		switch *r.Reason {
		case "inventory_unavailable":
			if v.Evidence.Inventory != nil || r.PartPath != nil {
				return fmt.Errorf("%w: inventory unavailable implication", ErrEmailCorrupt)
			}
		case "no_supported_body":
			if v.Evidence.Inventory == nil || selected != nil || r.PartPath != nil {
				return fmt.Errorf("%w: absent body implication", ErrEmailCorrupt)
			}
		case "empty_body", "body_text_limit", "body_render_limit", "unsupported_body":
			if selected == nil || body == nil || r.PartPath == nil || *r.PartPath != *selected {
				return fmt.Errorf("%w: unavailable selected body", ErrEmailCorrupt)
			}
		default:
			return fmt.Errorf("%w: unknown body reason", ErrEmailCorrupt)
		}
	default:
		return fmt.Errorf("%w: unknown body state", ErrEmailCorrupt)
	}
	return nil
}
func validateEmailBodyResult(ctx context.Context, q metadataQuerier, v EmailMetadataView) error {
	r, err := loadEmailBodyResult(ctx, q, v.Attachment.ID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := validateEmailBodyShape(v, r); err != nil {
		return err
	}
	if r.State == "available" {
		a, err := loadRenditionAttachment(ctx, q, *r.RenditionAttachmentID)
		if err != nil {
			return err
		}
		profile, err := emailBodyProfileRecord(v.Evidence.Recipe)
		if err != nil {
			return err
		}
		if a.ContentVersionID != v.Version.ID || !reflect.DeepEqual(a.Profile, profile) {
			return fmt.Errorf("%w: body rendition source/profile", ErrEmailCorrupt)
		}
		build, err := loadRenditionBuild(ctx, q, a.BuildID)
		if err != nil {
			return err
		}
		if err := validateEmailBodyBuild(v, build); err != nil {
			return err
		}
	}
	return nil
}
func emailBodySearch(ctx context.Context, q metadataQuerier, v EmailMetadataView) (EmailBodySearch, error) {
	unavailable := func(reason string) EmailBodySearch { return EmailBodySearch{State: "unavailable", Reason: new(reason)} }
	if err := validateEmailBodyResult(ctx, q, v); err != nil {
		return EmailBodySearch{}, err
	}
	suppressed, err := emailBodySuppressed(ctx, q, v)
	if err != nil {
		return EmailBodySearch{}, err
	}
	if suppressed {
		return unavailable("derivative_purged"), nil
	}
	r, err := loadEmailBodyResult(ctx, q, v.Attachment.ID)
	if errors.Is(err, ErrNotFound) {
		if v.Evidence.Inventory == nil {
			return unavailable("inventory_unavailable"), nil
		}
		return EmailBodySearch{State: "pending"}, nil
	}
	if err != nil {
		return EmailBodySearch{}, err
	}
	if r.State == "unavailable" {
		return unavailable(*r.Reason), nil
	}
	var buildID string
	var serving bool
	err = q.QueryRowContext(ctx, `SELECT a.build_id,EXISTS(SELECT 1 FROM rendition_heads h JOIN email_heads eh ON eh.content_version_id=h.content_version_id JOIN rendition_lexical_generation_builds gb ON gb.build_id=a.build_id JOIN rendition_lexical_heads lh ON lh.generation_id=gb.generation_id WHERE h.attachment_id=a.attachment_id AND eh.attachment_id=?) FROM rendition_attachments a WHERE a.attachment_id=?`, v.Attachment.ID, *r.RenditionAttachmentID).Scan(&buildID, &serving)
	if err != nil {
		return EmailBodySearch{}, err
	}
	if !serving {
		return unavailable("superseded"), nil
	}
	return EmailBodySearch{State: "available", RenditionBuildID: new(buildID), RenditionAttachmentID: r.RenditionAttachmentID}, nil
}
func selectedEmailAttachment(ctx context.Context, q metadataQuerier, id string) (EmailMetadataView, error) {
	a, err := loadEmailAttachment(ctx, q, id)
	if err != nil {
		return EmailMetadataView{}, err
	}
	v, err := emailVersion(ctx, q, a.ContentVersionID)
	if err != nil {
		return EmailMetadataView{}, err
	}
	view, err := emailMetadataView(ctx, q, v, "")
	if err != nil {
		return view, err
	}
	if view.Attachment.ID != id {
		return view, fmt.Errorf("%w: email attachment is no longer selected", ErrEmailCorrupt)
	}
	suppressed, err := emailBodySuppressed(ctx, q, view)
	if err != nil {
		return view, err
	}
	if suppressed {
		return view, ErrEmailDerivativeSuppressed
	}
	return view, nil
}
func insertEmailBodyResult(ctx context.Context, tx *sql.Tx, r metadataEmailBodyResult) error {
	old, err := loadEmailBodyResult(ctx, tx, r.EmailAttachmentID)
	if err == nil {
		if !reflect.DeepEqual(old, r) {
			return fmt.Errorf("%w: conflicting permanent body result", ErrEmailCorrupt)
		}
		return nil
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO email_body_results(email_attachment_id,body_recipe_fingerprint,state,part_path,rendition_attachment_id,reason) VALUES(?,?,?,?,?,?)`, r.EmailAttachmentID, r.BodyRecipeFingerprint, r.State, r.PartPath, r.RenditionAttachmentID, r.Reason)
	return err
}
func (s *Store) RecordEmailBodyUnavailable(ctx context.Context, emailAttachmentID, bodyRecipeFingerprint string, partPath *string, reason string) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		v, err := selectedEmailAttachment(ctx, tx, emailAttachmentID)
		if err != nil {
			return err
		}
		r := metadataEmailBodyResult{Type: "email_body_result", EmailAttachmentID: emailAttachmentID, BodyRecipeFingerprint: bodyRecipeFingerprint, State: "unavailable", PartPath: partPath, Reason: new(reason)}
		if err := validateEmailBodyShape(v, r); err != nil {
			return err
		}
		return insertEmailBodyResult(ctx, tx, r)
	})
}

// validateEmailBodyBuild binds the local receipt to independently loaded MIME
// and rendition authority. A well-formed receipt alone grants no association.
func validateEmailBodyBuild(v EmailMetadataView, build RenditionBuildRecord) error {
	receipt, _, err := document.DecodeEmailBodyReceiptV1(build.ProviderReceipt)
	if err != nil {
		return fmt.Errorf("%w: body receipt: %w", ErrEmailCorrupt, err)
	}
	path, body := emailSelectedBody(v.Evidence)
	recipe, err := document.EmailBodyRecipeFingerprint(v.Evidence.Recipe)
	if err != nil {
		return err
	}
	if path == nil || body == nil || receipt.SourceSHA256 != v.Version.BlobHash || receipt.SourceSize != v.Version.Size || build.SourceSHA256 != v.Version.BlobHash || receipt.EmailGenerationID != v.Generation.ID || receipt.EmailChecksum != v.Generation.Checksum || receipt.PartPath != *path || receipt.BodySHA256 != body.SHA256 || receipt.BodySize != body.Size || receipt.BodyRecipeFingerprint != recipe || receipt.EvidenceChecksum != build.EvidenceChecksum || receipt.RenditionChecksum != build.RenditionChecksum || receipt.MarkdownChecksum != build.MarkdownChecksum {
		return fmt.Errorf("%w: body receipt differs from selected MIME or rendition authority", ErrEmailCorrupt)
	}
	id, err := document.EmailBodyBuildID(receipt)
	if err != nil {
		return err
	}
	operation, err := document.EmailBodyOperationID(recipe)
	if err != nil {
		return err
	}
	authorization, err := document.EmailBodyAuthorizationChecksum(v.Version.BlobHash, v.Version.Size, recipe)
	if err != nil {
		return err
	}
	if id != build.ID || operation != build.ProviderOperationID || authorization != build.AuthorizationChecksum {
		return fmt.Errorf("%w: body build operation identity", ErrEmailCorrupt)
	}
	if build.Truncated || build.PartialSuccess || !slices.Equal(build.Warnings, []string{"degraded_provenance"}) || build.Completeness != document.EvidenceDegradedProvenance {
		return fmt.Errorf("%w: body rendition must be complete derived text", ErrEmailCorrupt)
	}
	roles := map[string]string{"normalized_evidence": build.EvidenceChecksum, "sanitized_markdown": build.MarkdownChecksum}
	if len(build.Artifacts) != len(roles) {
		return fmt.Errorf("%w: body rendition artifact set", ErrEmailCorrupt)
	}
	for _, a := range build.Artifacts {
		hash, ok := roles[a.Role]
		if !ok || a.Checksum != hash || a.BlobHash != hash || a.Size < 1 || a.State != RenditionArtifactVerified {
			return fmt.Errorf("%w: body rendition artifact identity", ErrEmailCorrupt)
		}
		delete(roles, a.Role)
	}
	return nil
}

// PublishEmailBody uses the ordinary rendition publisher's transaction so the
// exact email association, selected rendition, and complete lexical projection
// either become visible together or remain unchanged.
func (s *Store) PublishEmailBody(ctx context.Context, p EmailBodyPublication) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		v, err := selectedEmailAttachment(ctx, tx, p.EmailAttachmentID)
		if err != nil {
			return err
		}
		a, err := normalizeRenditionAttachmentRecord(p.RenditionAttachment)
		if err != nil {
			return err
		}
		profile, err := emailBodyProfileRecord(v.Evidence.Recipe)
		if err != nil {
			return err
		}
		if a.VaultID != s.VaultID() || a.ContentVersionID != v.Version.ID || !reflect.DeepEqual(a.Profile, profile) {
			return fmt.Errorf("%w: body rendition attachment source/profile", ErrEmailCorrupt)
		}
		r := metadataEmailBodyResult{Type: "email_body_result", EmailAttachmentID: p.EmailAttachmentID, BodyRecipeFingerprint: p.BodyRecipeFingerprint, State: "available", PartPath: new(p.PartPath), RenditionAttachmentID: new(a.ID)}
		if err := validateEmailBodyShape(v, r); err != nil {
			return err
		}
		build, err := loadRenditionBuild(ctx, tx, a.BuildID)
		if err != nil {
			return err
		}
		if err := validateEmailBodyBuild(v, build); err != nil {
			return err
		}
		prior, err := loadEmailBodyResult(ctx, tx, p.EmailAttachmentID)
		if err == nil && !reflect.DeepEqual(prior, r) {
			return fmt.Errorf("%w: conflicting permanent body result", ErrEmailCorrupt)
		}
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if err := s.publishRenditionAttachmentsAndLexicalHeadsTx(ctx, tx, []renditionPublicationPair{{attachment: a, head: p.RenditionHead}}, p.LexicalGenerationID, nil); err != nil {
			return err
		}
		return insertEmailBodyResult(ctx, tx, r)
	})
}
