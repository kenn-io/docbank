package production

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPublishPackageQCReceiptIsExternalImmutableAndReverified(t *testing.T) {
	projection, opener := packageArchiveFixture(t, "export-dat-pdf-v1")
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "production.zip")
	qc, err := BuildRecipientArchive(t.Context(), projection, packageJobID, opener, archivePath)
	require.NoError(t, err)
	before, err := os.ReadFile(archivePath)
	require.NoError(t, err)
	receiptPath := filepath.Join(dir, "production.qc.json")
	require.NoError(t, PublishPackageQCReceipt(archivePath, receiptPath, qc))
	requirePackagePrivateReadOnlyMode(t, receiptPath)
	stored, err := ReadPackageQCReceipt(receiptPath)
	require.NoError(t, err)
	require.Equal(t, qc, stored)
	require.NoError(t, VerifyRecipientArchiveWithQC(archivePath, stored))
	after, err := os.ReadFile(archivePath)
	require.NoError(t, err)
	require.True(t, bytes.Equal(before, after))
	require.NoError(t, PublishPackageQCReceipt(archivePath, receiptPath, qc), "same receipt is idempotent")
	wrong := qc
	wrong.ArchiveSHA256 = testHash("wrong archive")
	require.Error(t, PublishPackageQCReceipt(archivePath, receiptPath, wrong))
	changed := append([]byte(nil), before...)
	changed[len(changed)-1] ^= 1
	require.NoError(t, os.Chmod(archivePath, 0o600))
	require.NoError(t, os.WriteFile(archivePath, changed, 0o600))
	require.Error(t, PublishPackageQCReceipt(archivePath, filepath.Join(dir, "changed.qc.json"), qc))
}

func TestReadPackageQCReceiptRejectsChangedOrUnknownFields(t *testing.T) {
	projection, opener := packageArchiveFixture(t, "export-dat-opt-images-v1")
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "production.zip")
	qc, err := BuildRecipientArchive(t.Context(), projection, packageJobID, opener, archivePath)
	require.NoError(t, err)
	receiptPath := filepath.Join(dir, "production.qc.json")
	require.NoError(t, PublishPackageQCReceipt(archivePath, receiptPath, qc))
	data, err := os.ReadFile(receiptPath)
	require.NoError(t, err)
	data = bytes.Replace(data, []byte(`"contract":`), []byte(`"private_reason":"secret","contract":`), 1)
	require.NoError(t, os.Chmod(receiptPath, 0o600))
	require.NoError(t, os.WriteFile(receiptPath, data, 0o600))
	_, err = ReadPackageQCReceipt(receiptPath)
	require.Error(t, err)
}
