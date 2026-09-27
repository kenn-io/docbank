package production

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/kit/packstore"
)

type unverifiedReproductionOpener struct{ syntheticPackageOpener }

type leakingReproductionCatalog struct{}

func (leakingReproductionCatalog) LoadProductionPackageInputs(context.Context, string) (PublishedPackageInputs, error) {
	return PublishedPackageInputs{}, errors.New("private/source/path and private reason")
}

func (o unverifiedReproductionOpener) OpenVerifiedProductionArtifact(_ context.Context, _ string,
	artifact documentproduction.Artifact) (packstore.VerifiedReadCloser, int64, error) {
	data := o.data[artifact.ID]
	return &syntheticVerifiedPDF{Reader: bytes.NewReader(data)}, int64(len(data)), nil
}

func reproductionFixture(t *testing.T) (PublishedPackageInputs, syntheticPackageOpener, documentproduction.ReproductionRequest, PackageDeliveryPolicy) {
	t.Helper()
	job, _, opener := packageArchiveJobFixture(t, "export-dat-opt-images-v1")
	_, numbers, members := packageProjectionFixture(t)
	members[0].SourceVersionID = "66666666-6666-4666-8666-666666666666"
	members[1].SourceVersionID = "77777777-7777-4777-8777-777777777777"
	policy := PackageDeliveryPolicy{RecipientCode: "recipient-one", AllowedMethods: []string{"secure-transfer"}}
	policySHA, err := deliveryPolicyDigest(policy)
	require.NoError(t, err)
	ids := make([]string, len(job.Manifest.Artifacts))
	for i, artifact := range job.Manifest.Artifacts {
		ids[i] = artifact.ID
	}
	return PublishedPackageInputs{Job: job, Reservation: numbers, Members: members}, opener,
		documentproduction.ReproductionRequest{
			Contract:                        documentproduction.ReproductionRequestContractV1,
			OperationID:                     "88888888-8888-4888-8888-888888888888",
			OriginalProductionReceiptSHA256: job.Receipt.SHA256,
			ArtifactIDs:                     ids,
			DeliveryPolicySHA256:            policySHA,
		}, policy
}

func TestPrepareReproductionBindsHistoricalOutputsWithoutNewNumbers(t *testing.T) {
	inputs, opener, request, policy := reproductionFixture(t)
	originalReceipt := inputs.Job.Receipt
	originalManifest := inputs.Job.Manifest
	selection, err := PrepareReproduction(t.Context(), packageRuntimeCatalog{inputs}, opener, inputs.Job.ID, request, policy)
	require.NoError(t, err)
	require.Equal(t, originalReceipt.SHA256, selection.OriginalReceiptSHA256)
	require.Equal(t, inputs.Reservation.SHA256, selection.OriginalReservationSHA256)
	require.Equal(t, originalManifest.SHA256, selection.ArtifactManifestSHA256)
	require.Equal(t, request.ArtifactIDs, selection.ArtifactIDs)
	require.Equal(t, []string{inputs.Members[0].SourceVersionID, inputs.Members[1].SourceVersionID}, selection.SourceVersionIDs)
	require.Equal(t, originalReceipt, inputs.Job.Receipt)
	require.Equal(t, originalManifest, inputs.Job.Manifest)
}

func TestPrepareReproductionRejectsChangedAuthorityAndUnverifiedBytes(t *testing.T) {
	for _, change := range []struct {
		name string
		edit func(*PublishedPackageInputs, *syntheticPackageOpener, *documentproduction.ReproductionRequest, *PackageDeliveryPolicy)
	}{
		{"wrong original receipt", func(_ *PublishedPackageInputs, _ *syntheticPackageOpener, request *documentproduction.ReproductionRequest, _ *PackageDeliveryPolicy) {
			request.OriginalProductionReceiptSHA256 = testHash("other receipt")
		}},
		{"changed delivery policy", func(_ *PublishedPackageInputs, _ *syntheticPackageOpener, _ *documentproduction.ReproductionRequest, policy *PackageDeliveryPolicy) {
			policy.RecipientCode = "different-recipient"
		}},
		{"missing source version", func(inputs *PublishedPackageInputs, _ *syntheticPackageOpener, _ *documentproduction.ReproductionRequest, _ *PackageDeliveryPolicy) {
			inputs.Members[0].SourceVersionID = ""
		}},
		{"missing output", func(inputs *PublishedPackageInputs, opener *syntheticPackageOpener, _ *documentproduction.ReproductionRequest, _ *PackageDeliveryPolicy) {
			delete(opener.data, inputs.Job.Manifest.Artifacts[0].ID)
		}},
		{"tampered text", func(inputs *PublishedPackageInputs, opener *syntheticPackageOpener, _ *documentproduction.ReproductionRequest, _ *PackageDeliveryPolicy) {
			for _, artifact := range inputs.Job.Manifest.Artifacts {
				if artifact.Role == documentproduction.ArtifactRoleRedactedText {
					opener.data[artifact.ID] = []byte("changed redacted text")
					break
				}
			}
		}},
		{"tampered image", func(inputs *PublishedPackageInputs, opener *syntheticPackageOpener, _ *documentproduction.ReproductionRequest, _ *PackageDeliveryPolicy) {
			for _, artifact := range inputs.Job.Manifest.Artifacts {
				if artifact.Role == documentproduction.ArtifactRoleRedactedPage {
					opener.data[artifact.ID] = []byte("changed image")
					break
				}
			}
		}},
		{"omitted artifact", func(_ *PublishedPackageInputs, _ *syntheticPackageOpener, request *documentproduction.ReproductionRequest, _ *PackageDeliveryPolicy) {
			request.ArtifactIDs = slices.Clone(request.ArtifactIDs[1:])
		}},
		{"changed annotation receipt", func(inputs *PublishedPackageInputs, _ *syntheticPackageOpener, _ *documentproduction.ReproductionRequest, _ *PackageDeliveryPolicy) {
			inputs.Job.Receipt.EndorsementsSHA256 = testHash("changed endorsements")
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			inputs, opener, request, policy := reproductionFixture(t)
			change.edit(&inputs, &opener, &request, &policy)
			_, err := PrepareReproduction(t.Context(), packageRuntimeCatalog{inputs}, opener, inputs.Job.ID, request, policy)
			require.Error(t, err)
		})
	}
}

func TestPrepareReproductionRejectsUnverifiedStream(t *testing.T) {
	inputs, opener, request, policy := reproductionFixture(t)
	_, err := PrepareReproduction(t.Context(), packageRuntimeCatalog{inputs},
		unverifiedReproductionOpener{opener}, inputs.Job.ID, request, policy)
	require.ErrorIs(t, err, ErrReproductionConflict)
}

func TestPrepareReproductionSanitizesCatalogErrors(t *testing.T) {
	inputs, opener, request, policy := reproductionFixture(t)
	_, err := PrepareReproduction(t.Context(), leakingReproductionCatalog{}, opener,
		inputs.Job.ID, request, policy)
	require.ErrorIs(t, err, ErrReproductionConflict)
	require.NotContains(t, err.Error(), "private/source/path")
}
