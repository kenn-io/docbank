package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"fmt"
)

type metadataEmailGeneration struct {
	Type              string `json:"type"`
	GenerationID      string `json:"generation_id"`
	SourceSHA256      string `json:"source_sha256"`
	SourceSize        int64  `json:"source_size"`
	RecipeFingerprint string `json:"recipe_fingerprint"`
	CanonicalJSON     []byte `json:"canonical_json" format:"byte"`
	Checksum          string `json:"checksum"`
	CreatedAt         string `json:"created_at"`
}

type metadataEmailPartArtifact struct {
	Type         string `json:"type"`
	GenerationID string `json:"generation_id"`
	PartPath     string `json:"part_path"`
	Role         string `json:"role"`
	BlobHash     string `json:"blob_hash"`
	Size         int64  `json:"size"`
}

type metadataEmailAttachment struct {
	Type             string `json:"type"`
	AttachmentID     string `json:"attachment_id"`
	ContentVersionID string `json:"content_version_id"`
	GenerationID     string `json:"generation_id"`
	AttachedAt       string `json:"attached_at"`
}

type metadataEmailHead struct {
	Type             string `json:"type"`
	ContentVersionID string `json:"content_version_id"`
	AttachmentID     string `json:"attachment_id"`
	PublishedAt      string `json:"published_at"`
}

type metadataEmailBodyResult struct {
	Type                  string  `json:"type"`
	EmailAttachmentID     string  `json:"email_attachment_id"`
	BodyRecipeFingerprint string  `json:"body_recipe_fingerprint"`
	State                 string  `json:"state"`
	PartPath              *string `json:"part_path"`
	RenditionAttachmentID *string `json:"rendition_attachment_id"`
	Reason                *string `json:"reason"`
}

