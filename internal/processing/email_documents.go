package processing

import (
	"context"
	"errors"
	"fmt"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
	"io"
)

// PublishEmailDocuments verifies every retained decoded payload before an
// atomic ordinary-file publication. No decoder, host path, or provider runs.
func PublishEmailDocuments(ctx context.Context, catalog *store.Store, blobs *blob.Store, request document.EmailDocumentPublicationRequest) (document.EmailDocumentPublicationReceipt, error) {
	digest, err := document.EmailDocumentRequestDigest(request)
	if err != nil {
		return document.EmailDocumentPublicationReceipt{}, fmt.Errorf("%w: %w", store.ErrInvalidEmailDocumentRequest, err)
	}
	if err = ctx.Err(); err != nil {
		return document.EmailDocumentPublicationReceipt{}, err
	}
	old, err := catalog.EmailDocumentPublication(ctx, request.OperationID)
	if err == nil {
		if old.RequestDigest != digest {
			return document.EmailDocumentPublicationReceipt{}, store.ErrEmailDocumentConflict
		}
		return old, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return document.EmailDocumentPublicationReceipt{}, err
	}
	var result document.EmailDocumentPublicationReceipt
	err = blobs.WithMutation(ctx, func() error {
		view, err := catalog.EmailMetadataGeneration(ctx, request.Parent.VersionID, request.GenerationID)
		if err != nil {
			return err
		}
		if view.Version.NodeID != request.Parent.NodeID || view.Version.BlobHash != request.Parent.SHA256 || view.Version.Size != request.Parent.Size || view.Attachment.ID != request.AttachmentID {
			return store.ErrEmailDocumentConflict
		}
		if view.Evidence.Inventory == nil || len(view.Evidence.Inventory.Parts) > document.EmailDocumentMaxParts {
			return store.ErrEmailPartUnavailable
		}
		var total int64
		for _, p := range document.EmailAttachmentParts(view.Evidence) {
			if err := ctx.Err(); err != nil {
				return err
			}
			if document.EmailDocumentPartOutcome(p) != "decoded" {
				continue
			}
			if p.Payload.Size < 0 || p.Payload.Size > 128<<20 {
				return store.ErrEmailCorrupt
			}
			total += p.Payload.Size
			if total > 256<<20 {
				return store.ErrEmailCorrupt
			}
			r, size, err := blobs.OpenStreamContext(ctx, p.Payload.SHA256)
			if err != nil {
				return err
			}
			if size != p.Payload.Size {
				return errors.Join(store.ErrEmailCorrupt, r.Close())
			}
			n, copyErr := io.Copy(io.Discard, io.LimitReader(r, size+1))
			closeErr := r.Close()
			if err = errors.Join(copyErr, closeErr); err != nil {
				return err
			}
			if n != size {
				return store.ErrEmailCorrupt
			}
		}
		result, err = catalog.PublishEmailDocuments(ctx, request)
		return err
	})
	return result, err
}
