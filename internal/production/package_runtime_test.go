package production

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

type packageRuntimeCatalog struct{ inputs PublishedPackageInputs }

func (c packageRuntimeCatalog) LoadProductionPackageInputs(_ context.Context, _ string) (PublishedPackageInputs, error) {
	return c.inputs, nil
}

func TestPackageRuntimeRetainsVerifiedHandoffAndCleansPrivateStaging(t *testing.T) {
	job, _, opener := packageArchiveJobFixture(t, "export-dat-opt-images-v1")
	_, numbers, members := packageProjectionFixture(t)
	root := t.TempDir()
	retained := false
	runtime := PackageRuntime{
		Catalog: packageRuntimeCatalog{PublishedPackageInputs{Job: job, Reservation: numbers, Members: members}},
		Opener:  opener, StagingDir: root,
		Retain: func(ctx context.Context, request RecipientPackageRequest, published PublishedRecipientPackage) error {
			retained = true
			require.Equal(t, job.ID, request.JobID)
			require.Equal(t, "export-dat-opt-images-v1", published.Manifest.ProfileID)
			require.Equal(t, filepath.Dir(request.ArchivePath), filepath.Dir(request.QCPath))
			require.Equal(t, filepath.Dir(request.ArchivePath), filepath.Dir(request.TransmittalPath))
			require.NoError(t, VerifyRecipientArchiveWithQCContext(ctx, request.ArchivePath, published.QC))
			_, err := ReadPackageQCReceipt(request.QCPath)
			require.NoError(t, err)
			_, err = ReadRecipientTransmittal(request.TransmittalPath)
			require.NoError(t, err)
			return nil
		},
	}
	result, err := runtime.Run(t.Context(), job.ID, "export-dat-opt-images-v1",
		PackageLimits{MaxVolumeBytes: 1000, MaxVolumeDocuments: 10})
	require.NoError(t, err)
	require.True(t, retained)
	require.Equal(t, "export-dat-opt-images-v1", result.Manifest.ProfileID)
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries)

	errLost := errors.New("synthetic retention response loss")
	runtime.Retain = func(context.Context, RecipientPackageRequest, PublishedRecipientPackage) error { return errLost }
	_, err = runtime.Run(t.Context(), job.ID, "export-dat-opt-images-v1",
		PackageLimits{MaxVolumeBytes: 1000, MaxVolumeDocuments: 10})
	require.ErrorIs(t, err, errLost)
	entries, err = os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries)
}
