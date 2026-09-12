package docbank

import (
	"context"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

var ErrEmailDocumentConflict = store.ErrEmailDocumentConflict
var ErrInvalidEmailDocumentRequest = store.ErrInvalidEmailDocumentRequest

func (v *Vault) RequestEmailDocumentProcessing(ctx context.Context, request document.EmailDocumentProcessingRequest) (document.EmailDocumentProcessingReceipt, error) {
	if err := v.begin(); err != nil {
		return document.EmailDocumentProcessingReceipt{}, err
	}
	defer v.lifecycle.RUnlock()
	v.mutation.Lock()
	defer v.mutation.Unlock()
	return v.metadata.RequestEmailDocumentProcessing(ctx, request)
}

// PublishEmailDocuments creates ordinary children from one retained MIME
// inventory. Its exact receipt is reusable after response loss and restore.
func (v *Vault) PublishEmailDocuments(ctx context.Context, request document.EmailDocumentPublicationRequest) (document.EmailDocumentPublicationReceipt, error) {
	if err := v.begin(); err != nil {
		return document.EmailDocumentPublicationReceipt{}, err
	}
	defer v.lifecycle.RUnlock()
	v.mutation.Lock()
	defer v.mutation.Unlock()
	return processing.PublishEmailDocuments(ctx, v.metadata, v.blobs, request)
}
func (v *Vault) EmailDocumentPublication(ctx context.Context, operationID string) (document.EmailDocumentPublicationReceipt, error) {
	if err := v.begin(); err != nil {
		return document.EmailDocumentPublicationReceipt{}, err
	}
	defer v.lifecycle.RUnlock()
	return v.metadata.EmailDocumentPublication(ctx, operationID)
}
func (v *Vault) EmailDocumentRelations(ctx context.Context, query document.EmailDocumentRelationQuery) (document.EmailDocumentRelationPage, error) {
	if err := v.begin(); err != nil {
		return document.EmailDocumentRelationPage{}, err
	}
	defer v.lifecycle.RUnlock()
	return v.metadata.EmailDocumentRelations(ctx, query)
}

// RemoveEmailDocumentPublication releases a receipt and its exact references,
// leaving the ordinary children intact. This relinquishes its retry guarantee.
func (v *Vault) RemoveEmailDocumentPublication(ctx context.Context, operationID, digest string) error {
	if err := v.begin(); err != nil {
		return err
	}
	defer v.lifecycle.RUnlock()
	v.mutation.Lock()
	defer v.mutation.Unlock()
	return v.metadata.RemoveEmailDocumentPublication(ctx, operationID, digest)
}
