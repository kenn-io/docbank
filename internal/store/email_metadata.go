package store

import (
	"context"
	"errors"
	"fmt"
)

type metadataEmailGeneration struct {
	Type              string `json:"type"`
	GenerationID      string `json:"generation_id" db:"generation_id"`
	SourceSHA256      string `json:"source_sha256" db:"source_sha256"`
	SourceSize        int64  `json:"source_size" db:"source_size"`
	RecipeFingerprint string `json:"recipe_fingerprint" db:"recipe_fingerprint"`
	CanonicalJSON     []byte `json:"canonical_json" format:"byte" db:"canonical_json"`
	Checksum          string `json:"checksum" db:"checksum"`
	CreatedAt         string `json:"created_at" db:"created_at"`
}

type metadataEmailPartArtifact struct {
	Type         string `json:"type"`
	GenerationID string `json:"generation_id" db:"generation_id"`
	PartPath     string `json:"part_path" db:"part_path"`
	Role         string `json:"role" db:"role"`
	BlobHash     string `json:"blob_hash" db:"blob_hash"`
	Size         int64  `json:"size" db:"size"`
}

type metadataEmailAttachment struct {
	Type             string `json:"type"`
	AttachmentID     string `json:"attachment_id" db:"attachment_id"`
	ContentVersionID string `json:"content_version_id" db:"content_version_id"`
	GenerationID     string `json:"generation_id" db:"generation_id"`
	AttachedAt       string `json:"attached_at" db:"attached_at"`
}

type metadataEmailHead struct {
	Type             string `json:"type"`
	ContentVersionID string `json:"content_version_id" db:"content_version_id"`
	AttachmentID     string `json:"attachment_id" db:"attachment_id"`
	PublishedAt      string `json:"published_at" db:"published_at"`
}

type metadataEmailBodyResult struct {
	Type                  string  `json:"type"`
	EmailAttachmentID     string  `json:"email_attachment_id" db:"email_attachment_id"`
	BodyRecipeFingerprint string  `json:"body_recipe_fingerprint" db:"body_recipe_fingerprint"`
	State                 string  `json:"state" db:"state"`
	PartPath              *string `json:"part_path" db:"part_path"`
	RenditionAttachmentID *string `json:"rendition_attachment_id" db:"rendition_attachment_id"`
	Reason                *string `json:"reason" db:"reason"`
}

var emailHeadMetadata = newMetadataTable(metadataTable[metadataEmailHead]{
	record: metadataEmailHead{Type: "email_head"}, table: "email_heads", suffix: "ORDER BY content_version_id"})

// emailMetadataTables exports the email records in dependency order. None of
// them is validated per row; validateEmailMetadataState checks the graph.
var emailMetadataTables = []metadataRecordCodec{
	newMetadataTable(metadataTable[metadataEmailGeneration]{record: metadataEmailGeneration{Type: "email_generation"},
		table: "email_generations", suffix: "ORDER BY generation_id"}),
	newMetadataTable(metadataTable[metadataEmailPartArtifact]{record: metadataEmailPartArtifact{Type: "email_part_artifact"},
		table: "email_part_artifacts", suffix: "ORDER BY generation_id,part_path,role"}),
	newMetadataTable(metadataTable[metadataEmailAttachment]{record: metadataEmailAttachment{Type: "email_attachment"},
		table: "email_attachments", suffix: "ORDER BY attachment_id"}),
	emailHeadMetadata,
	newMetadataTable(metadataTable[metadataEmailBodyResult]{record: metadataEmailBodyResult{Type: "email_body_result"},
		table: "email_body_results", suffix: "ORDER BY email_attachment_id"}),
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
	return emailHeadMetadata.export(ctx, q, func(value any) error {
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
