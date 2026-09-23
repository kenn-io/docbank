package production

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPackageHandoffPublishesExternalTransmittalAndDeliveryReceipt(t *testing.T) {
	projection, opener := packageArchiveFixture(t, "export-dat-opt-images-v1")
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "production.zip")
	qcPath := filepath.Join(dir, "production.qc.json")
	transmittalPath := filepath.Join(dir, "production.transmittal.json")
	deliveryPath := filepath.Join(dir, "production.delivery.json")
	proofPath := filepath.Join(dir, "synthetic-transfer-ack.txt")
	archiveQC, err := BuildRecipientArchive(t.Context(), projection, packageJobID, opener, archivePath)
	require.NoError(t, err)
	archiveBefore, err := os.ReadFile(archivePath)
	require.NoError(t, err)
	require.NoError(t, PublishPackageQCReceipt(archivePath, qcPath, archiveQC))
	require.NoError(t, PublishRecipientTransmittal(archivePath, transmittalPath, projection.Manifest, archiveQC))
	transmittal, err := ReadRecipientTransmittal(transmittalPath)
	require.NoError(t, err)
	require.Equal(t, archiveQC.ArchiveSHA256, transmittal.ArchiveSHA256)
	require.Equal(t, "ÉX-0001", transmittal.Volumes[0].FirstPage)
	require.Equal(t, "ÉX-0003", transmittal.Volumes[0].LastPage)
	require.NoError(t, PublishRecipientTransmittal(archivePath, transmittalPath, projection.Manifest, archiveQC))
	require.NoError(t, os.WriteFile(proofPath, []byte("synthetic transfer acknowledgment"), 0o600))
	policy := PackageDeliveryPolicy{RecipientCode: "synthetic-recipient", AllowedMethods: []string{"secure-transfer"}}
	evidence := PackageDeliveryEvidence{RecipientCode: "synthetic-recipient", Method: "secure-transfer",
		DeliveredAt: "2026-09-23T21:00:00Z", ProofPath: proofPath}
	receipt, err := RecordPackageDelivery(archivePath, qcPath, transmittalPath, deliveryPath, policy, evidence)
	require.NoError(t, err)
	for _, path := range []string{qcPath, transmittalPath, deliveryPath} {
		requirePackagePrivateReadOnlyMode(t, path)
	}
	require.Equal(t, archiveQC.ArchiveSHA256, receipt.ArchiveSHA256)
	require.NotEmpty(t, receipt.ProofSHA256)
	require.NoError(t, VerifyPackageDeliveryReceipt(archivePath, qcPath, transmittalPath, deliveryPath, proofPath, policy))
	retry, err := RecordPackageDelivery(archivePath, qcPath, transmittalPath, deliveryPath, policy, evidence)
	require.NoError(t, err)
	require.Equal(t, receipt, retry)
	archiveAfter, err := os.ReadFile(archivePath)
	require.NoError(t, err)
	require.True(t, bytes.Equal(archiveBefore, archiveAfter))
	for _, path := range []string{transmittalPath, deliveryPath} {
		data, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		for _, secret := range []string{packageJobID, packageOneID, packageTwoID, "private/", "source_sha256", "reason"} {
			require.NotContains(t, string(data), secret)
		}
	}
}

