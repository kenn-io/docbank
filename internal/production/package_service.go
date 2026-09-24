package production

import (
	"context"
	"path/filepath"

	documentproduction "go.kenn.io/docbank/document/production"
)

// PublishedPackageInputs are the immutable inputs recovered from one completed
// job. The catalog owns their historical validation before this service runs.
type PublishedPackageInputs struct {
	Job         Job
	Reservation documentproduction.NumberReservation
	Members     []PackageMember
}

// PublishedPackageCatalog reads only completed, verified job authority.
type PublishedPackageCatalog interface {
	LoadProductionPackageInputs(ctx context.Context, jobID string) (PublishedPackageInputs, error)
}

// RecipientPackageRequest names one immutable archive and its external
// sidecars. The caller owns the private destination directory.
type RecipientPackageRequest struct {
	JobID           string
	ProfileID       string
	Limits          PackageLimits
	ArchivePath     string
	QCPath          string
	TransmittalPath string
}

// PublishedRecipientPackage returns the public manifest and actual-archive QC.
type PublishedRecipientPackage struct {
	Manifest RecipientManifest
	QC       PackageQC
}

// PublishRecipientPackage builds a recipient archive from a published job's
// verified output artifacts, then publishes immutable QC and transmittal
// sidecars. An exact retry reopens the existing archive and sidecars.
func PublishRecipientPackage(ctx context.Context, catalog PublishedPackageCatalog,
	opener PackageArtifactOpener, request RecipientPackageRequest) (PublishedRecipientPackage, error) {
	if ctx == nil || catalog == nil || opener == nil || request.JobID == "" ||
		request.ArchivePath == "" || request.QCPath == "" || request.TransmittalPath == "" {
		return PublishedRecipientPackage{}, ErrRecipientArchive
	}
	archive, qcPath, transmittalPath := filepath.Clean(request.ArchivePath),
		filepath.Clean(request.QCPath), filepath.Clean(request.TransmittalPath)
	if archive == qcPath || archive == transmittalPath || qcPath == transmittalPath {
		return PublishedRecipientPackage{}, ErrRecipientArchive
	}
	inputs, err := catalog.LoadProductionPackageInputs(ctx, request.JobID)
	if err != nil {
		return PublishedRecipientPackage{}, err
	}
	projection, err := PlanPackageProjection(inputs.Job, inputs.Reservation, inputs.Members,
		request.ProfileID, request.Limits)
	if err != nil {
		return PublishedRecipientPackage{}, err
	}
	qc, err := BuildRecipientArchive(ctx, projection, request.JobID, opener, archive)
	if err != nil {
		return PublishedRecipientPackage{}, err
	}
	if err := PublishPackageQCReceipt(archive, qcPath, qc); err != nil {
		return PublishedRecipientPackage{}, err
	}
	if err := PublishRecipientTransmittal(archive, transmittalPath, projection.Manifest, qc); err != nil {
		return PublishedRecipientPackage{}, err
	}
	return PublishedRecipientPackage{Manifest: projection.Manifest, QC: qc}, nil
}
