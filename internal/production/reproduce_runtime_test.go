package production

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
)

func TestReproductionRuntimeBuildsFreshVerifiedPackageAndReplaysAfterLostResponse(t *testing.T) {
	inputs, opener, request, policy := reproductionFixture(t)
	root := t.TempDir()
	var firstQC, secondQC PackageQC
	retained := 0
	lostResponse := errors.New("synthetic lost retention response")
	runtime := ReproductionRuntime{
		Catalog: packageRuntimeCatalog{inputs}, Opener: opener, StagingDir: root,
		Retain: func(ctx context.Context, gotRequest documentproduction.ReproductionRequest,
			selection ReproductionSelection, paths RecipientPackageRequest, published PublishedRecipientPackage) error {
			retained++
			require.Equal(t, request.OperationID, gotRequest.OperationID)
			require.Equal(t, inputs.Job.Receipt.SHA256, selection.OriginalReceiptSHA256)
			require.Equal(t, inputs.Reservation.SHA256, selection.OriginalReservationSHA256)
			require.NoError(t, VerifyRecipientArchiveWithQCContext(ctx, paths.ArchivePath, published.QC))
			qc, err := ReadPackageQCReceipt(paths.QCPath)
			require.NoError(t, err)
			require.Equal(t, published.QC, qc)
			_, err = ReadRecipientTransmittal(paths.TransmittalPath)
			require.NoError(t, err)
			if retained == 1 {
				firstQC = qc
				return lostResponse
			}
			secondQC = qc
			return nil
		},
	}
	const profile = "export-dat-opt-images-v1"
	limits := PackageLimits{MaxVolumeBytes: 1000, MaxVolumeDocuments: 10}
	_, err := runtime.Run(t.Context(), inputs.Job.ID, request, policy, profile, limits)
	require.ErrorIs(t, err, lostResponse)
	staging, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, staging)
	result, err := runtime.Run(t.Context(), inputs.Job.ID, request, policy, profile, limits)
	require.NoError(t, err)
	require.Equal(t, 2, retained)
	require.Equal(t, firstQC, secondQC)
	require.Equal(t, secondQC, result.Package.QC)
	require.Equal(t, []string{"ÉX-0001", "ÉX-0002", "ÉX-0003"}, result.Package.QC.PageNumbers)
	require.Equal(t, inputs.Job.Manifest.SHA256, result.Selection.ArtifactManifestSHA256)
	staging, err = os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, staging)
}

func TestReproductionRuntimeRejectsChangedSelectionBeforePackaging(t *testing.T) {
	inputs, opener, request, policy := reproductionFixture(t)
	request.OriginalProductionReceiptSHA256 = testHash("changed receipt")
	retained := false
	runtime := ReproductionRuntime{
		Catalog: packageRuntimeCatalog{inputs}, Opener: opener, StagingDir: t.TempDir(),
		Retain: func(context.Context, documentproduction.ReproductionRequest, ReproductionSelection,
			RecipientPackageRequest, PublishedRecipientPackage) error {
			retained = true
			return nil
		},
	}
	_, err := runtime.Run(t.Context(), inputs.Job.ID, request, policy, "export-dat-opt-images-v1",
		PackageLimits{MaxVolumeBytes: 1000, MaxVolumeDocuments: 10})
	require.ErrorIs(t, err, ErrReproductionConflict)
	require.False(t, retained)
}
