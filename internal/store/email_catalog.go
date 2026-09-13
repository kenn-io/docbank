package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"mime"
	"slices"
	"strings"

	"go.kenn.io/docbank/document"
)

var (
	ErrEmailPending              = errors.New("email metadata is pending")
	ErrEmailNotSupported         = errors.New("email source is not supported")
	ErrEmailDerivativeSuppressed = errors.New("email derivative was purged")
	ErrEmailCorrupt              = errors.New("email metadata integrity check failed")
	ErrEmailPartUnavailable      = errors.New("email part is unavailable")
	ErrInvalidEmailPart          = errors.New("invalid email part selection")
)

type EmailPartArtifactRecord struct {
	PartPath   string
	Role       string
	BlobSHA256 string
	Size       int64
}
type EmailGenerationRecord struct {
	ID                string
	SourceSHA256      string
	SourceSize        int64
	RecipeFingerprint string
	CanonicalJSON     []byte
	Checksum          string
	CreatedAt         string
	Artifacts         []EmailPartArtifactRecord
}
type EmailAttachmentRecord struct {
	ID               string
	ContentVersionID string
	GenerationID     string
	AttachedAt       string
}
type EmailBodySearch struct {
	State                 string
	RenditionBuildID      *string
	RenditionAttachmentID *string
	Reason                *string
}
type EmailMetadataView struct {
	Version     ContentVersion
	Generation  EmailGenerationRecord
	Attachment  EmailAttachmentRecord
	PublishedAt string
	Evidence    document.EmailV1
	BodySearch  EmailBodySearch
}
type EmailTarget struct{ Version ContentVersion }
type EmailPartReceipt struct {
	Version      ContentVersion
	GenerationID string
	AttachmentID string
	PartPath     string
	Role         string
	Filename     string
	BlobSHA256   string
	Size         int64
}
type EmailPublication struct {
	ContentVersionID string
	CanonicalJSON    []byte
	Artifacts        []EmailPartArtifactRecord
}
type EmailBodyPublication struct {
	EmailAttachmentID     string
	PartPath              string
	BodyRecipeFingerprint string
	RenditionAttachment   RenditionAttachmentRecord
	RenditionHead         RenditionHeadRecord
	LexicalGenerationID   string
}

func emailArtifacts(e document.EmailV1) []EmailPartArtifactRecord {
	refs := []EmailPartArtifactRecord{}
	if e.Inventory != nil {
		for _, p := range e.Inventory.Parts {
			for _, r := range []*document.EmailArtifactRefV1{p.HeaderBlock, p.Payload, p.BodyUTF8} {
				if r != nil {
					refs = append(refs, EmailPartArtifactRecord{p.Path, string(r.Role), r.SHA256, r.Size})
				}
			}
		}
	}
	sortEmailArtifacts(refs)
	return refs
}
func sortEmailArtifacts(refs []EmailPartArtifactRecord) {
	slices.SortFunc(refs, func(a, b EmailPartArtifactRecord) int {
		if c := strings.Compare(a.PartPath, b.PartPath); c != 0 {
			return c
		}
		return strings.Compare(a.Role, b.Role)
	})
}

