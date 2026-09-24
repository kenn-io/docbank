package production

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// PackageRuntime owns private staging for a recipient package until its
// verified archive and external sidecars have been retained by the caller.
type PackageRuntime struct {
	Catalog    PublishedPackageCatalog
	Opener     PackageArtifactOpener
	StagingDir string
	Retain     func(context.Context, RecipientPackageRequest, PublishedRecipientPackage) error
}

func (r PackageRuntime) Run(ctx context.Context, jobID, profileID string,
	limits PackageLimits) (result PublishedRecipientPackage, err error) {
	if ctx == nil || r.Catalog == nil || r.Opener == nil || r.Retain == nil ||
		!filepath.IsAbs(r.StagingDir) || jobID == "" {
		return PublishedRecipientPackage{}, ErrRecipientArchive
	}
	if err := ctx.Err(); err != nil {
		return PublishedRecipientPackage{}, err
	}
	dir, err := os.MkdirTemp(r.StagingDir, ".production-package-")
	if err != nil {
		return PublishedRecipientPackage{}, err
	}
	defer func() {
		if removeErr := os.RemoveAll(dir); removeErr != nil {
			result = PublishedRecipientPackage{}
			err = errors.Join(err, removeErr)
		}
	}()
	request := RecipientPackageRequest{
		JobID: jobID, ProfileID: profileID, Limits: limits,
		ArchivePath:     filepath.Join(dir, "recipient.zip"),
		QCPath:          filepath.Join(dir, "qc.json"),
		TransmittalPath: filepath.Join(dir, "transmittal.json"),
	}
	result, err = PublishRecipientPackage(ctx, r.Catalog, r.Opener, request)
	if err != nil {
		return PublishedRecipientPackage{}, err
	}
	if err = ctx.Err(); err != nil {
		return PublishedRecipientPackage{}, err
	}
	if err = r.Retain(ctx, request, result); err != nil {
		return PublishedRecipientPackage{}, err
	}
	return result, nil
}
