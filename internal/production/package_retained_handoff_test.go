package production

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/canonical"
)

func TestVerifyRetainedPackageHandoffBindsPhysicalArchiveAndSidecars(t *testing.T) {
	job, projection, opener := packageArchiveJobFixture(t, "export-dat-pdf-v1")
	dir := t.TempDir()
	archive, qcPath, transmittalPath := filepath.Join(dir, "recipient.zip"),
		filepath.Join(dir, "qc.json"), filepath.Join(dir, "transmittal.json")
	qc, err := BuildRecipientArchive(t.Context(), projection, job.ID, opener, archive)
	require.NoError(t, err)
	require.NoError(t, PublishPackageQCReceiptContext(t.Context(), archive, qcPath, qc))
	require.NoError(t, PublishRecipientTransmittalContext(t.Context(), archive, transmittalPath, projection.Manifest, qc))
	evidence, err := BuildPackageEvidenceReceipt(job, projection, qc,
		"77777777-7777-4777-8777-777777777777")
	require.NoError(t, err)
	require.NoError(t, VerifyRetainedPackageHandoffContext(t.Context(), archive, qcPath, transmittalPath, evidence))
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, VerifyRetainedPackageHandoffContext(canceled, archive, qcPath, transmittalPath, evidence), context.Canceled)
	wrongEvidence := evidence
	wrongEvidence.QCSHA256 = testHash("different QC")
	wrongEvidence.SHA256 = ""
	raw, err := canonical.Marshal(wrongEvidence)
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	wrongEvidence.SHA256 = hex.EncodeToString(sum[:])
	require.NoError(t, ValidatePackageEvidenceReceipt(wrongEvidence))
	require.ErrorIs(t, VerifyRetainedPackageHandoffContext(t.Context(), archive, qcPath, transmittalPath, wrongEvidence), ErrPackageEvidence)

	sidecar, err := os.ReadFile(qcPath)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(qcPath, 0o600))
	require.NoError(t, os.WriteFile(qcPath, append(sidecar, '\n'), 0o600))
	require.ErrorIs(t, VerifyRetainedPackageHandoffContext(t.Context(), archive, qcPath, transmittalPath, evidence), ErrRecipientArchive)
	require.NoError(t, os.WriteFile(qcPath, sidecar, 0o600))
	transmittal, err := os.ReadFile(transmittalPath)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(transmittalPath, 0o600))
	require.NoError(t, os.WriteFile(transmittalPath, append(transmittal, '\n'), 0o600))
	require.ErrorIs(t, VerifyRetainedPackageHandoffContext(t.Context(), archive, qcPath, transmittalPath, evidence), ErrRecipientArchive)
	require.NoError(t, os.WriteFile(transmittalPath, transmittal, 0o600))
	contents, err := os.ReadFile(archive)
	require.NoError(t, err)
	contents[len(contents)-1] ^= 0xff
	require.NoError(t, os.Chmod(archive, 0o600))
	require.NoError(t, os.WriteFile(archive, contents, 0o600))
	require.ErrorIs(t, VerifyRetainedPackageHandoffContext(t.Context(), archive, qcPath, transmittalPath, evidence), ErrRecipientArchive)
}
