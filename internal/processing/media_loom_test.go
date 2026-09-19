package processing

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/store"
)

func TestLoomRecording(t *testing.T) {
	accepted := []string{
		"https://www.loom.com/share/synthloom01",
		"https://loom.com/share/synthloom01?sid=synthetic",
		"https://www.loom.com/embed/synthloom01?hide_owner=true#t=5",
		"HTTPS://WWW.LOOM.COM:443/share/synthloom01",
	}
	for _, raw := range accepted {
		canonicalURL, _, err := canonicalRemoteRecordingReference(raw)
		require.NoError(t, err)
		id, ok := loomRecording(canonicalURL)
		assert.True(t, ok, raw)
		assert.NotEmpty(t, id, raw)
	}
	for _, raw := range []string{
		"http://www.loom.com/share/synthloom01", "https://loom.com:8443/share/synthloom01",
		"https://app.loom.com/share/synthloom01", "https://loom.com.example/share/synthloom01",
		"https://www.loom.com./share/synthloom01", "https://www.loom.com/share/",
		"https://www.loom.com/share/x/extra", "https://www.loom.com/share/synth%6Coom01",
		"https://www.loom.com/v/synthloom01", "https://www.loom.com/",
		"https://example.com/share/synthloom01",
	} {
		id, ok := loomRecording(raw)
		assert.False(t, ok, raw)
		assert.Empty(t, id, raw)
	}
}

func TestLoomAcquireRetainsUnsupportedReference(t *testing.T) {
	fixture := newPublicationFixture(t)
	service := newRemoteRecordingTestService(t, fixture, "operator:loom", 0, nil)
	request := RemoteRecordingRequest{
		OperationID: uuid.New().String(), ReferenceURL: "https://private.invalid/loom",
		CanonicalURL: "HTTPS://WWW.LOOM.COM:443/share/synthloom01?sid=private#ignored", Acquire: true,
		Occurrence: MediaOccurrenceInput{Ref: "loom", Revision: "1", Filename: "loom.mp4"},
	}
	receipt, err := service.SubmitRemoteRecording(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, "unsupported", receipt.Outcome)
	assert.Empty(t, receipt.ContentVersionID)
	status, err := service.MediaStatus(t.Context(), receipt.SourceID)
	require.NoError(t, err)
	assert.Equal(t, "unprocessed", status.CoverageState)
}

func TestLoomRecordingIdentity(t *testing.T) {
	fixture := newPublicationFixture(t)
	service := newRemoteRecordingTestService(t, fixture, "operator:loom-identity", 0, nil)
	request := func(operationID, canonicalURL, ref string) RemoteRecordingRequest {
		return RemoteRecordingRequest{OperationID: operationID, ReferenceURL: "https://private.invalid/" + ref,
			CanonicalURL: canonicalURL, Occurrence: MediaOccurrenceInput{Ref: ref, Revision: "1", Filename: "loom.mp4"}}
	}
	first, err := service.SubmitRemoteRecording(t.Context(), request(uuid.New().String(),
		"https://loom.com/share/synthloom01?sid=a", "first"))
	require.NoError(t, err)
	second, err := service.SubmitRemoteRecording(t.Context(), request(uuid.New().String(),
		"https://www.loom.com/share/synthloom01#t=2", "second"))
	require.NoError(t, err)
	assert.Equal(t, first.SourceID, second.SourceID)
	embed, err := service.SubmitRemoteRecording(t.Context(), request(uuid.New().String(),
		"https://www.loom.com/embed/synthloom01", "embed"))
	require.NoError(t, err)
	assert.NotEqual(t, first.SourceID, embed.SourceID)
	changed, err := service.SubmitRemoteRecording(t.Context(), request(uuid.New().String(),
		"https://www.loom.com/share/Synthloom01", "case"))
	require.NoError(t, err)
	assert.NotEqual(t, first.SourceID, changed.SourceID)
}

