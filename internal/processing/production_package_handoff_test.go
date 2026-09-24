package processing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

type handoffArtifactOpener struct{ data map[string][]byte }

func (o handoffArtifactOpener) OpenVerifiedProductionArtifact(_ context.Context, _ string,
	artifact documentproduction.Artifact) (packstore.VerifiedReadCloser, int64, error) {
	data, ok := o.data[artifact.ID]
	if !ok {
		return nil, 0, os.ErrNotExist
	}
	return &stagedPackageStream{Reader: bytes.NewReader(data)}, int64(len(data)), nil
}

func handoffHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func handoffPackageFixture(t *testing.T) (store.RetainedProductionPackage, stagedPackageBlobs) {
	t.Helper()
	const (
		jobID       = "77777777-7777-4777-8777-777777777777"
		setID       = "77777777-7777-4777-8777-777777777778"
		memberID    = "77777777-7777-4777-8777-777777777779"
		operationID = "88888888-8888-4888-8888-888888888888"
	)
	var imageBody bytes.Buffer
	page := image.NewRGBA(image.Rect(0, 0, 1, 1))
	page.Set(0, 0, color.RGBA{R: 42, G: 60, B: 80, A: 255})
	require.NoError(t, png.Encode(&imageBody, page))
	parts := []struct {
		role, media string
		page        int
		body        []byte
	}{
		{documentproduction.ArtifactRoleRedactedPDF, "application/pdf", 0,
			[]byte("%PDF-1.4\nsynthetic redacted PDF\n%%EOF\n")},
		{documentproduction.ArtifactRoleRedactedText, "text/plain; charset=utf-8", 0,
			[]byte("synthetic redacted text")},
		{documentproduction.ArtifactRoleRedactedPage, "image/png", 1, imageBody.Bytes()},
	}
	opener := handoffArtifactOpener{data: make(map[string][]byte)}
	manifest := documentproduction.ArtifactManifest{Contract: documentproduction.ArtifactManifestContractV1}
	for index, part := range parts {
		id := "aaaaaaaa-aaaa-4aaa-8aaa-" + []string{"000000000001", "000000000002", "000000000003"}[index]
		manifest.Artifacts = append(manifest.Artifacts, documentproduction.Artifact{
			ID: id, MemberID: memberID, MemberOrdinal: 1, Role: part.role, Page: part.page,
			Path: "private/synthetic-artifact-" + id, SHA256: handoffHash(part.body),
			Size: int64(len(part.body)), MediaType: part.media,
		})
		opener.data[id] = part.body
	}
	var err error
	_, manifest.SHA256, err = documentproduction.CanonicalArtifactManifest(manifest)
	require.NoError(t, err)
	revisionSHA := handoffHash([]byte("synthetic revision"))
	numbers := documentproduction.NumberReservation{
		Contract: documentproduction.NumberReservationContractV1, Authority: "synthetic-ledger",
		ID: "55555555-5555-4555-8555-555555555555", OperationID: jobID,
		RevisionSHA256: revisionSHA, State: "reserved",
		Numbers: []documentproduction.AssignedNumber{{MemberID: memberID, MemberOrdinal: 1, Page: 1, Text: "OUT000001"}},
	}
	_, numbers.SHA256, err = documentproduction.CanonicalNumberReservation(numbers)
	require.NoError(t, err)
	receipt := documentproduction.ProductionReceipt{
		Contract: documentproduction.ProductionReceiptContractV1, ID: jobID, JobID: jobID,
		SetID: setID, Revision: 1, RevisionSHA256: revisionSHA,
		PreparedInputSHA256: handoffHash([]byte("prepared")), PolicySHA256: handoffHash([]byte("policy")),
		NumberReservationSHA256: numbers.SHA256, LayoutSHA256: handoffHash([]byte("layout")),
		EndorsementsSHA256: handoffHash([]byte("endorsements")), ArtifactManifestSHA256: manifest.SHA256,
		CreatedAt: "2026-09-23T12:00:00Z",
	}
	_, receipt.SHA256, err = documentproduction.CanonicalProductionReceipt(receipt)
	require.NoError(t, err)
	job := production.Job{ID: jobID, SetID: setID, Revision: 1, RevisionSHA256: revisionSHA,
		PreparedInputSHA256: receipt.PreparedInputSHA256, State: production.ProductionJobSucceeded,
		Receipt: receipt, Manifest: manifest}
	projection, err := production.PlanPackageProjection(job, numbers,
		[]production.PackageMember{{ID: memberID, Ordinal: 1, FamilyID: "synthetic-family"}},
		"export-dat-pdf-v1", production.PackageLimits{MaxVolumeBytes: 10 << 20, MaxVolumeDocuments: 10})
	require.NoError(t, err)
	dir := t.TempDir()
	archivePath, qcPath, transmittalPath := filepath.Join(dir, "recipient.zip"),
		filepath.Join(dir, "qc.json"), filepath.Join(dir, "transmittal.json")
	qc, err := production.BuildRecipientArchive(t.Context(), projection, jobID, opener, archivePath)
	require.NoError(t, err)
	require.NoError(t, production.PublishPackageQCReceiptContext(t.Context(), archivePath, qcPath, qc))
	require.NoError(t, production.PublishRecipientTransmittalContext(t.Context(), archivePath,
		transmittalPath, projection.Manifest, qc))
	evidence, err := production.BuildPackageEvidenceReceipt(job, projection, qc, operationID)
	require.NoError(t, err)
	record := store.RetainedProductionPackage{Evidence: evidence}
	blobs := stagedPackageBlobs{data: make(map[string][]byte)}
	for _, part := range []struct {
		path    string
		receipt *store.ContentWriteReceipt
	}{
		{archivePath, &record.Archive}, {qcPath, &record.QC}, {transmittalPath, &record.Transmittal},
	} {
		data, readErr := os.ReadFile(part.path)
		require.NoError(t, readErr)
		part.receipt.Version.BlobHash = handoffHash(data)
		part.receipt.Version.Size = int64(len(data))
		blobs.data[part.receipt.Version.BlobHash] = data
	}
	return record, blobs
}

func TestPrepareRetainedProductionPackageDownloadVerifiesHandoffBeforePublication(t *testing.T) {
	record, blobs := handoffPackageFixture(t)
	root := t.TempDir()
	staged, err := PrepareRetainedProductionPackageDownload(t.Context(),
		stagedPackageCatalog{record}, blobs, root, record.Evidence.JobID, record.Evidence.ID)
	require.NoError(t, err)
	require.Equal(t, record.Archive.Version.BlobHash, handoffHash(mustReadHandoffFile(t, staged.ArchivePath)))
	require.NoError(t, staged.Close())
	require.Empty(t, mustReadHandoffDir(t, root))

	changed := record
	changed.Evidence.QCSHA256 = handoffHash([]byte("wrong synthetic QC"))
	changed.Evidence.SHA256 = ""
	raw, err := canonical.Marshal(changed.Evidence)
	require.NoError(t, err)
	changed.Evidence.SHA256 = handoffHash(raw)
	require.NoError(t, production.ValidatePackageEvidenceReceipt(changed.Evidence))
	_, err = PrepareRetainedProductionPackageDownload(t.Context(),
		stagedPackageCatalog{changed}, blobs, root, changed.Evidence.JobID, changed.Evidence.ID)
	require.ErrorIs(t, err, production.ErrPackageEvidence)
	require.Empty(t, mustReadHandoffDir(t, root))
}

func mustReadHandoffFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

func mustReadHandoffDir(t *testing.T, path string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(path)
	require.NoError(t, err)
	return entries
}
