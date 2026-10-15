package api_test

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
)

// Routine suites exercise the same complete path with a small generated source.
// DOCBANK_TEST_4GIB_MEDIA=1 runs the exact boundary, serially on the shared host.
func TestMediaUploadThroughSuppliedTranscript(t *testing.T) { //nolint:paralleltest // sets the process-wide memory limit
	size := int64(64 << 10)
	if os.Getenv("DOCBANK_TEST_4GIB_MEDIA") == "1" {
		size = 4 << 30
	}
	previous := debug.SetMemoryLimit(256 << 20)
	defer debug.SetMemoryLimit(previous)
	ts, catalog := newTestServer(t, configureMediaTestService(t, 0))
	client := daemonconn.New(ts.URL, testAPIKey)
	hash := sha256.New()
	_, err := io.Copy(hash, mediatest.SizedWAV(size))
	require.NoError(t, err)
	digest := hex.EncodeToString(hash.Sum(nil))
	metadata := api.MediaSuppliedMetadata{
		OperationID: "00000000-0000-4000-8000-000000000901", Filename: "generated.wav",
		MediaType: "audio/wav", SHA256: digest, ByteLength: size,
		Occurrence: api.MediaOccurrenceBody{Ref: "synthetic-large", Revision: "1", Filename: "generated.wav"},
	}
	retained, err := client.SubmitSuppliedMedia(t.Context(), metadata, mediatest.SizedWAV(size))
	require.NoError(t, err)
	version, err := catalog.ContentVersionByID(t.Context(), retained.ContentVersionID)
	require.NoError(t, err)
	require.Equal(t, size, version.Size)
	require.Equal(t, digest, version.BlobHash)
	transcript := "synthetic ceiling transcript evidence\n"
	input, err := client.ImportMediaArtifact(t.Context(), retained.SourceID, api.MediaArtifactMetadata{
		OperationID: "00000000-0000-4000-8000-000000000902", OccurrenceID: retained.OccurrenceID,
		Kind: "transcript", Filename: "transcript.txt", MediaType: "text/plain",
		SHA256: processingTestHash(transcript), ByteLength: int64(len(transcript)),
	}, strings.NewReader(transcript))
	require.NoError(t, err)
	selector := api.ProcessingSelector{NodeID: version.NodeID, ContentVersionID: version.ID, Profile: processing.SuppliedMediaProfileName}
	plan, err := client.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: selector}})
	require.NoError(t, err)
	require.Empty(t, plan.RenditionIneligibleReason)
	_, err = client.API().GrantDocumentProcessingConsent(t.Context(), &apiclient.GrantDocumentProcessingConsentRequestOptions{Body: &api.ProcessingConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint}})
	require.NoError(t, err)
	queued, err := client.RetryMedia(t.Context(), retained.SourceID, api.MediaRetryBody{
		OperationID: "00000000-0000-4000-8000-000000000903",
		Processing:  &api.MediaProcessingBody{Profile: processing.SuppliedMediaProfileName, SuppliedInputID: input.SuppliedInputID},
	})
	require.NoError(t, err)
	pollInterval := 20 * time.Millisecond
	if size > 1<<30 {
		pollInterval = time.Second
	}
	require.Eventually(t, func() bool {
		status, statusErr := client.MediaStatus(t.Context(), retained.SourceID)
		return statusErr == nil && status.OperationID == queued.OperationID && status.OperationState == "succeeded"
	}, 3*time.Minute, pollInterval)
	evidence, err := client.MediaTranscript(t.Context(), retained.SourceID, retained.SourceVersionID, retained.ContentVersionID)
	require.NoError(t, err)
	require.NotNil(t, evidence.Transcript)
	require.Contains(t, evidence.Transcript.Units[0].Text, "synthetic ceiling transcript")
	// Oversize metadata fails before the file is read, preserving the wire error.
	metadata.OperationID = "00000000-0000-4000-8000-000000000904"
	metadata.ByteLength = (4 << 30) + 1
	_, err = client.SubmitSuppliedMedia(t.Context(), metadata, strings.NewReader("unused"))
	require.ErrorContains(t, err, "byte_limit")
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	require.Less(t, memory.HeapSys, uint64(256<<20))
	t.Logf("uploaded, published, and processed %d source bytes under 256 MiB Go memory limit; heap reservation %d bytes", size, memory.HeapSys)
}

func TestMediaHTTPUsesSmallerConfiguredCeiling(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, func(deps *api.Deps) {
		gate := api.NewOperationGate()
		deps.Gate = gate
		service, err := processing.NewService(processing.ServiceConfig{Catalog: deps.Store, Blobs: deps.Blobs, Gate: gate, SpoolDirectory: filepath.Join(deps.VaultRoot, "blobs", "tmp"), MediaMaxBytes: 64})
		require.NoError(t, err)
		deps.Processing = service
	})
	client := daemonconn.New(ts.URL, testAPIKey)
	raw := mediatest.WAV()
	_, err := client.SubmitSuppliedMedia(t.Context(), api.MediaSuppliedMetadata{
		OperationID: "00000000-0000-4000-8000-000000000905", Filename: "call.wav", MediaType: "audio/wav",
		SHA256: processingTestHash(string(raw)), ByteLength: int64(len(raw)), Occurrence: api.MediaOccurrenceBody{Ref: "small-ceiling", Revision: "1"},
	}, strings.NewReader(string(raw)))
	require.ErrorContains(t, err, "byte_limit")
}

func TestMediaUploadRejectsActualExtraByte(t *testing.T) { //nolint:paralleltest // serializes opt-in 4 GiB staging I/O
	size := int64(64 << 10)
	if os.Getenv("DOCBANK_TEST_4GIB_MEDIA") == "1" {
		size = 4 << 30
	}
	ts, catalog := newTestServer(t, configureMediaTestService(t, 0))
	client := daemonconn.New(ts.URL, testAPIKey)
	_, err := client.SubmitSuppliedMedia(t.Context(), api.MediaSuppliedMetadata{
		OperationID: "00000000-0000-4000-8000-000000000907", Filename: "generated.wav", MediaType: "audio/wav",
		SHA256: processingTestHash("oversize synthetic source"), ByteLength: size, Occurrence: api.MediaOccurrenceBody{Ref: "extra-byte", Revision: "1"},
	}, io.MultiReader(mediatest.SizedWAV(size), strings.NewReader("x")))
	require.ErrorContains(t, err, "byte_limit")
	stages, err := filepath.Glob(filepath.Join(catalog.BlobsDir, "tmp", ".docbank-media-*"))
	require.NoError(t, err)
	require.Empty(t, stages)
	_, total, _, err := catalog.MediaSources(t.Context(), "daemon:operator", "", 10)
	require.NoError(t, err)
	require.Zero(t, total)
	t.Logf("rejected actual %d-byte body and removed staging", size+1)
}