// validateEmailGeneration validates immutable bytes and their complete typed
// reference set. Reads, publication, and portable metadata share this gate.
func validateEmailGeneration(g EmailGenerationRecord) (document.EmailV1, error) {
	e, checksum, err := document.DecodeEmailV1(g.CanonicalJSON)
	if err != nil {
		return e, fmt.Errorf("%w: %w", ErrEmailCorrupt, err)
	}
	id, err := document.EmailGenerationID(e, checksum)
	if err != nil {
		return e, err
	}
	recipe, err := document.EmailRecipeFingerprint(e.Recipe)
	if err != nil {
		return e, err
	}
	if g.ID != id || g.Checksum != checksum || g.RecipeFingerprint != recipe || g.SourceSHA256 != e.Source.SHA256 || g.SourceSize != e.Source.Size {
		return e, fmt.Errorf("%w: generation identity differs from canonical evidence", ErrEmailCorrupt)
	}
	if err := validateMetadataTime("email created_at", g.CreatedAt); err != nil {
		return e, fmt.Errorf("%w: %w", ErrEmailCorrupt, err)
	}
	expected := emailArtifacts(e)
	if len(g.Artifacts) != len(expected) {
		return e, fmt.Errorf("%w: part reference count", ErrEmailCorrupt)
	}
	actual := slices.Clone(g.Artifacts)
	sortEmailArtifacts(actual)
	if !slices.Equal(actual, expected) {
		return e, fmt.Errorf("%w: part artifact references differ from canonical evidence", ErrEmailCorrupt)
	}
	return e, nil
}
func validateEmailBlobRefs(ctx context.Context, q metadataQuerier, g EmailGenerationRecord) error {
	for _, a := range g.Artifacts {
		var size int64
		err := q.QueryRowContext(ctx, `SELECT size FROM blobs WHERE hash=?`, a.BlobSHA256).Scan(&size)
		if err != nil {
			return fmt.Errorf("%w: part blob: %w", ErrEmailCorrupt, err)
		}
		if size != a.Size {
			return fmt.Errorf("%w: part blob size mismatch", ErrEmailCorrupt)
		}
	}
	return nil
}
func loadEmailGeneration(ctx context.Context, q metadataQuerier, id string) (EmailGenerationRecord, document.EmailV1, error) {
	var g EmailGenerationRecord
	err := q.QueryRowContext(ctx, `SELECT generation_id,source_sha256,source_size,recipe_fingerprint,canonical_json,checksum,created_at FROM email_generations WHERE generation_id=?`, id).Scan(&g.ID, &g.SourceSHA256, &g.SourceSize, &g.RecipeFingerprint, &g.CanonicalJSON, &g.Checksum, &g.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return g, document.EmailV1{}, ErrNotFound
	}
	if err != nil {
		return g, document.EmailV1{}, err
	}
	rows, err := q.QueryContext(ctx, `SELECT part_path,role,blob_hash,size FROM email_part_artifacts WHERE generation_id=? ORDER BY part_path,role`, id)
	if err != nil {
		return g, document.EmailV1{}, err
	}
	defer func() { _ = rows.Close() }()
	g.Artifacts = []EmailPartArtifactRecord{}
	for rows.Next() {
		var a EmailPartArtifactRecord
		if err = rows.Scan(&a.PartPath, &a.Role, &a.BlobSHA256, &a.Size); err != nil {
			break
		}
		g.Artifacts = append(g.Artifacts, a)
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		return g, document.EmailV1{}, err
	}
	e, err := validateEmailGeneration(g)
	if err != nil {
		return g, e, err
	}
	return g, e, validateEmailBlobRefs(ctx, q, g)
}
func loadEmailAttachment(ctx context.Context, q metadataQuerier, id string) (EmailAttachmentRecord, error) {
	var a EmailAttachmentRecord
	err := q.QueryRowContext(ctx, `SELECT attachment_id,content_version_id,generation_id,attached_at FROM email_attachments WHERE attachment_id=?`, id).Scan(&a.ID, &a.ContentVersionID, &a.GenerationID, &a.AttachedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	want, err := document.EmailAttachmentID(a.ContentVersionID, a.GenerationID)
	if err != nil || want != a.ID {
		return a, fmt.Errorf("%w: attachment identity", ErrEmailCorrupt)
	}
	return a, validateMetadataTime("email attached_at", a.AttachedAt)
}
func emailVersion(ctx context.Context, q metadataQuerier, id string) (ContentVersion, error) {
	return scanContentVersion(q.QueryRowContext(ctx, `SELECT `+contentVersionCols+` FROM content_versions WHERE version_id=?`, id))
}
func (s *Store) EmailGenerationForSource(ctx context.Context, sourceSHA256 string, sourceSize int64, recipeFingerprint string) (EmailGenerationRecord, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return EmailGenerationRecord{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT generation_id FROM email_generations WHERE source_sha256=? AND source_size=? AND recipe_fingerprint=?`, sourceSHA256, sourceSize, recipeFingerprint).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return EmailGenerationRecord{}, ErrNotFound
	}
	if err != nil {
		return EmailGenerationRecord{}, err
	}
	g, _, err := loadEmailGeneration(ctx, tx, id)
	return g, err
}
func (s *Store) PublishEmailGeneration(ctx context.Context, p EmailPublication) (EmailMetadataView, error) {
	e, checksum, err := document.DecodeEmailV1(p.CanonicalJSON)
	if err != nil {
		return EmailMetadataView{}, fmt.Errorf("%w: %w", ErrEmailCorrupt, err)
	}
	id, err := document.EmailGenerationID(e, checksum)
	if err != nil {
		return EmailMetadataView{}, err
	}
	recipe, err := document.EmailRecipeFingerprint(e.Recipe)
	if err != nil {
		return EmailMetadataView{}, err
	}
	if len(p.Artifacts) != len(emailArtifacts(e)) {
		return EmailMetadataView{}, fmt.Errorf("%w: part reference count", ErrEmailCorrupt)
	}
	g := EmailGenerationRecord{ID: id, SourceSHA256: e.Source.SHA256, SourceSize: e.Source.Size, RecipeFingerprint: recipe, CanonicalJSON: bytes.Clone(p.CanonicalJSON), Checksum: checksum, CreatedAt: nowRFC3339(), Artifacts: slices.Clone(p.Artifacts)}
	sortEmailArtifacts(g.Artifacts)
	if _, err = validateEmailGeneration(g); err != nil {
		return EmailMetadataView{}, err
	}
	var view EmailMetadataView
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		version, err := emailVersion(ctx, tx, p.ContentVersionID)
		if err != nil {
			return err
		}
		if version.BlobHash != g.SourceSHA256 || version.Size != g.SourceSize {
			return fmt.Errorf("%w: email source differs from retained version", ErrEmailCorrupt)
		}
		suppressed, err := emailInventorySuppressed(ctx, tx, version)
		if err != nil {
			return err
		}
		if suppressed {
			return ErrEmailDerivativeSuppressed
		}
		if err = validateEmailBlobRefs(ctx, tx, g); err != nil {
			return err
		}
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT generation_id FROM email_generations WHERE source_sha256=? AND source_size=? AND recipe_fingerprint=?`, g.SourceSHA256, g.SourceSize, g.RecipeFingerprint).Scan(&existing)
		switch {
		case err == nil:
			stored, _, err := loadEmailGeneration(ctx, tx, existing)
			if err != nil {
				return err
			}
			if stored.ID != g.ID || !bytes.Equal(stored.CanonicalJSON, g.CanonicalJSON) {
				return fmt.Errorf("%w: source and recipe already name different evidence", ErrEmailCorrupt)
			}
			g = stored
		case errors.Is(err, sql.ErrNoRows):
			if _, err = tx.ExecContext(ctx, `INSERT INTO email_generations(generation_id,source_sha256,source_size,recipe_fingerprint,canonical_json,checksum,created_at) VALUES(?,?,?,?,?,?,?)`, g.ID, g.SourceSHA256, g.SourceSize, g.RecipeFingerprint, g.CanonicalJSON, g.Checksum, g.CreatedAt); err != nil {
				return err
			}
			for _, a := range g.Artifacts {
				if _, err = tx.ExecContext(ctx, `INSERT INTO email_part_artifacts(generation_id,part_path,role,blob_hash,size) VALUES(?,?,?,?,?)`, g.ID, a.PartPath, a.Role, a.BlobSHA256, a.Size); err != nil {
					return err
				}
			}
		default:
			return err
		}
		attachmentID, err := document.EmailAttachmentID(version.ID, g.ID)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO email_attachments(attachment_id,content_version_id,generation_id,attached_at) VALUES(?,?,?,?) ON CONFLICT(content_version_id,generation_id) DO NOTHING`, attachmentID, version.ID, g.ID, nowRFC3339()); err != nil {
			return err
		}
		attachment, err := loadEmailAttachment(ctx, tx, attachmentID)
		if err != nil {
			return err
		}
		// Re-selecting the same inventory leaves its valid body and timestamps alone.
		if _, err = tx.ExecContext(ctx, `DELETE FROM rendition_heads WHERE attachment_id IN (
   SELECT b.rendition_attachment_id FROM email_body_results b JOIN email_attachments a ON a.attachment_id=b.email_attachment_id
   WHERE a.content_version_id=? AND a.attachment_id<>?) AND NOT EXISTS (SELECT 1 FROM email_heads WHERE content_version_id=? AND attachment_id=?)`, version.ID, attachment.ID, version.ID, attachment.ID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO email_heads(content_version_id,attachment_id,published_at) VALUES(?,?,?) ON CONFLICT(content_version_id) DO UPDATE SET attachment_id=excluded.attachment_id,published_at=excluded.published_at WHERE email_heads.attachment_id<>excluded.attachment_id`, version.ID, attachment.ID, attachment.AttachedAt); err != nil {
			return err
		}
		for _, a := range g.Artifacts {
			if _, err = tx.ExecContext(ctx, `DELETE FROM rendition_blob_staging WHERE blob_hash=?`, a.BlobSHA256); err != nil {
				return err
			}
		}
		view, err = emailMetadataView(ctx, tx, version, g.ID)
		return err
	})
	if err != nil {
		return EmailMetadataView{}, err
	}
	return view, nil
}
func (s *Store) EmailMetadata(ctx context.Context, versionID string) (EmailMetadataView, error) {
	return s.emailMetadata(ctx, versionID, "")
}
func (s *Store) EmailMetadataGeneration(ctx context.Context, versionID, generationID string) (EmailMetadataView, error) {
	if generationID == "" {
		return EmailMetadataView{}, ErrNotFound
	}
	return s.emailMetadata(ctx, versionID, generationID)
}
func (s *Store) emailMetadata(ctx context.Context, versionID, generationID string) (EmailMetadataView, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return EmailMetadataView{}, err
	}
	defer func() { _ = tx.Rollback() }()
	v, err := emailVersion(ctx, tx, versionID)
	if err != nil {
		return EmailMetadataView{}, err
	}
	return emailMetadataView(ctx, tx, v, generationID)
}
func emailMetadataView(ctx context.Context, q metadataQuerier, v ContentVersion, generationID string) (EmailMetadataView, error) {
	view := EmailMetadataView{Version: v, BodySearch: EmailBodySearch{State: "pending"}}
	suppressed, err := emailInventorySuppressed(ctx, q, v)
	if err != nil {
		return view, err
	}
	if suppressed {
		return view, ErrEmailDerivativeSuppressed
	}
	var id string
	if generationID == "" {
		err = q.QueryRowContext(ctx, `SELECT attachment_id FROM email_heads WHERE content_version_id=?`, v.ID).Scan(&id)
	} else {
		err = q.QueryRowContext(ctx, `SELECT attachment_id FROM email_attachments WHERE content_version_id=? AND generation_id=?`, v.ID, generationID).Scan(&id)
	}
	if errors.Is(err, sql.ErrNoRows) {
		if generationID != "" {
			return view, ErrNotFound
		}
		if emailMIME(v.MimeType) {
			return view, ErrEmailPending
		}
		return view, ErrEmailNotSupported
	}
	if err != nil {
		return view, err
	}
	view.Attachment, err = loadEmailAttachment(ctx, q, id)
	if err != nil {
		return view, err
	}
	if view.Attachment.ContentVersionID != v.ID {
		return view, fmt.Errorf("%w: cross-version head", ErrEmailCorrupt)
	}
	view.Generation, view.Evidence, err = loadEmailGeneration(ctx, q, view.Attachment.GenerationID)
	if err != nil {
		return view, err
	}
	if view.Generation.SourceSHA256 != v.BlobHash || view.Generation.SourceSize != v.Size {
		return view, fmt.Errorf("%w: cross-source attachment", ErrEmailCorrupt)
	}
	view.PublishedAt = view.Attachment.AttachedAt
	view.BodySearch, err = emailBodySearch(ctx, q, view)
	return view, err
}
func emailMIME(value string) bool {
	media, _, err := mime.ParseMediaType(value)
	return err == nil && strings.EqualFold(media, "message/rfc822")
}
func (s *Store) EmailPart(ctx context.Context, versionID, generationID, partPath, role string) (EmailPartReceipt, error) {
	if err := document.ValidateEmailPartPath(partPath); err != nil {
		return EmailPartReceipt{}, ErrInvalidEmailPart
	}
	if role != "raw_headers" && role != "decoded_payload" && role != "body_utf8" {
		return EmailPartReceipt{}, ErrInvalidEmailPart
	}
	v, err := s.EmailMetadataGeneration(ctx, versionID, generationID)
	if err != nil {
		return EmailPartReceipt{}, err
	}
	if v.Evidence.Inventory == nil {
		return EmailPartReceipt{}, ErrEmailPartUnavailable
	}
	exists := false
	filename := ""
	for _, p := range v.Evidence.Inventory.Parts {
		if p.Path == partPath {
			exists = true
			filename = p.Filename.SafeName
			break
		}
	}
	if !exists {
		return EmailPartReceipt{}, ErrNotFound
	}
	if filename == "" {
		filename, err = document.SafeEmailFilename("", partPath)
		if err != nil {
			return EmailPartReceipt{}, fmt.Errorf("%w: fallback part filename: %w", ErrEmailCorrupt, err)
		}
	}
	for _, a := range v.Generation.Artifacts {
		if a.PartPath == partPath && a.Role == role {
			return EmailPartReceipt{
				Version: v.Version, GenerationID: v.Generation.ID, AttachmentID: v.Attachment.ID,
				PartPath: a.PartPath, Role: a.Role, Filename: filename, BlobSHA256: a.BlobSHA256, Size: a.Size,
			}, nil
		}
	}
	return EmailPartReceipt{}, ErrEmailPartUnavailable
}

