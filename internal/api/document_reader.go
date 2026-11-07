package api

import (
	"context"

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

func (r *DocumentReader) VaultInfo(ctx context.Context) (VaultInfo, error) {
	return readVaultInfo(ctx, Deps{Store: r.store, Blobs: r.blobs, VaultRoot: r.vaultRoot})
}

func (r *DocumentReader) VaultID(ctx context.Context) (string, error) {
	return r.store.VaultID(), nil
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

func processingProfileSummaries(service *processing.Service) []ProcessingProfileSummary {
	profiles := service.Profiles()
	result := make([]ProcessingProfileSummary, len(profiles))
	for index, profile := range profiles {
		result[index] = ProcessingProfileSummary{Name: profile.Name, Fingerprint: profile.Fingerprint,
			Rendition: profile.Rendition, EmbeddingBindings: profile.EmbeddingBindings,
			QueryEmbeddingBindings: profile.QueryEmbeddingBindings, RerankingAvailable: profile.RerankingAvailable}
	}
	return result
}

func (r *DocumentReader) ListDocumentProcessingProfiles(ctx context.Context) ([]ProcessingProfileSummary, error) {
	if r.processing == nil {
		return nil, processingUnavailable()
	}
	return processingProfileSummaries(r.processing), nil
}
