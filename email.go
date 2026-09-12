package docbank

import (
	"context"
	"errors"
	"fmt"
	"io"

	"go.kenn.io/docbank/document"
	internalprocessing "go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

// EmailBodySearch describes whether the chosen outer email body is serving
// through the exact retained rendition and lexical authority.
type EmailBodySearch struct {
	State                 string  `json:"state"`
	RenditionBuildID      *string `json:"rendition_build_id"`
	RenditionAttachmentID *string `json:"rendition_attachment_id"`
	Reason                *string `json:"reason"`
}

// EmailMetadata is canonical MIME evidence and mutable chosen-body search
// state for one exact immutable content version.
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

// EmailPartReceipt binds one verified MIME artifact stream to its exact
// version, generation, structural part, role, and byte identity.
type EmailPartReceipt struct {
	Version      ContentVersion `json:"version"`
	GenerationID string         `json:"generation_id"`
	AttachmentID string         `json:"attachment_id"`
	PartPath     string         `json:"part_path"`
	Role         string         `json:"role"`
	BlobSHA256   string         `json:"blob_sha256"`
	Size         int64          `json:"size"`
}

// EmailMetadata returns the selected canonical email evidence for one exact
// immutable content version. Pending and ineligible versions retain their
// populated Version alongside the corresponding error.
func (v *Vault) EmailMetadata(ctx context.Context, versionID string) (EmailMetadata, error) {
	if err := v.begin(); err != nil {
		return EmailMetadata{}, err
	}
	defer v.lifecycle.RUnlock()
	view, err := v.metadata.EmailMetadata(ctx, versionID)
	return fromStoreEmailMetadata(view), err
}

// EnsureEmailMetadata synchronously decodes and publishes current email
// metadata and chosen-body search authority for one exact retained version.
func (v *Vault) EnsureEmailMetadata(ctx context.Context, versionID string) (EmailMetadata, error) {
	if err := v.begin(); err != nil {
		return EmailMetadata{}, err
	}
	defer v.lifecycle.RUnlock()
	v.mutation.Lock()
	defer v.mutation.Unlock()
	if err := ctx.Err(); err != nil {
		return EmailMetadata{}, err
	}
	version, err := v.metadata.ContentVersionByID(ctx, versionID)
	if err != nil {
		return EmailMetadata{}, err
	}
	view, err := internalprocessing.EnsureEmailTarget(
		ctx, v.metadata, v.blobs, v.emailSpoolParent, store.EmailTarget{Version: version},
	)
	return fromStoreEmailMetadata(view), err
}

// EmailMetadataGeneration returns one immutable email generation attached to
// the requested exact content version without changing its selected head.
func (v *Vault) EmailMetadataGeneration(
	ctx context.Context, versionID, generationID string,
) (EmailMetadata, error) {
	if err := v.begin(); err != nil {
		return EmailMetadata{}, err
	}
	defer v.lifecycle.RUnlock()
	view, err := v.metadata.EmailMetadataGeneration(ctx, versionID, generationID)
	return fromStoreEmailMetadata(view), err
}

// OpenEmailPart opens one catalog-authorized MIME artifact. The caller must
// close the stream to release its vault lifecycle lease.
func (v *Vault) OpenEmailPart(
	ctx context.Context, versionID, generationID, partPath, role string,
) (io.ReadCloser, EmailPartReceipt, error) {
	if err := v.begin(); err != nil {
		return nil, EmailPartReceipt{}, err
	}
	receipt, err := v.metadata.EmailPart(ctx, versionID, generationID, partPath, role)
	if err != nil {
		v.lifecycle.RUnlock()
		return nil, fromStoreEmailPartReceipt(receipt), err
	}
	reader, size, err := v.blobs.OpenStreamContext(ctx, receipt.BlobSHA256)
	if err != nil {
		closeErr := closeContentReader(reader)
		v.lifecycle.RUnlock()
		return nil, fromStoreEmailPartReceipt(receipt), errors.Join(fmt.Errorf(
			"opening email part for version %q generation %q part %q role %q: %w: %w",
			versionID, generationID, partPath, role, ErrContentUnavailable, err,
		), closeErr)
	}
	if size != receipt.Size {
		closeErr := reader.Close()
		v.lifecycle.RUnlock()
		return nil, fromStoreEmailPartReceipt(receipt), errors.Join(fmt.Errorf(
			"email part catalog size %d does not match physical size %d: %w",
			receipt.Size, size, ErrContentUnavailable,
		), closeErr)
	}
	return &leasedReader{VerifiedReadCloser: reader, release: v.lifecycle.RUnlock},
		fromStoreEmailPartReceipt(receipt), nil
}

func fromStoreEmailMetadata(view store.EmailMetadataView) EmailMetadata {
	return EmailMetadata{
		Version:           fromStoreVersion(view.Version),
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

func fromStoreEmailPartReceipt(receipt store.EmailPartReceipt) EmailPartReceipt {
	return EmailPartReceipt{
		Version: fromStoreVersion(receipt.Version), GenerationID: receipt.GenerationID,
		AttachmentID: receipt.AttachmentID, PartPath: receipt.PartPath, Role: receipt.Role,
		BlobSHA256: receipt.BlobSHA256, Size: receipt.Size,
	}
}
