package api

import (
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

const (
	EmailGenerationHeader = "X-Docbank-Email-Generation"
	EmailAttachmentHeader = "X-Docbank-Email-Attachment"
	EmailPartPathHeader   = "X-Docbank-Email-Part-Path"
	EmailPartRoleHeader   = "X-Docbank-Email-Part-Role"
)

type EmailBodySearch struct {
	State                 string  `json:"state"`
	RenditionBuildID      *string `json:"rendition_build_id"`
	RenditionAttachmentID *string `json:"rendition_attachment_id"`
	Reason                *string `json:"reason"`
}

type EmailMetadata struct {
	Version           ContentVersion   `json:"version"`
	GenerationID      string           `json:"generation_id"`
	AttachmentID      string           `json:"attachment_id"`
	RecipeFingerprint string           `json:"recipe_fingerprint"`
	Checksum          string           `json:"checksum"`
	Evidence          document.EmailV1 `json:"evidence"`
	BodySearch        EmailBodySearch  `json:"body_search"`
	CreatedAt         string           `json:"created_at"`
	PublishedAt       string           `json:"published_at"`
}

type EmailPending struct {
	Version ContentVersion `json:"version"`
	State   string         `json:"state"`
}

type EmailPartReceipt struct {
	Version      ContentVersion `json:"version"`
	GenerationID string         `json:"generation_id"`
	AttachmentID string         `json:"attachment_id"`
	PartPath     string         `json:"part_path"`
	Role         string         `json:"role"`
	BlobSHA256   string         `json:"blob_sha256"`
	Size         int64          `json:"size"`
}

func fromStoreEmailMetadata(view store.EmailMetadataView) EmailMetadata {
	return EmailMetadata{
		Version:           fromStoreContentVersion(view.Version),
		GenerationID:      view.Generation.ID,
		AttachmentID:      view.Attachment.ID,
		RecipeFingerprint: view.Generation.RecipeFingerprint,
		Checksum:          view.Generation.Checksum,
		Evidence:          view.Evidence,
		BodySearch: EmailBodySearch{
			State:                 view.BodySearch.State,
			RenditionBuildID:      view.BodySearch.RenditionBuildID,
			RenditionAttachmentID: view.BodySearch.RenditionAttachmentID,
			Reason:                view.BodySearch.Reason,
		},
		CreatedAt: view.Generation.CreatedAt, PublishedAt: view.PublishedAt,
	}
}
