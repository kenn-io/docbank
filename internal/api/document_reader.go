package api

import (
	"context"
	"errors"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

// DocumentReader shares the daemon's document services without HTTP or discovery.
// Its cursor key belongs to this reader's lifetime and selected vault.
type DocumentReader struct {
	*documentQueryService

	processing *processing.Service
	blobs      *blob.Store
	vaultRoot  string
}

func NewDocumentReader(st *store.Store, blobs *blob.Store, vaultRoot string, service *processing.Service) *DocumentReader {
	return &DocumentReader{documentQueryService: newDocumentQueryService(Deps{Store: st}), processing: service, blobs: blobs, vaultRoot: vaultRoot}
}

// CatalogVaultID reads vault identity without inventorying physical storage.
func (r *DocumentReader) CatalogVaultID(ctx context.Context) (string, error) {
	if r == nil || r.documentQueryService == nil || r.store == nil {
		return "", errors.New("document reader has no catalog")
	}
	status, err := r.store.AuditStatus(ctx, nil)
	if err != nil {
		return "", err
	}
	return status.VaultID, nil
}

func (r *DocumentReader) VaultInfo(ctx context.Context) (VaultInfo, error) {
	return readVaultInfo(ctx, Deps{Store: r.store, Blobs: r.blobs, VaultRoot: r.vaultRoot})
}

func (r *DocumentReader) ResolveDocumentSourceFence(ctx context.Context, input DocumentSourceFenceResolveRequest) (DocumentSourceFenceResolution, error) {
	return resolveDocumentSourceFence(ctx, r.processing, input)
}

func (r *DocumentReader) ValidateDocumentSearch(ctx context.Context, input DocumentSearchValidationRequest) error {
	return validateDocumentSearch(ctx, r.processing, input)
}

func (r *DocumentReader) SearchDocuments(ctx context.Context, input DocumentSearchRequest) (DocumentSearchReport, error) {
	return searchDocuments(ctx, r.processing, input)
}