func TestLoomRecognitionPreservesLegacyURLIdentity(t *testing.T) {
	fixture := newPublicationFixture(t)
	service := newRemoteRecordingTestService(t, fixture, "operator:legacy-loom", 0, nil)
	canonicalURL := "https://www.loom.com/share/legacyloom"
	_, requestSHA, err := remoteRecordingOperationDigest(RemoteRecordingRequest{
		ReferenceURL: "https://private.invalid/legacy", CanonicalURL: canonicalURL,
		Occurrence: MediaOccurrenceInput{Ref: "legacy", Revision: "1", Filename: "legacy.wav"},
	})
	require.NoError(t, err)
	legacySource, err := store.MediaSourceKey("remote_recording", fixture.catalog.VaultID(), "url",
		hashMediaPrivateValue("https://www.loom.com"), hashMediaPrivateValue(canonicalURL))
	require.NoError(t, err)
	operation := store.MediaOperation{ID: uuid.New().String(), Principal: service.principal,
		Verb: "submit_remote_recording", RequestSHA256: requestSHA, SourceID: legacySource}
	_, err = fixture.catalog.RetainMediaReference(t.Context(), store.MediaReferencePublicationRequest{
		Operation: operation, Provider: "url", OriginScope: hashMediaPrivateValue("https://www.loom.com"),
		IdentitySHA256: legacySource, Outcome: "unsupported",
		Occurrence: store.MediaOccurrenceInput{ID: uuid.New().String(), SourceID: legacySource,
			Principal: service.principal, Ref: "legacy", Revision: "1", Filename: "legacy.wav", MessageJSON: "{}"},
	})
	require.NoError(t, err)
	request := RemoteRecordingRequest{OperationID: operation.ID, ReferenceURL: "https://private.invalid/legacy",
		CanonicalURL: canonicalURL, Occurrence: MediaOccurrenceInput{Ref: "legacy", Revision: "1", Filename: "legacy.wav"}}
	replayed, err := service.SubmitRemoteRecording(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, legacySource, replayed.SourceID)
	newRequest := request
	newRequest.OperationID = uuid.New().String()
	newRequest.Occurrence.Ref = "legacy-new"
	newReceipt, err := service.SubmitRemoteRecording(t.Context(), newRequest)
	require.NoError(t, err)
	assert.NotEqual(t, legacySource, newReceipt.SourceID)
}

func TestRemoteRecordingVideoPolicy(t *testing.T) {
	video := remoteRecordingInspectionPolicy("loom.mp4", "video/mp4", strings.Repeat("a", 64), 123,
		40<<20, true)
	assert.Equal(t, int64(20<<20), video.MaxSourceBytes)
	assert.Equal(t, int64(1920*1088), video.MaxPixels)
	assert.Equal(t, int64(300_000), video.MaxDurationMS)
	assert.Equal(t, int64(18_000), video.MaxFrames)
	t.Log("limits: 20 MiB, 1920x1088, 300000 ms, 18000 frames")
	audio := mediaInspectionPolicyForFile("loom.wav", "audio/wav", strings.Repeat("a", 64), 123, 40<<20)
	assert.Equal(t, audio, remoteRecordingInspectionPolicy("loom.wav", "audio/wav", strings.Repeat("a", 64), 123, 40<<20, false))
}

