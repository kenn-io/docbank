package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/production"
)

// PublishedProductionPackageHTTPFixture supplies the external Store test
// package with a real published job and three retained vault versions. The
// source, render, numbering, archive, and retention paths all run before the
// HTTP server is opened against the same vault root.
func PublishedProductionPackageHTTPFixture(t *testing.T) (*Store, string, production.Job,
	RetainedProductionPackage) {
	t.Helper()
	f, job := publishedRealRetentionFixture(t)
	dir := t.TempDir()
	request := production.RecipientPackageRequest{
		JobID: job.ID, ProfileID: "export-dat-opt-images-v1",
		Limits:          production.PackageLimits{MaxVolumeBytes: 50 << 20, MaxVolumeDocuments: 10},
		ArchivePath:     filepath.Join(dir, "recipient.zip"),
		QCPath:          filepath.Join(dir, "qc.json"),
		TransmittalPath: filepath.Join(dir, "transmittal.json"),
	}
	_, err := production.PublishRecipientPackage(t.Context(), f.Store, f, request)
	require.NoError(t, err)
	const operationID = "77777777-7777-4777-8777-777777777777"
	retained, err := f.RetainProductionPackage(t.Context(), job.ID, operationID,
		request.ProfileID, request.Limits, request.ArchivePath, request.QCPath,
		request.TransmittalPath, restartPackageBlobWriter(f))
	require.NoError(t, err)
	f.reopen(t)
	return f.Store, f.root, job, retained
}

// PreparedProductionPackageHTTPFixture supplies a published synthetic job
// before any recipient package is retained by the daemon.
func PreparedProductionPackageHTTPFixture(t *testing.T) (*Store, string, string) {
	t.Helper()
	f, job := publishedRealRetentionFixture(t)
	f.reopen(t)
	return f.Store, f.root, job.ID
}

// PublishedProductionSupplementHTTPFixture supplies a real published parent
// and separately prepared child for the external HTTP route test.
func PublishedProductionSupplementHTTPFixture(t *testing.T) (*Store, string, production.SupplementRequest) {
	t.Helper()
	f, parent := publishedRealRetentionFixture(t)
	child := supplementChildFixture(t, f, parent)
	return f.Store, f.root, production.SupplementRequest{
		OperationID: "78000000-0000-4000-8000-000000000019",
		ParentJobID: parent.ID, JobID: child.ID,
		ParentReceiptSHA256: parent.Receipt.SHA256,
		PreparedSHA256:      child.RevisionSHA256, PreparedInputSHA256: child.PreparedInputSHA256,
	}
}

// PublishedProductionReproductionHTTPFixture supplies a retained verified
// reproduction and its original job for exact external API readback.
func PublishedProductionReproductionHTTPFixture(t *testing.T) (*Store, string, string,
	documentproduction.ReproductionReceipt) {
	t.Helper()
	f, job := publishedRealRetentionFixture(t)
	inputs, err := f.LoadProductionPackageInputs(t.Context(), job.ID)
	require.NoError(t, err)
	artifactIDs := make([]string, len(job.Manifest.Artifacts))
	for i, artifact := range job.Manifest.Artifacts {
		artifactIDs[i] = artifact.ID
	}
	sourceVersionIDs := make([]string, len(inputs.Members))
	for i, member := range inputs.Members {
		sourceVersionIDs[i] = member.SourceVersionID
	}
	policy := production.PackageDeliveryPolicy{RecipientCode: "synthetic-recipient",
		AllowedMethods: []string{"offline-media"}}
	policyRaw, err := canonical.Marshal(policy)
	require.NoError(t, err)
	request := documentproduction.ReproductionRequest{
		Contract:                        documentproduction.ReproductionRequestContractV1,
		OperationID:                     "88000000-0000-4000-8000-000000000018",
		OriginalProductionReceiptSHA256: job.Receipt.SHA256,
		ArtifactIDs:                     artifactIDs, SourceVersionIDs: sourceVersionIDs,
		DeliveryPolicySHA256: productionHash(string(policyRaw)),
	}
	dir := t.TempDir()
	paths := production.RecipientPackageRequest{
		JobID: job.ID, ProfileID: "export-dat-opt-images-v1",
		Limits:          production.PackageLimits{MaxVolumeBytes: 50 << 20, MaxVolumeDocuments: 10},
		ArchivePath:     filepath.Join(dir, "recipient.zip"),
		QCPath:          filepath.Join(dir, "qc.json"),
		TransmittalPath: filepath.Join(dir, "transmittal.json"),
	}
	_, err = production.PublishRecipientPackage(t.Context(), f.Store, f, paths)
	require.NoError(t, err)
	receipt, err := f.RetainProductionReproduction(t.Context(), job.ID, request, policy,
		paths, f, restartPackageBlobWriter(f))
	require.NoError(t, err)
	f.reopen(t)
	return f.Store, f.root, job.ID, receipt
}

// PreparedProductionReproductionHTTPFixture supplies a published original
// production and exact synthetic selection for daemon-side package creation.
func PreparedProductionReproductionHTTPFixture(t *testing.T) (*Store, string, string,
	documentproduction.ReproductionRequest, production.PackageDeliveryPolicy) {
	t.Helper()
	f, job := publishedRealRetentionFixture(t)
	inputs, err := f.LoadProductionPackageInputs(t.Context(), job.ID)
	require.NoError(t, err)
	artifactIDs := make([]string, len(job.Manifest.Artifacts))
	for i, artifact := range job.Manifest.Artifacts {
		artifactIDs[i] = artifact.ID
	}
	sourceVersionIDs := make([]string, len(inputs.Members))
	for i, member := range inputs.Members {
		sourceVersionIDs[i] = member.SourceVersionID
	}
	policy := production.PackageDeliveryPolicy{RecipientCode: "synthetic-recipient",
		AllowedMethods: []string{"offline-media"}}
	policySHA256, err := production.PackageDeliveryPolicySHA256(policy)
	require.NoError(t, err)
	request := documentproduction.ReproductionRequest{
		Contract:                        documentproduction.ReproductionRequestContractV1,
		OperationID:                     "88000000-0000-4000-8000-000000000021",
		OriginalProductionReceiptSHA256: job.Receipt.SHA256,
		ArtifactIDs:                     artifactIDs, SourceVersionIDs: sourceVersionIDs,
		DeliveryPolicySHA256: policySHA256,
	}
	f.reopen(t)
	return f.Store, f.root, job.ID, request, policy
}