// MissingEmailTargetsAfter is keyed by immutable version occurrence, including
// inventories whose transient body publication has not yet completed.
func (s *Store) MissingEmailTargetsAfter(
	ctx context.Context, recipeFingerprint, bodyProfileFingerprint, afterVersionID string, limit int,
) ([]EmailTarget, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("email target page size must be between 1 and 100")
	}
	if err := validateCatalogSHA256(recipeFingerprint, "email recipe"); err != nil {
		return nil, err
	}
	if err := validateCatalogSHA256(bodyProfileFingerprint, "email body profile"); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	// Scan bounded pages so MIME normalization and suppression use the same Go
	// validators as publication without materializing the entire retained vault.
	result := []EmailTarget{}
	for len(result) < limit {
		versionIDs, err := readMissingEmailVersionIDsPage(ctx, tx, recipeFingerprint, afterVersionID)
		if err != nil {
			return nil, err
		}
		if len(versionIDs) == 0 {
			break
		}
		for _, versionID := range versionIDs {
			afterVersionID = versionID
			v, err := emailVersion(ctx, tx, versionID)
			if err != nil {
				return nil, err
			}
			if !emailMIME(v.MimeType) {
				continue
			}
			suppressed, err := emailInventorySuppressed(ctx, tx, v)
			if err != nil {
				return nil, err
			}
			if suppressed {
				continue
			}
			selectedRecipe, err := selectedEmailRecipeFingerprint(ctx, tx, v.ID)
			if err != nil && !errors.Is(err, ErrNotFound) {
				return nil, err
			}
			if selectedRecipe == recipeFingerprint {
				suppressed, err = emailBodyProfileSuppressed(ctx, tx, v, bodyProfileFingerprint)
				if err != nil {
					return nil, err
				}
				if suppressed {
					continue
				}
			}
			result = append(result, EmailTarget{Version: v})
			if len(result) == limit {
				break
			}
		}
	}
	return result, nil
}

