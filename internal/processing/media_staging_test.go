package processing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/media/mediatest"
)

// TestStageMediaRejectsTruncationAndDigestMismatch catches accepting bytes
// whose declared length, digest, or configured staging budget is false.
func TestStageMediaRejectsTruncationAndDigestMismatch(t *testing.T) {
	t.Parallel()
	raw := []byte("synthetic media")
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	var dst bytes.Buffer
	n, err := stageMedia(t.Context(), bytes.NewReader(raw), &dst, int64(len(raw)), 64, digest)
	require.NoError(t, err)
	require.Equal(t, int64(len(raw)), n)
	require.Equal(t, raw, dst.Bytes())

	_, err = stageMedia(t.Context(), bytes.NewReader(raw[:3]), io.Discard, int64(len(raw)), 64, digest)
	require.ErrorContains(t, err, "partial_download")
	_, err = stageMedia(t.Context(), bytes.NewReader(raw), io.Discard, int64(len(raw)), 64, strings.Repeat("0", 64))
	require.ErrorContains(t, err, "digest_mismatch")
	_, err = stageMedia(t.Context(), bytes.NewReader(raw), io.Discard, int64(len(raw)), 3, digest)
	require.ErrorContains(t, err, "byte_limit")
}

func TestMediaArtifactStagingReleasesSharedReservation(t *testing.T) {
	t.Parallel()
	fixture := newPublicationFixture(t)
	raw := mediatest.WAV()
	written, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(raw))
	require.NoError(t, err)
	node, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), "quota.wav",
		written.Hash, written.Size, "audio/wav", processingBlobPhysical(t, written))
	require.NoError(t, err)
	service, err := NewService(ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
		Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(),
		Principal: "operator:quota", MediaMaxBytes: 64})
	require.NoError(t, err)
	firstBytes := bytes.Repeat([]byte("q"), 40)
	firstDigest := sha256.Sum256(firstBytes)
	first, err := service.StageMediaContent(t.Context(), bytes.NewReader(firstBytes), 40, 64,
		hex.EncodeToString(firstDigest[:]))
	require.NoError(t, err)
	secondBytes := bytes.Repeat([]byte("r"), 25)
	secondDigest := sha256.Sum256(secondBytes)
	_, err = service.StageMediaContent(t.Context(), bytes.NewReader(secondBytes), 25, 64,
		hex.EncodeToString(secondDigest[:]))
	require.ErrorContains(t, err, "byte_limit")
	require.NoError(t, first.Close())
	second, err := service.StageMediaContent(t.Context(), bytes.NewReader(secondBytes), 25, 64,
		hex.EncodeToString(secondDigest[:]))
	require.NoError(t, err)
	require.NoError(t, second.Close())
	service.mediaMaxBytes = written.Size
	receipt, err := service.SubmitSuppliedMedia(t.Context(), SuppliedMediaRequest{
		OperationID: "00000000-0000-4000-8000-000000000191", Filename: "quota.wav",
		MediaType: "audio/wav", SHA256: written.Hash, ByteLength: written.Size,
		ExistingContentVersionID: node.CurrentVersionID,
		Occurrence:               MediaOccurrenceInput{Ref: "quota", Revision: "1", Filename: "quota.wav"},
	})
	require.NoError(t, err)
	one := sha256.Sum256([]byte("x"))
	request := MediaArtifactRequest{OperationID: "00000000-0000-4000-8000-000000000192",
		SourceID: receipt.SourceID, OccurrenceID: receipt.OccurrenceID, Kind: "transcript",
		Filename: "quota.txt", MediaType: "text/plain", SHA256: hex.EncodeToString(one[:]), ByteLength: 1}
	request.Content = strings.NewReader("xy")
	_, err = service.ImportRecordingArtifact(t.Context(), request)
	require.ErrorContains(t, err, "byte_limit")
	service.mediaMu.Lock()
	require.Zero(t, service.mediaStagedBytes)
	service.mediaMu.Unlock()

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	request.OperationID = "00000000-0000-4000-8000-000000000193"
	request.Content = strings.NewReader("x")
	_, err = service.ImportRecordingArtifact(canceled, request)
	require.ErrorIs(t, err, context.Canceled)
	service.mediaMu.Lock()
	require.Zero(t, service.mediaStagedBytes)
	service.mediaMu.Unlock()
}