func TestRemoteRecordingVideoAdmission(t *testing.T) {
	fixture := newPublicationFixture(t)
	service := newRemoteRecordingTestService(t, fixture, "operator:loom-video", 0, nil)
	remote := loomRemoteForTest(t, service, "video")
	content := mediatest.H264AACMP4()
	digest := processingSHA256(content)
	accepted, err := service.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: uuid.New().String(), SourceID: remote.SourceID, OccurrenceID: remote.OccurrenceID,
		Kind: "media", Origin: "supplied", Provider: "loom", Filename: "loom.mp4", MediaType: "video/mp4",
		SHA256: digest, ByteLength: int64(len(content)), Content: bytes.NewReader(content),
	})
	require.NoError(t, err)
	assert.Equal(t, "content_available", accepted.Outcome)
	assert.NotEmpty(t, accepted.ContentVersionID)
	node, err := fixture.catalog.NodeByPath(t.Context(), "/media/"+remote.SourceID+"/"+digest+".mp4")
	require.NoError(t, err)
	assert.Equal(t, accepted.ContentVersionID, node.CurrentVersionID)

	for name, data := range map[string][]byte{"long": mediatest.H264LongMP4(), "wide": mediatest.H264WideMP4()} {
		t.Run(name, func(t *testing.T) {
			other := loomRemoteForTest(t, service, name)
			hash := processingSHA256(data)
			_, err := service.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
				OperationID: uuid.New().String(), SourceID: other.SourceID, OccurrenceID: other.OccurrenceID,
				Kind: "media", Origin: "supplied", Filename: name + ".mp4", MediaType: "video/mp4",
				SHA256: hash, ByteLength: int64(len(data)), Content: bytes.NewReader(data),
			})
			require.ErrorContains(t, err, "unqualified_codec: visual_bounds_exceeded")
			status, statusErr := service.MediaStatus(t.Context(), other.SourceID)
			require.NoError(t, statusErr)
			assert.Empty(t, status.ContentVersionID)
		})
	}

	bad := []struct {
		name, filename, mediaType string
		data                      []byte
	}{
		{"wrong type", "loom.mp4", "audio/mpeg", content},
		{"quicktime", "loom.mov", "video/quicktime", content},
		{"wrong extension", "loom.wav", "audio/wav", content},
	}
	for _, test := range bad {
		t.Run(test.name, func(t *testing.T) {
			other := loomRemoteForTest(t, service, test.name)
			hash := processingSHA256(test.data)
			_, err := service.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
				OperationID: uuid.New().String(), SourceID: other.SourceID, OccurrenceID: other.OccurrenceID,
				Kind: "media", Origin: "supplied", Filename: test.filename, MediaType: test.mediaType,
				SHA256: hash, ByteLength: int64(len(test.data)), Content: bytes.NewReader(test.data),
			})
			require.Error(t, err)
		})
	}
	noAuthority := loomRemoteForTest(t, service, "no-authority")
	noAuthorityBytes := mediatest.MP4(64, 48, 1000)
	_, err = service.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: uuid.New().String(), SourceID: noAuthority.SourceID, OccurrenceID: noAuthority.OccurrenceID,
		Kind: "media", Origin: "supplied", Filename: "no-authority.mp4", MediaType: "video/mp4",
		SHA256: processingSHA256(noAuthorityBytes), ByteLength: int64(len(noAuthorityBytes)),
		Content: bytes.NewReader(noAuthorityBytes),
	})
	require.Error(t, err)

	revoked := loomRemoteForTest(t, service, "revoked-video")
	revokedHash := processingSHA256(content)
	_, err = service.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: uuid.New().String(), SourceID: revoked.SourceID, OccurrenceID: revoked.OccurrenceID,
		Kind: "media", Origin: "supplied", Filename: "revoked-video.mp4", MediaType: "video/mp4",
		SHA256: revokedHash, ByteLength: int64(len(content)), Content: bytes.NewReader(content),
	})
	require.NoError(t, err)
	_, err = service.RevokeMediaOccurrence(t.Context(), uuid.New().String(), revoked.OccurrenceID, "1")
	require.NoError(t, err)
	_, err = service.MediaStatus(t.Context(), revoked.SourceID)
	require.ErrorIs(t, err, store.ErrNotFound)
	racing := loomRemoteForTest(t, service, "racing-video")
	raceCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var wait sync.WaitGroup
	importErr := make(chan error, 1)
	wait.Go(func() {
		_, importErrValue := service.ImportRecordingArtifact(raceCtx, MediaArtifactRequest{
			OperationID: uuid.New().String(), SourceID: racing.SourceID, OccurrenceID: racing.OccurrenceID,
			Kind: "media", Origin: "supplied", Filename: "racing-video.mp4", MediaType: "video/mp4",
			SHA256: digest, ByteLength: int64(len(content)), Content: bytes.NewReader(content),
		})
		importErr <- importErrValue
	})
	revokeErr := make(chan error, 1)
	wait.Go(func() {
		_, revokeErrValue := service.RevokeMediaOccurrence(raceCtx, uuid.New().String(), racing.OccurrenceID, "1")
		revokeErr <- revokeErrValue
	})
	wait.Wait()
	require.NoError(t, <-revokeErr)
	importResult := <-importErr
	if importResult != nil {
		require.ErrorIs(t, importResult, store.ErrNotFound)
	}

	_, err = service.SubmitSuppliedMedia(t.Context(), SuppliedMediaRequest{
		OperationID: uuid.New().String(), Content: bytes.NewReader(content), Filename: "loom.mp4",
		MediaType: "video/mp4", SHA256: digest, ByteLength: int64(len(content)),
		Occurrence: MediaOccurrenceInput{Ref: "supplied-video", Revision: "1", Filename: "loom.mp4"},
	})
	require.ErrorContains(t, err, "supplied media requires a WAV or MP3 filename and media type")

	counting := &countingReader{}
	_, err = service.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: uuid.New().String(), SourceID: remote.SourceID, OccurrenceID: remote.OccurrenceID,
		Kind: "media", Origin: "supplied", Filename: "oversized.mp4", MediaType: "video/mp4",
		SHA256: strings.Repeat("b", 64), ByteLength: remoteVideoMaxBytes + 1, Content: counting,
	})
	require.EqualError(t, err, "byte_limit")
	assert.Zero(t, counting.reads)
}