func selectedEmailRecipeFingerprint(
	ctx context.Context, q metadataQuerier, versionID string,
) (string, error) {
	var fingerprint string
	err := q.QueryRowContext(ctx, `SELECT g.recipe_fingerprint FROM email_heads h
		JOIN email_attachments a ON a.attachment_id=h.attachment_id
		JOIN email_generations g ON g.generation_id=a.generation_id
		WHERE h.content_version_id=?`, versionID).Scan(&fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return fingerprint, err
}

const missingEmailVersionIDsQuery = `SELECT version_id FROM content_versions
		WHERE version_id>?
		AND instr(lower(COALESCE(mime_type,'')),'message/rfc822')>0
		AND NOT EXISTS (
			SELECT 1 FROM email_heads h
			JOIN email_attachments a ON a.attachment_id=h.attachment_id
				AND a.content_version_id=content_versions.version_id
			JOIN email_generations g ON g.generation_id=a.generation_id
				AND g.recipe_fingerprint=?
			JOIN email_body_results b ON b.email_attachment_id=a.attachment_id
			WHERE h.content_version_id=content_versions.version_id
			AND (b.state='unavailable' OR EXISTS (
				SELECT 1 FROM rendition_heads rh
				JOIN rendition_attachments ra ON ra.attachment_id=rh.attachment_id
				JOIN rendition_lexical_generation_builds gb ON gb.build_id=ra.build_id
				JOIN rendition_lexical_heads lh ON lh.generation_id=gb.generation_id
				WHERE rh.attachment_id=b.rendition_attachment_id
			))
		)
		ORDER BY version_id LIMIT 100`

func readMissingEmailVersionIDsPage(
	ctx context.Context, q metadataQuerier, recipeFingerprint, after string,
) (versionIDs []string, retErr error) {
	rows, err := q.QueryContext(ctx, missingEmailVersionIDsQuery, after, recipeFingerprint)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	for rows.Next() {
		var versionID string
		if err := rows.Scan(&versionID); err != nil {
			return nil, err
		}
		versionIDs = append(versionIDs, versionID)
	}
	return versionIDs, rows.Err()
}