func TestPackageHandoffRejectsStaleQCWrongPolicyAndTamperedEvidence(t *testing.T) {
	projection, opener := packageArchiveFixture(t, "export-dat-pdf-v1")
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "production.zip")
	qcPath := filepath.Join(dir, "production.qc.json")
	transmittalPath := filepath.Join(dir, "production.transmittal.json")
	proofPath := filepath.Join(dir, "synthetic-transfer-ack.txt")
	qc, err := BuildRecipientArchive(t.Context(), projection, packageJobID, opener, archivePath)
	require.NoError(t, err)
	wrong := projection.Manifest
	wrong.Documents = slices.Clone(projection.Manifest.Documents)
	wrong.Documents[0].Control = "changed"
	require.Error(t, PublishRecipientTransmittal(archivePath, transmittalPath, wrong, qc))
	require.NoError(t, PublishPackageQCReceipt(archivePath, qcPath, qc))
	require.NoError(t, PublishRecipientTransmittal(archivePath, transmittalPath, projection.Manifest, qc))
	require.NoError(t, os.WriteFile(proofPath, []byte("synthetic acknowledgment"), 0o600))
	policy := PackageDeliveryPolicy{RecipientCode: "synthetic-recipient", AllowedMethods: []string{"secure-transfer"}}
	evidence := PackageDeliveryEvidence{RecipientCode: "synthetic-recipient", Method: "secure-transfer",
		DeliveredAt: "2026-09-23T21:00:00Z", ProofPath: proofPath}
	falseTransmittal, err := ReadRecipientTransmittal(transmittalPath)
	require.NoError(t, err)
	falseTransmittal.Volumes[0].LastPage = "bogus"
	falseData, err := packageJSON(falseTransmittal)
	require.NoError(t, err)
	falsePath := filepath.Join(dir, "false-transmittal.json")
	require.NoError(t, os.WriteFile(falsePath, falseData, 0o600))
	_, err = RecordPackageDelivery(archivePath, qcPath, falsePath,
		filepath.Join(dir, "false-delivery.json"), policy, evidence)
	require.Error(t, err)
	for _, change := range []struct {
		name     string
		policy   PackageDeliveryPolicy
		evidence PackageDeliveryEvidence
	}{
		{"wrong recipient", PackageDeliveryPolicy{RecipientCode: "other", AllowedMethods: []string{"secure-transfer"}}, evidence},
		{"unapproved method", policy, PackageDeliveryEvidence{RecipientCode: evidence.RecipientCode, Method: "email", DeliveredAt: evidence.DeliveredAt, ProofPath: proofPath}},
		{"missing proof", policy, PackageDeliveryEvidence{RecipientCode: evidence.RecipientCode, Method: evidence.Method, DeliveredAt: evidence.DeliveredAt, ProofPath: filepath.Join(dir, "missing")}},
		{"archive as proof", policy, PackageDeliveryEvidence{RecipientCode: evidence.RecipientCode, Method: evidence.Method, DeliveredAt: evidence.DeliveredAt, ProofPath: archivePath}},
		{"nonutc time", policy, PackageDeliveryEvidence{RecipientCode: evidence.RecipientCode, Method: evidence.Method, DeliveredAt: "2026-09-23T14:00:00-07:00", ProofPath: proofPath}},
	} {
		t.Run(change.name, func(t *testing.T) {
			_, recordErr := RecordPackageDelivery(archivePath, qcPath, transmittalPath,
				filepath.Join(dir, change.name+".json"), change.policy, change.evidence)
			require.Error(t, recordErr)
		})
	}
	deliveryPath := filepath.Join(dir, "delivery.json")
	_, err = RecordPackageDelivery(archivePath, qcPath, transmittalPath, deliveryPath, policy, evidence)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(proofPath, []byte("changed synthetic acknowledgment"), 0o600))
	_, err = RecordPackageDelivery(archivePath, qcPath, transmittalPath, deliveryPath, policy, evidence)
	require.Error(t, err)
	require.Error(t, VerifyPackageDeliveryReceipt(archivePath, qcPath, transmittalPath, deliveryPath, proofPath, policy))
	transmittalData, err := os.ReadFile(transmittalPath)
	require.NoError(t, err)
	transmittalData = bytes.Replace(transmittalData, []byte(`"contract":`), []byte(`"private_reason":"secret","contract":`), 1)
	require.NoError(t, os.Chmod(transmittalPath, 0o600))
	require.NoError(t, os.WriteFile(transmittalPath, transmittalData, 0o600))
	require.Error(t, VerifyPackageDeliveryReceipt(archivePath, qcPath, transmittalPath, deliveryPath, proofPath, policy))
}