func TestSuppliedCaptionBindingsStayBySource(t *testing.T) {
	fixture := newPublicationFixture(t)
	service := newRemoteRecordingTestService(t, fixture, "operator:loom-caption", 0, nil)
	first := loomRemoteForTest(t, service, "caption-a")
	second := loomRemoteForTest(t, service, "caption-b")
	content := mediatest.H264AACMP4()
	mediaReceipts := make([]MediaReceipt, 2)
	captionIDs := make([]string, 2)
	var err error
	for index, remote := range []MediaReceipt{first, second} {
		hash := processingSHA256(content)
		mediaReceipts[index], err = service.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
			OperationID: uuid.New().String(), SourceID: remote.SourceID, OccurrenceID: remote.OccurrenceID,
			Kind: "media", Origin: "supplied", Filename: "loom.mp4", MediaType: "video/mp4",
			SHA256: hash, ByteLength: int64(len(content)), Content: bytes.NewReader(content),
		})
		require.NoError(t, err)
	}
	for index, remote := range []MediaReceipt{first, second} {
		srt := []byte("1\n00:00:00,000 --> 00:00:01,000\ncaption-" + string(rune('a'+index)))
		receipt, err := service.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
			OperationID: uuid.New().String(), SourceID: remote.SourceID, OccurrenceID: remote.OccurrenceID,
			Kind: "caption", Origin: "supplied", Provider: "loom", Filename: "loom.srt",
			MediaType: "application/x-subrip", SHA256: processingSHA256(srt), ByteLength: int64(len(srt)), Content: bytes.NewReader(srt),
		})
		require.NoError(t, err)
		captionIDs[index] = receipt.SuppliedInputID
		sourceID, sourceVersionID, err := fixture.catalog.MediaSourceBindingForContentVersion(
			t.Context(), service.principal, mediaReceipts[index].ContentVersionID)
		require.NoError(t, err)
		bound, err := service.resolveMediaInputBinding(t.Context(), SuppliedCaptionProfileName,
			processingSHA256(content), mediaSourceBinding{sourceID: sourceID, sourceVersionID: sourceVersionID}, "")
		require.NoError(t, err)
		assert.Equal(t, receipt.SuppliedInputID, bound)
	}
	transcript := []byte("legacy transcript")
	transcriptReceipt, err := service.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: uuid.New().String(), SourceID: first.SourceID, OccurrenceID: first.OccurrenceID,
		Kind: "transcript", Origin: "supplied", Filename: "loom.txt", MediaType: "text/plain",
		SHA256: processingSHA256(transcript), ByteLength: int64(len(transcript)), Content: bytes.NewReader(transcript),
	})
	require.NoError(t, err)
	_, err = service.resolveMediaInputBinding(t.Context(), SuppliedMediaProfileName,
		processingSHA256(content), mediaSourceBinding{sourceID: first.SourceID, sourceVersionID: mediaReceipts[0].SourceVersionID},
		transcriptReceipt.SuppliedInputID)
	require.NoError(t, err)
	_, err = service.resolveMediaInputBinding(t.Context(), SuppliedCaptionProfileName,
		processingSHA256(content), mediaSourceBinding{sourceID: first.SourceID, sourceVersionID: mediaReceipts[0].SourceVersionID},
		transcriptReceipt.SuppliedInputID)
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = service.RevokeMediaOccurrence(t.Context(), uuid.New().String(), first.OccurrenceID, "1")
	require.NoError(t, err)
	_, err = service.resolveMediaInputBinding(t.Context(), SuppliedCaptionProfileName,
		processingSHA256(content), mediaSourceBinding{sourceID: first.SourceID, sourceVersionID: mediaReceipts[0].SourceVersionID},
		captionIDs[0])
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = service.resolveMediaInputBinding(t.Context(), SuppliedCaptionProfileName,
		processingSHA256(content), mediaSourceBinding{sourceID: second.SourceID, sourceVersionID: mediaReceipts[1].SourceVersionID},
		captionIDs[1])
	require.NoError(t, err)
}

