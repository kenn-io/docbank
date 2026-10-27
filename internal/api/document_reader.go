package api

import (
	"context"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/misslog"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

// DocumentReader shares the daemon's document services without HTTP or discovery.
// Its cursor key belongs to this reader's lifetime and selected vault.
type DocumentReader struct {
	*documentQueryService

	missLog    *misslog.Service
	processing *processing.Service
	blobs      *blob.Store
	vaultRoot  string
}

func NewDocumentReader(st *store.Store, blobs *blob.Store, vaultRoot string, service *processing.Service, logs ...*misslog.Service) *DocumentReader {
	var log *misslog.Service
	if len(logs) > 0 {
		log = logs[0]
	}
	return &DocumentReader{missLog: log, documentQueryService: newDocumentQueryService(Deps{Store: st}), processing: service, blobs: blobs, vaultRoot: vaultRoot}
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
	report, err := searchDocuments(ctx, r.processing, r.store, input)
	if err == nil {
		report.MissLogID = recordDocumentSearch(ctx, r.missLog, input, report)
	}
	return report, err
}