func exportEmailMetadata(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	if err := exportEmailGeneration(ctx, q, write); err != nil {
		return err
	}
	if err := exportEmailPartArtifact(ctx, q, write); err != nil {
		return err
	}
	if err := exportEmailAttachment(ctx, q, write); err != nil {
		return err
	}
	if err := exportEmailHead(ctx, q, write); err != nil {
		return err
	}
	if err := exportEmailBodyResult(ctx, q, write); err != nil {
		return err
	}
	return nil
}
func exportEmailGeneration(ctx context.Context, q metadataQuerier, write metadataWrite) (retErr error) {
	rows, err := q.QueryContext(ctx, `SELECT generation_id,source_sha256,source_size,recipe_fingerprint,canonical_json,checksum,created_at FROM email_generations ORDER BY generation_id`)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	for rows.Next() {
		v := metadataEmailGeneration{Type: "email_generation"}
		if err := rows.Scan(&v.GenerationID, &v.SourceSHA256, &v.SourceSize, &v.RecipeFingerprint, &v.CanonicalJSON, &v.Checksum, &v.CreatedAt); err != nil {
			return err
		}
		if err := write(v); err != nil {
			return err
		}
	}
	return rows.Err()
}
func exportEmailPartArtifact(ctx context.Context, q metadataQuerier, write metadataWrite) (retErr error) {
	rows, err := q.QueryContext(ctx, `SELECT generation_id,part_path,role,blob_hash,size FROM email_part_artifacts ORDER BY generation_id,part_path,role`)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	for rows.Next() {
		v := metadataEmailPartArtifact{Type: "email_part_artifact"}
		if err := rows.Scan(&v.GenerationID, &v.PartPath, &v.Role, &v.BlobHash, &v.Size); err != nil {
			return err
		}
		if err := write(v); err != nil {
			return err
		}
	}
	return rows.Err()
}
func exportEmailAttachment(ctx context.Context, q metadataQuerier, write metadataWrite) (retErr error) {
	rows, err := q.QueryContext(ctx, `SELECT attachment_id,content_version_id,generation_id,attached_at FROM email_attachments ORDER BY attachment_id`)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	for rows.Next() {
		v := metadataEmailAttachment{Type: "email_attachment"}
		if err := rows.Scan(&v.AttachmentID, &v.ContentVersionID, &v.GenerationID, &v.AttachedAt); err != nil {
			return err
		}
		if err := write(v); err != nil {
			return err
		}
	}
	return rows.Err()
}
func exportEmailHead(ctx context.Context, q metadataQuerier, write metadataWrite) (retErr error) {
	rows, err := q.QueryContext(ctx, `SELECT content_version_id,attachment_id,published_at FROM email_heads ORDER BY content_version_id`)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	for rows.Next() {
		v := metadataEmailHead{Type: "email_head"}
		if err := rows.Scan(&v.ContentVersionID, &v.AttachmentID, &v.PublishedAt); err != nil {
			return err
		}
		if err := write(v); err != nil {
			return err
		}
	}
	return rows.Err()
}
func exportEmailBodyResult(ctx context.Context, q metadataQuerier, write metadataWrite) (retErr error) {
	rows, err := q.QueryContext(ctx, `SELECT email_attachment_id,body_recipe_fingerprint,state,part_path,rendition_attachment_id,reason FROM email_body_results ORDER BY email_attachment_id`)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	for rows.Next() {
		v := metadataEmailBodyResult{Type: "email_body_result"}
		if err := rows.Scan(&v.EmailAttachmentID, &v.BodyRecipeFingerprint, &v.State, &v.PartPath, &v.RenditionAttachmentID, &v.Reason); err != nil {
			return err
		}
		if err := write(v); err != nil {
			return err
		}
	}
	return rows.Err()
}
func importEmailMetadataRecord(ctx context.Context, tx *sql.Tx, kind string, raw jsontext.Value) error {
	switch kind {
	case "email_generation":
		var v metadataEmailGeneration
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO email_generations(generation_id,source_sha256,source_size,recipe_fingerprint,canonical_json,checksum,created_at) VALUES(?,?,?,?,?,?,?)`, v.GenerationID, v.SourceSHA256, v.SourceSize, v.RecipeFingerprint, v.CanonicalJSON, v.Checksum, v.CreatedAt)
		return err
	case "email_part_artifact":
		var v metadataEmailPartArtifact
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO email_part_artifacts(generation_id,part_path,role,blob_hash,size) VALUES(?,?,?,?,?)`, v.GenerationID, v.PartPath, v.Role, v.BlobHash, v.Size)
		return err
	case "email_attachment":
		var v metadataEmailAttachment
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO email_attachments(attachment_id,content_version_id,generation_id,attached_at) VALUES(?,?,?,?)`, v.AttachmentID, v.ContentVersionID, v.GenerationID, v.AttachedAt)
		return err
	case "email_head":
		var v metadataEmailHead
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO email_heads(content_version_id,attachment_id,published_at) VALUES(?,?,?)`, v.ContentVersionID, v.AttachmentID, v.PublishedAt)
		return err
	case "email_body_result":
		var v metadataEmailBodyResult
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO email_body_results(email_attachment_id,body_recipe_fingerprint,state,part_path,rendition_attachment_id,reason) VALUES(?,?,?,?,?,?)`, v.EmailAttachmentID, v.BodyRecipeFingerprint, v.State, v.PartPath, v.RenditionAttachmentID, v.Reason)
		return err
	default:
		return fmt.Errorf("unknown email record type %q", kind)
	}
}

// Validate the entire graph after import and before export, including historical
// attachments and body results that are no longer selected for serving.
func validateEmailMetadataState(ctx context.Context, q metadataQuerier) error {
	ids, err := loadProcessingMetadataIDs(ctx, q, "email authority", `SELECT generation_id FROM email_generations ORDER BY generation_id`)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, _, err := loadEmailGeneration(ctx, q, id); err != nil {
			return err
		}
	}
	ids, err = loadProcessingMetadataIDs(ctx, q, "email authority", `SELECT attachment_id FROM email_attachments ORDER BY attachment_id`)
	if err != nil {
		return err
	}
	for _, id := range ids {
		a, err := loadEmailAttachment(ctx, q, id)
		if err != nil {
			return err
		}
		v, err := emailVersion(ctx, q, a.ContentVersionID)
		if err != nil {
			return err
		}
		g, e, err := loadEmailGeneration(ctx, q, a.GenerationID)
		if err != nil {
			return err
		}
		if v.BlobHash != g.SourceSHA256 || v.Size != g.SourceSize {
			return fmt.Errorf("%w: attachment source", ErrEmailCorrupt)
		}
		suppressed, err := emailInventorySuppressed(ctx, q, v)
		if err != nil {
			return err
		}
		if suppressed {
			return fmt.Errorf("%w: suppressed email inventory retained", ErrEmailCorrupt)
		}
		if err := validateEmailBodyResult(ctx, q, EmailMetadataView{Version: v, Generation: g, Attachment: a, Evidence: e}); err != nil {
			return err
		}
	}
	return exportEmailHead(ctx, q, func(value any) error {
		h, ok := value.(metadataEmailHead)
		if !ok {
			return errors.New("invalid email head validator input")
		}
		a, err := loadEmailAttachment(ctx, q, h.AttachmentID)
		if err != nil {
			return err
		}
		if a.ContentVersionID != h.ContentVersionID || a.AttachedAt != h.PublishedAt {
			return fmt.Errorf("%w: email head identity or observation", ErrEmailCorrupt)
		}
		return validateMetadataTime("email published_at", h.PublishedAt)
	})
}