func TestSuppliedCaptionInvalidBytesAreMalformed(t *testing.T) {
	fixture := newPublicationFixture(t)
	service := newRemoteRecordingTestService(t, fixture, "operator:loom-invalid-caption", 0, nil)
	remote := loomRemoteForTest(t, service, "invalid-caption")
	video := mediatest.H264AACMP4()
	videoHash := processingSHA256(video)
	_, err := service.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: uuid.New().String(), SourceID: remote.SourceID, OccurrenceID: remote.OccurrenceID,
		Kind: "media", Origin: "supplied", Provider: "loom", Filename: "loom.mp4", MediaType: "video/mp4",
		SHA256: videoHash, ByteLength: int64(len(video)), Content: bytes.NewReader(video),
	})
	require.NoError(t, err)
	invalid := []byte("1\n00:00:00,000 --> 00:00:01,000\ncaf\xe9\n")
	caption, err := service.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: uuid.New().String(), SourceID: remote.SourceID, OccurrenceID: remote.OccurrenceID,
		Kind: "caption", Origin: "supplied", Provider: "loom", Filename: "loom.srt",
		MediaType: "application/x-subrip", SHA256: processingSHA256(invalid),
		ByteLength: int64(len(invalid)), Content: bytes.NewReader(invalid),
	})
	require.NoError(t, err)
	source := retainedTranscriptSource{catalog: fixture.catalog, blobs: fixture.blobs, principal: service.principal}
	_, err = source.CaptionForBinding(t.Context(), videoHash, caption.SuppliedInputID)
	var providerErr *document.RenditionProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, document.RenditionErrorMalformedEvidence, providerErr.Code())
}

func TestLoomCaptionProcessingRequiresConsentAndNoEgress(t *testing.T) {
	fixture := newPublicationFixture(t)
	base := newRemoteRecordingTestService(t, fixture, "operator:loom-consent", 0, nil)
	remote := loomRemoteForTest(t, base, "consent")
	video := mediatest.H264AACMP4()
	videoHash := processingSHA256(video)
	videoReceipt, err := base.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: uuid.New().String(), SourceID: remote.SourceID, OccurrenceID: remote.OccurrenceID,
		Kind: "media", Origin: "supplied", Provider: "loom", Filename: "loom.mp4", MediaType: "video/mp4",
		SHA256: videoHash, ByteLength: int64(len(video)), Content: bytes.NewReader(video),
	})
	require.NoError(t, err)
	srt := []byte("1\n00:00:00,000 --> 00:00:01,000\nconsent cue")
	caption, err := base.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: uuid.New().String(), SourceID: remote.SourceID, OccurrenceID: remote.OccurrenceID,
		Kind: "caption", Origin: "supplied", Provider: "loom", Filename: "loom.srt",
		MediaType: "application/x-subrip", SHA256: processingSHA256(srt), ByteLength: int64(len(srt)), Content: bytes.NewReader(srt),
	})
	require.NoError(t, err)
	name, profile, err := NewSuppliedCaptionProfile(fixture.catalog, fixture.blobs, base.principal)
	require.NoError(t, err)
	captionService, err := NewService(ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
		Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Principal: base.principal,
		Profiles: map[string]ProfileConfig{name: profile}})
	require.NoError(t, err)
	var transport loomCountingTransport
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		transport.calls.Add(1)
	}))
	defer server.Close()
	previousTransport := http.DefaultTransport
	http.DefaultTransport = &transport
	defer func() { http.DefaultTransport = previousTransport }()
	version, err := fixture.catalog.ContentVersionByID(t.Context(), videoReceipt.ContentVersionID)
	require.NoError(t, err)
	plan, err := captionService.Plan(t.Context(), Selector{NodeID: version.NodeID,
		ContentVersionID: version.ID, Profile: name})
	require.NoError(t, err)
	require.Len(t, plan.Flow, 1)
	assert.Equal(t, string(document.RenditionTrustLocalProcess), plan.Flow[0].TrustBoundary)
	assert.Equal(t, "in-process", plan.Flow[0].RuntimeDisclosure.Endpoint)
	_, err = captionService.RetryMedia(t.Context(), uuid.New().String(), remote.SourceID,
		MediaProcessingRequest{Profile: name, SuppliedInputID: caption.SuppliedInputID})
	require.Error(t, err)
	status, err := base.MediaStatus(t.Context(), remote.SourceID)
	require.NoError(t, err)
	assert.Equal(t, "content_available", status.Outcome)
	assert.Equal(t, "unprocessed", status.CoverageState)
	_, err = captionService.GrantConsent(t.Context(), ConsentGrantRequest{Selector: Selector{
		NodeID: version.NodeID, ContentVersionID: version.ID, Profile: name}, PlanFingerprint: plan.Fingerprint})
	require.NoError(t, err)
	queued, err := captionService.RetryMedia(t.Context(), uuid.New().String(), remote.SourceID,
		MediaProcessingRequest{Profile: name, SuppliedInputID: caption.SuppliedInputID})
	require.NoError(t, err)
	runLoomRenditionJob(t, captionService, queued.JobID)
	processed, err := captionService.MediaStatus(t.Context(), remote.SourceID)
	require.NoError(t, err)
	assert.Equal(t, "transcribed", processed.CoverageState)
	assert.Zero(t, transport.calls.Load())
	_, err = captionService.RevokeConsent(t.Context())
	require.NoError(t, err)
	_, err = captionService.RetryMedia(t.Context(), uuid.New().String(), remote.SourceID,
		MediaProcessingRequest{Profile: name, SuppliedInputID: caption.SuppliedInputID})
	require.Error(t, err)
}

