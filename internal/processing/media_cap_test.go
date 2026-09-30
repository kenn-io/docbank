package processing

import (
	"bytes"
	"strconv"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/store"
)

// capCloudRejectedURLs keep the generic URL identity because their scheme,
// host, port, route, escaping, or segment count is not a Cap Cloud share.
var capCloudRejectedURLs = []string{
	"http://cap.so/s/synthcap01",
	"https://cap.so:8443/s/synthcap01",
	"https://app.cap.so/s/synthcap01",
	"https://cap.so.example/s/synthcap01",
	"https://cap.so./s/synthcap01",
	"https://example.com/s/synthcap01",
	"https://cap.so/s/",
	"https://cap.so/s/synthcap01/extra",
	"https://cap.so/s/synth%63ap01",
	"https://cap.so/",
}

func TestCapCloudRecording(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ raw, videoID string }{
		{"https://cap.so/s/synthcap01", "synthcap01"},
		{"https://www.cap.so/embed/synthcap01", "synthcap01"},
		{"https://cap.so/dev/synthcap01", "synthcap01"},
		{"https://cap.so/embed/synthcap01?sdk=1", "synthcap01"},
	} {
		t.Run(test.raw, func(t *testing.T) {
			canonicalURL, _, err := canonicalRemoteRecordingReference(test.raw)
			require.NoError(t, err)
			videoID, ok := capCloudRecording(canonicalURL)
			require.True(t, ok)
			require.Equal(t, test.videoID, videoID)
		})
	}
	for _, raw := range capCloudRejectedURLs {
		t.Run(raw, func(t *testing.T) {
			canonicalURL, _, err := canonicalRemoteRecordingReference(raw)
			require.NoError(t, err)
			videoID, ok := capCloudRecording(canonicalURL)
			require.False(t, ok)
			require.Empty(t, videoID)
		})
	}
}

// Cap documents no download route for received links, so an acquisition
// request retains the reference for the manual exact-file import path.
func TestCapCloudAcquireRetainsUnsupportedReference(t *testing.T) {
	t.Parallel()
	fixture := newPublicationFixture(t)
	service := newRemoteRecordingTestService(t, fixture, "operator:cap", 0, nil)
	request := RemoteRecordingRequest{
		OperationID:  uuid.New().String(),
		ReferenceURL: "https://cap.so/s/synthcap01?t=private-synthetic",
		CanonicalURL: "https://cap.so/s/synthcap01", Acquire: true,
		Occurrence: MediaOccurrenceInput{Ref: "cap-a", Revision: "1", Filename: "cap.wav"},
	}
	retained, err := service.SubmitRemoteRecording(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, "unsupported", retained.Outcome)
	require.Equal(t, "unprocessed", retained.CoverageState)
	require.Equal(t, "succeeded", retained.OperationState)
	require.NotEmpty(t, retained.OccurrenceID)
	require.Empty(t, retained.SourceVersionID)
	require.Empty(t, retained.ContentVersionID)

	status, err := service.MediaStatus(t.Context(), retained.SourceID)
	require.NoError(t, err)
	require.Equal(t, "unsupported", status.Outcome)
	require.Empty(t, status.ContentVersionID)

	replayed, err := service.SubmitRemoteRecording(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, retained, replayed)
	changed := request
	changed.Acquire = false
	_, err = service.SubmitRemoteRecording(t.Context(), changed)
	require.ErrorIs(t, err, store.ErrMediaOperationConflict)

	var metadata bytes.Buffer
	require.NoError(t, fixture.catalog.ExportMetadata(t.Context(), &metadata))
	require.Contains(t, metadata.String(), `"provider":"cap"`)
	for _, private := range []string{"synthcap01", "cap.so", "private-synthetic"} {
		require.NotContains(t, metadata.String(), private)
	}

	raw := mediatest.WAV()
	imported, err := service.ImportRecordingArtifact(t.Context(), remoteRecordingTestArtifact(
		uuid.New().String(), retained.SourceID, retained.OccurrenceID, "cap.wav", "audio/wav",
		processingSHA256(raw), int64(len(raw)), raw))
	require.NoError(t, err)
	require.Equal(t, "content_available", imported.Outcome)
	require.Equal(t, retained.SourceID, imported.SourceID)
	require.Equal(t, retained.OccurrenceID, imported.OccurrenceID)
	require.NotEmpty(t, imported.ContentVersionID)
}

func TestCapCloudRecordingIdentity(t *testing.T) {
	t.Parallel()
	fixture := newPublicationFixture(t)
	service := newRemoteRecordingTestService(t, fixture, "operator:cap-identity", 0, nil)
	submit := func(t *testing.T, canonicalURL, ref string, acquire bool) (MediaReceipt, error) {
		t.Helper()
		return service.SubmitRemoteRecording(t.Context(), RemoteRecordingRequest{
			OperationID: uuid.New().String(), ReferenceURL: canonicalURL, CanonicalURL: canonicalURL,
			Acquire:    acquire,
			Occurrence: MediaOccurrenceInput{Ref: ref, Revision: "1", Filename: "cap.wav"},
		})
	}

	var sourceID string
	occurrences := map[string]bool{}
	for index, raw := range []string{
		"https://cap.so/s/synthcap02",
		"HTTPS://WWW.CAP.SO:443/s/synthcap02#t=5",
		"https://cap.so/dev/synthcap02",
		"https://cap.so/embed/synthcap02?sdk=1",
	} {
		receipt, err := submit(t, raw, []string{"a", "b", "c", "d"}[index], false)
		require.NoError(t, err)
		if sourceID == "" {
			sourceID = receipt.SourceID
		}
		require.Equal(t, sourceID, receipt.SourceID, raw)
		occurrences[receipt.OccurrenceID] = true
	}
	require.Len(t, occurrences, 4)
	for index, raw := range []string{"https://cap.so/s/SynthCap02", "https://cap.so/s/synthcap03"} {
		receipt, err := submit(t, raw, "distinct-"+strconv.Itoa(index), false)
		require.NoError(t, err)
		require.NotEqual(t, sourceID, receipt.SourceID, raw)
	}
	recognized, err := submit(t, "https://cap.so/s/synthcap01", "recognized", false)
	require.NoError(t, err)

	for index, raw := range capCloudRejectedURLs {
		t.Run(raw, func(t *testing.T) {
			_, err := submit(t, raw, "generic-acquire-"+strconv.Itoa(index), true)
			require.ErrorIs(t, err, ErrMediaCapabilityUnavailable)
			generic, err := submit(t, raw, "generic-"+strconv.Itoa(index), false)
			require.NoError(t, err)
			require.Equal(t, "unsupported", generic.Outcome)
			require.NotEqual(t, sourceID, generic.SourceID)
			require.NotEqual(t, recognized.SourceID, generic.SourceID)
		})
	}
	var metadata bytes.Buffer
	require.NoError(t, fixture.catalog.ExportMetadata(t.Context(), &metadata))
	require.Contains(t, metadata.String(), `"provider":"url"`)
	require.Contains(t, metadata.String(), `"provider":"cap"`)
}
