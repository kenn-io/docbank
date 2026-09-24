package exporter

import (
	"context"
	"fmt"
	"io"

	"github.com/google/uuid"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
)

// PublishProductionPackage builds one recipient package from a completed job,
// then keeps only its verified vault-owned versions. A retry with the same
// operation ID reconciles through Store rather than publishing another copy.
func (w *Worker) PublishProductionPackage(ctx context.Context, operationID, jobID, profileID string,
	limits production.PackageLimits) (retained store.RetainedProductionPackage, err error) {
	if w == nil || ctx == nil || w.catalog == nil || w.blobs == nil || w.gate == nil {
		return store.RetainedProductionPackage{}, production.ErrPackageEvidence
	}
	for _, value := range []string{operationID, jobID} {
		parsed, parseErr := uuid.Parse(value)
		if parseErr != nil || parsed.Version() != 4 || parsed.String() != value {
			return store.RetainedProductionPackage{}, production.ErrPackageEvidence
		}
	}
	w.packageMu.RLock()
	defer w.packageMu.RUnlock()
	runtime := production.PackageRuntime{
		Catalog:    w.catalog,
		Opener:     processing.ProductionFinalArtifactAdapter{Catalog: w.catalog, Blobs: w.blobs},
		StagingDir: w.dir,
		Retain: func(ctx context.Context, request production.RecipientPackageRequest,
			_ production.PublishedRecipientPackage) error {
			return w.gate.MutateContext(ctx, func() error {
				return w.blobs.WithMutation(ctx, func() error {
					var retainErr error
					retained, retainErr = w.catalog.RetainProductionPackage(ctx, jobID, operationID,
						profileID, limits, request.ArchivePath, request.QCPath,
						request.TransmittalPath, w.writeProductionPackageBlob)
					return retainErr
				})
			})
		},
	}
	_, err = runtime.Run(ctx, jobID, profileID, limits)
	if err != nil {
		return store.RetainedProductionPackage{}, err
	}
	return retained, nil
}

func (w *Worker) writeProductionPackageBlob(ctx context.Context, reader io.Reader) (string, int64, store.BlobPhysical, error) {
	written, err := w.blobs.WriteDetailedContext(ctx, reader)
	if err != nil {
		return "", 0, store.BlobPhysical{}, fmt.Errorf("writing recipient package blob: %w", err)
	}
	encoding, err := written.EncodingName()
	if err != nil {
		return "", 0, store.BlobPhysical{}, err
	}
	return written.Hash, written.Size, store.BlobPhysical{
		Encoding: encoding, StoredBytes: written.StoredSize, PackEligible: written.PackEligible,
		MD5: written.MD5, Created: written.Created,
	}, nil
}