func TestLoomCoverageStaysTruthful(t *testing.T) {
	fixture := newPublicationFixture(t)
	service := newRemoteRecordingTestService(t, fixture, "operator:loom-coverage", 0, nil)
	remote := loomRemoteForTest(t, service, "coverage")
	status, err := service.MediaStatus(t.Context(), remote.SourceID)
	require.NoError(t, err)
	assert.Equal(t, "unsupported", status.Outcome)
	assert.Equal(t, "unprocessed", status.CoverageState)
	video := mediatest.H264AACMP4()
	videoHash := processingSHA256(video)
	videoReceipt, err := service.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: uuid.New().String(), SourceID: remote.SourceID, OccurrenceID: remote.OccurrenceID,
		Kind: "media", Origin: "supplied", Provider: "loom", Filename: "loom.mp4", MediaType: "video/mp4",
		SHA256: videoHash, ByteLength: int64(len(video)), Content: bytes.NewReader(video),
	})
	require.NoError(t, err)
	var transport loomCountingTransport
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		transport.calls.Add(1)
	}))
	defer server.Close()
	previousTransport := http.DefaultTransport
	http.DefaultTransport = &transport
	defer func() { http.DefaultTransport = previousTransport }()
	status, err = service.MediaStatus(t.Context(), remote.SourceID)
	require.NoError(t, err)
	assert.Equal(t, "content_available", status.Outcome)
	assert.Equal(t, "unprocessed", status.CoverageState)
	srt := []byte("1\n00:00:00,000 --> 00:00:01,000\ncoverage cue")
	_, err = service.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: uuid.New().String(), SourceID: remote.SourceID, OccurrenceID: remote.OccurrenceID,
		Kind: "caption", Origin: "supplied", Provider: "loom", Filename: "loom.srt",
		MediaType: "application/x-subrip", SHA256: processingSHA256(srt), ByteLength: int64(len(srt)), Content: bytes.NewReader(srt),
	})
	require.NoError(t, err)
	status, err = service.MediaStatus(t.Context(), remote.SourceID)
	require.NoError(t, err)
	assert.Equal(t, "content_available", status.Outcome)
	assert.Equal(t, "unprocessed", status.CoverageState)
	invalid := []byte("1\n00:00:00,000 --> 00:00:01,000\ncaf\xe9\n")
	badCaption, err := service.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: uuid.New().String(), SourceID: remote.SourceID, OccurrenceID: remote.OccurrenceID,
		Kind: "caption", Origin: "supplied", Provider: "loom", Filename: "bad.srt",
		MediaType: "application/x-subrip", SHA256: processingSHA256(invalid),
		ByteLength: int64(len(invalid)), Content: bytes.NewReader(invalid),
	})
	require.NoError(t, err)
	name, profile, err := NewSuppliedCaptionProfile(fixture.catalog, fixture.blobs, service.principal)
	require.NoError(t, err)
	captionService, err := NewService(ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
		Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Principal: service.principal,
		Profiles: map[string]ProfileConfig{name: profile}})
	require.NoError(t, err)
	version, err := fixture.catalog.ContentVersionByID(t.Context(), videoReceipt.ContentVersionID)
	require.NoError(t, err)
	selector := Selector{NodeID: version.NodeID, ContentVersionID: version.ID, Profile: name}
	plan, err := captionService.Plan(t.Context(), selector)
	require.NoError(t, err)
	_, err = captionService.GrantConsent(t.Context(), ConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
	require.NoError(t, err)
	queued, err := captionService.RetryMedia(t.Context(), uuid.New().String(), remote.SourceID,
		MediaProcessingRequest{Profile: name, SuppliedInputID: badCaption.SuppliedInputID})
	require.NoError(t, err)
	runLoomRenditionJob(t, captionService, queued.JobID)
	failed, err := captionService.MediaStatus(t.Context(), remote.SourceID)
	require.NoError(t, err)
	assert.Equal(t, "failed", failed.OperationState)
	assert.Equal(t, "unavailable", failed.CoverageState)
	valid := []byte("1\n00:00:00,000 --> 00:00:01,000\ncoverage cue\n")
	goodCaption, err := service.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: uuid.New().String(), SourceID: remote.SourceID, OccurrenceID: remote.OccurrenceID,
		Kind: "caption", Origin: "supplied", Provider: "loom", Filename: "good.srt",
		MediaType: "application/x-subrip", SHA256: processingSHA256(valid),
		ByteLength: int64(len(valid)), Content: bytes.NewReader(valid),
	})
	require.NoError(t, err)
	queued, err = captionService.RetryMedia(t.Context(), uuid.New().String(), remote.SourceID,
		MediaProcessingRequest{Profile: name, SuppliedInputID: goodCaption.SuppliedInputID})
	require.NoError(t, err)
	runLoomRenditionJob(t, captionService, queued.JobID)
	succeeded, err := captionService.MediaStatus(t.Context(), remote.SourceID)
	require.NoError(t, err)
	assert.Equal(t, "succeeded", succeeded.OperationState)
	assert.Equal(t, "transcribed", succeeded.CoverageState)
}

