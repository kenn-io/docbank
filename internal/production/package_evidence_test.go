package production

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/canonical"
)

func TestBuildPackageEvidenceReceiptBindsMultiSourceManifest(t *testing.T) {
	job, projection, opener := packageArchiveJobFixture(t, "export-dat-pdf-v1")
	archive := filepath.Join(t.TempDir(), "recipient.zip")
	qc, err := BuildRecipientArchive(t.Context(), projection, job.ID, opener, archive)
	require.NoError(t, err)
	const operationID = "77777777-7777-4777-8777-777777777777"
	receipt, err := BuildPackageEvidenceReceipt(job, projection, qc, operationID)
	require.NoError(t, err)
	require.Equal(t, job.Receipt.SHA256, receipt.ProductionReceiptSHA256)
	require.Equal(t, job.Manifest.SHA256, receipt.ArtifactManifestSHA256)
	require.Equal(t, qc.ManifestSHA256, receipt.RecipientManifestSHA256)
	require.Equal(t, qc.ArchiveSHA256, receipt.ArchiveSHA256)
	require.NoError(t, ValidatePackageEvidenceReceipt(receipt))
	tampered := receipt
	tampered.ArchiveSHA256 = testHash("another archive")
	require.Error(t, ValidatePackageEvidenceReceipt(tampered))
	repeated, err := BuildPackageEvidenceReceipt(job, projection, qc, operationID)
	require.NoError(t, err)
	require.Equal(t, receipt, repeated)
	raw, err := canonical.Marshal(receipt)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "source_version_id")
	require.NotContains(t, string(raw), "member_id")
	require.NotContains(t, string(raw), "private")

	changedQC := qc
	changedQC.ManifestSHA256 = testHash("another manifest")
	_, err = BuildPackageEvidenceReceipt(job, projection, changedQC, operationID)
	require.Error(t, err)
	changedQC = qc
	changedQC.PageNumbers = []string{"wrong page"}
	_, err = BuildPackageEvidenceReceipt(job, projection, changedQC, operationID)
	require.Error(t, err)
	changedQC = qc
	changedQC.Entries = append(append([]PackageQCEntry(nil), qc.Entries...),
		PackageQCEntry{Path: "private/source.json", SHA256: testHash("private entry"), Size: 3})
	_, err = BuildPackageEvidenceReceipt(job, projection, changedQC, operationID)
	require.Error(t, err)
	changedJob := job
	changedJob.Receipt.ArtifactManifestSHA256 = strings.Repeat("0", 64)
	_, err = BuildPackageEvidenceReceipt(changedJob, projection, qc, operationID)
	require.Error(t, err)
}
