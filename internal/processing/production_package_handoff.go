package processing

import (
	"context"
	"errors"

	"go.kenn.io/docbank/internal/production"
)

// PrepareRetainedProductionPackageDownload reopens the exact retained vault
// versions, verifies their complete physical streams, and independently checks
// the archive and sidecars against the stored package evidence. The caller owns
// the returned private directory and must close it when the handoff ends.
func PrepareRetainedProductionPackageDownload(ctx context.Context,
	catalog RetainedProductionPackageCatalog, blobs verifiedBlobReader,
	stagingRoot, jobID, operationID string) (StagedRetainedProductionPackage, error) {
	staged, err := StageRetainedProductionPackageBlobs(ctx, catalog, blobs, stagingRoot, jobID, operationID)
	if err != nil {
		return StagedRetainedProductionPackage{}, err
	}
	if err := production.VerifyRetainedPackageHandoffContext(ctx, staged.ArchivePath,
		staged.QCPath, staged.TransmittalPath, staged.Retained.Evidence); err != nil {
		return StagedRetainedProductionPackage{}, errors.Join(err, staged.Close())
	}
	return staged, nil
}