func runLoomRenditionJob(t *testing.T, service *Service, jobID string) {
	t.Helper()
	waiter, err := service.catalog.RenditionJobWaiterByID(t.Context(), jobID)
	require.NoError(t, err)
	worker, err := NewRenditionWorker(RenditionWorkerConfig{
		Catalog: service.catalog, Blobs: service.blobs, Runtime: service.RenditionRuntimes(),
		Gate: newWorkerTestGate(), Owner: "loom-coverage-worker", LeaseDuration: time.Minute,
		IdleDelay: time.Millisecond,
	})
	require.NoError(t, err)
	_, err = worker.RunJob(t.Context(), waiter.JobID)
	require.NoError(t, err)
	continuation := &MediaContinuationWorker{Service: service, IdleDelay: time.Millisecond}
	_, err = continuation.RunOne(t.Context())
	require.NoError(t, err)
}

type countingReader struct{ reads int }

type loomCountingTransport struct{ calls atomic.Int64 }

func (transport *loomCountingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	transport.calls.Add(1)
	return nil, io.EOF
}

func (reader *countingReader) Read(_ []byte) (int, error) { reader.reads++; return 0, io.EOF }

func (reader *countingReader) Close() error { return nil }

func loomRemoteForTest(t *testing.T, service *Service, ref string) MediaReceipt {
	t.Helper()
	request := RemoteRecordingRequest{OperationID: uuid.New().String(), ReferenceURL: "https://private.invalid/" + ref,
		CanonicalURL: "https://www.loom.com/share/" + ref,
		Occurrence:   MediaOccurrenceInput{Ref: ref, Revision: "1", Filename: "loom.mp4"}}
	receipt, err := service.SubmitRemoteRecording(t.Context(), request)
	require.NoError(t, err)
	return receipt
}
