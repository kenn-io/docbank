package processing

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/mediatranscript"
	"go.kenn.io/docbank/internal/retrieval"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

func TestMediaTranscriptRequiresTheCompleteStableTuple(t *testing.T) {
	fixture := newPublicationFixture(t)
	service, err := NewService(ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
		Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Principal: "operator:transcript"})
	require.NoError(t, err)
	_, err = service.MediaTranscript(t.Context(), MediaTranscriptRequest{SourceID: "source"})
	require.ErrorIs(t, err, ErrMediaTranscriptInvalid)
}

func TestMediaTranscriptArtifactBounds(t *testing.T) {
	timed := document.NormalizedEvidenceV1{Family: "video", UnitKind: document.EvidenceUnitSegment}
	legacy := document.NormalizedEvidenceV1{Family: "audio", UnitKind: document.EvidenceUnitGeneric}
	require.Equal(t, int64(16<<20), mediaTranscriptArtifactLimit(timed))
	require.Equal(t, int64(64<<20), mediaTranscriptArtifactLimit(legacy))

	payload := bytes.Repeat([]byte{'x'}, int(mediatranscript.MaxArtifactBytes))
	hash := processingSHA256(payload)
	reader := &mediaTranscriptArtifactReader{payload: payload}
	artifact := store.RenditionArtifactRecord{BlobHash: hash, Size: int64(len(payload))}
	got, err := readMediaTranscriptArtifact(t.Context(), reader, artifact, timed)
	require.NoError(t, err)
	require.Equal(t, payload, got)
	require.Equal(t, 1, reader.opens)

	artifact.Size++
	_, err = readMediaTranscriptArtifact(t.Context(), reader, artifact, timed)
	require.ErrorIs(t, err, retrieval.ErrMediaArtifactOversize)
	require.Equal(t, 1, reader.opens, "timed size gate runs before blob access")

	over := append([]byte(`{"contract_version":"media-transcript/v1","padding":"`),
		bytes.Repeat([]byte{'x'}, int(mediatranscript.MaxArtifactBytes))...)
	over = append(over, []byte(`"}`)...)
	_, err = decodeMediaTranscriptArtifact(over, timed, false)
	require.ErrorIs(t, err, retrieval.ErrMediaArtifactOversize)
	legacyArtifact := store.RenditionArtifactRecord{BlobHash: hash, Size: mediaTranscriptMaxArtifactBytes + 1}
	_, err = readMediaTranscriptArtifact(t.Context(), reader, legacyArtifact, legacy)
	require.ErrorIs(t, err, retrieval.ErrMediaArtifactOversize)
	require.Equal(t, 1, reader.opens, "legacy size gate also runs before blob access")
	t.Logf("artifact limits: timed=%d legacy=%d timed_limit_plus_one=16777217 opens_after_rejections=%d",
		mediaTranscriptArtifactLimit(timed), mediaTranscriptArtifactLimit(legacy), reader.opens)
}

func TestMediaTranscriptProvenance(t *testing.T) {
	policy, err := document.NewEvidencePolicy(4096)
	require.NoError(t, err)

	legacyEvidence, legacyArtifact, err := document.BuildTranscriptEvidenceV1(
		document.SuppliedTranscript{Provider: "synthetic", Text: "legacy supplied text"}, policy)
	require.NoError(t, err)
	legacy, err := decodeMediaTranscriptArtifact(legacyArtifact.Payload, legacyEvidence, false)
	require.NoError(t, err)
	require.Equal(t, "supplied", legacy.Origin)
	require.Equal(t, "synthetic", legacy.Provider)
	require.Len(t, legacy.Units, 1)
	require.Equal(t, "legacy supplied text", legacy.Units[0].Text)
	require.Nil(t, legacy.Units[0].StartMS)
	require.Nil(t, legacy.Units[0].EndMS)

	timedSource, timedArtifact, err := mediatranscript.Build(mediatranscript.ArtifactV1{
		ContractVersion: "media-transcript/v1", Origin: "generated", Provider: "synthetic",
		Language: "en", Segments: []mediatranscript.Segment{{Order: 0, StartMS: 0, EndMS: 1000,
			Speaker: "Speaker 1", Text: "timed generated text"}},
	}, "audio", policy)
	require.NoError(t, err)
	timedEvidence, err := document.NormalizeEvidenceV1(timedSource, policy)
	require.NoError(t, err)
	timed, err := decodeMediaTranscriptArtifact(timedArtifact.Payload, timedEvidence, true)
	require.NoError(t, err)
	require.Equal(t, "generated", timed.Origin)
	require.Equal(t, "synthetic", timed.Provider)
	require.Equal(t, "en", timed.Language)
	require.True(t, timed.Truncated)
	require.Len(t, timed.Units, 1)
	require.Equal(t, "Speaker 1", timed.Units[0].Speaker)
	require.Equal(t, int64(0), *timed.Units[0].StartMS)
	require.Equal(t, int64(1000), *timed.Units[0].EndMS)

	_, err = decodeMediaTranscriptArtifact([]byte(`{"contract_version":"media-transcript/v2"}`), timedEvidence, false)
	require.ErrorIs(t, err, errUnknownMediaTranscriptFormat)
	_, err = decodeMediaTranscriptArtifact([]byte(`{"contract_version":"media-transcript/v1","segments":`), timedEvidence, false)
	require.ErrorIs(t, err, ErrMediaTranscriptCorrupt)
	t.Logf("provenance: legacy_origin=%s legacy_provider=%s legacy_timing=nil timed_origin=%s timed_provider=%s timed_language=%s timed_start_ms=%d timed_end_ms=%d unknown=unavailable malformed=corrupt",
		legacy.Origin, legacy.Provider, timed.Origin, timed.Provider, timed.Language,
		*timed.Units[0].StartMS, *timed.Units[0].EndMS)
}

func TestMediaTranscriptNodeAndContentVersionStates(t *testing.T) {
	version := store.ContentVersion{ID: "version-1", NodeRevision: 1}
	trashedAt := "2026-09-24T00:00:00Z"
	for name, test := range map[string]struct {
		node store.Node
		want bool
	}{
		"live file":        {node: store.Node{Kind: "file", CurrentVersionID: version.ID, Revision: 1}, want: true},
		"directory":        {node: store.Node{Kind: "dir", CurrentVersionID: version.ID, Revision: 1}},
		"trash":            {node: store.Node{Kind: "file", CurrentVersionID: version.ID, Revision: 1, TrashedAt: &trashedAt}},
		"changed head":     {node: store.Node{Kind: "file", CurrentVersionID: "version-2", Revision: 2}},
		"changed revision": {node: store.Node{Kind: "file", CurrentVersionID: version.ID, Revision: 2}},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, test.want, mediaTranscriptNodeReadable(version, test.node))
		})
	}
}

func TestMediaTranscriptCatalogLivenessStates(t *testing.T) {
	fixture := newPublicationFixture(t)
	service, err := NewService(ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
		Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Principal: "operator:catalog-liveness"})
	require.NoError(t, err)

	initialBytes := mediatest.WAV()
	initialBlob, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(initialBytes))
	require.NoError(t, err)
	node, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), "catalog-liveness.wav",
		initialBlob.Hash, initialBlob.Size, "audio/wav", processingBlobPhysical(t, initialBlob))
	require.NoError(t, err)
	initialVersion, err := fixture.catalog.ContentVersionByID(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	retained, err := service.SubmitSuppliedMedia(t.Context(), SuppliedMediaRequest{
		OperationID: uuid.New().String(), Content: bytes.NewReader(initialBytes), Filename: "catalog-liveness.wav",
		MediaType: "audio/wav", SHA256: initialBlob.Hash, ByteLength: initialBlob.Size,
		ExistingContentVersionID: initialVersion.ID,
		Occurrence:               MediaOccurrenceInput{Ref: "catalog-liveness", Revision: "1", Filename: "catalog-liveness.wav"},
	})
	require.NoError(t, err)

	replacedBytes := append([]byte(nil), initialBytes...)
	replacedBytes[len(replacedBytes)-1] ^= 1
	replacedBlob, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(replacedBytes))
	require.NoError(t, err)
	replacedNode, _, err := fixture.catalog.ReplaceContent(t.Context(), node.ID, node.Revision,
		replacedBlob.Hash, replacedBlob.Size, "audio/wav", processingBlobPhysical(t, replacedBlob))
	require.NoError(t, err)
	boundRead, err := service.MediaTranscript(t.Context(), MediaTranscriptRequest{
		SourceID: retained.SourceID, SourceVersionID: retained.SourceVersionID,
		ContentVersionID: initialVersion.ID,
	})
	require.NoError(t, err)
	require.Equal(t, mediaTranscriptEvidenceStale, boundRead.EvidenceState)
	require.Nil(t, boundRead.Transcript)

	directory, err := fixture.catalog.Mkdir(t.Context(), fixture.catalog.RootID(), "catalog-liveness-dir")
	require.NoError(t, err)
	directoryNode, err := fixture.catalog.NodeByID(t.Context(), directory.ID)
	require.NoError(t, err)
	require.Equal(t, "dir", directoryNode.Kind)
	require.Empty(t, directoryNode.CurrentVersionID)
	_, _, err = fixture.catalog.ContentVersions(t.Context(), directory.ID, 10, 0)
	require.ErrorIs(t, err, store.ErrNotFile)

	pruned, err := fixture.catalog.PruneContentVersions(t.Context(), node.ID, replacedNode.Revision,
		store.VersionPruneSelector{VersionIDs: []string{initialVersion.ID}}, true)
	require.NoError(t, err)
	require.Zero(t, pruned.DeletedVersions)
	require.Len(t, pruned.DependencyRetained, 1)
	require.Equal(t, initialVersion.ID, pruned.DependencyRetained[0].ID)
	_, err = fixture.catalog.ContentVersionByID(t.Context(), initialVersion.ID)
	require.NoError(t, err)
	boundAfterPrune, err := service.MediaTranscript(t.Context(), MediaTranscriptRequest{
		SourceID: retained.SourceID, SourceVersionID: retained.SourceVersionID,
		ContentVersionID: initialVersion.ID,
	})
	require.NoError(t, err)
	require.Equal(t, mediaTranscriptEvidenceStale, boundAfterPrune.EvidenceState)
	require.Nil(t, boundAfterPrune.Transcript)

	unboundBlob, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(initialBytes))
	require.NoError(t, err)
	unboundNode, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), "catalog-prunable.wav",
		unboundBlob.Hash, unboundBlob.Size, "audio/wav", processingBlobPhysical(t, unboundBlob))
	require.NoError(t, err)
	unboundVersion, err := fixture.catalog.ContentVersionByID(t.Context(), unboundNode.CurrentVersionID)
	require.NoError(t, err)
	unboundChanged := append([]byte(nil), initialBytes...)
	unboundChanged[len(unboundChanged)-1] ^= 1
	unboundChangedBlob, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(unboundChanged))
	require.NoError(t, err)
	unboundHead, _, err := fixture.catalog.ReplaceContent(t.Context(), unboundNode.ID, unboundNode.Revision,
		unboundChangedBlob.Hash, unboundChangedBlob.Size, "audio/wav", processingBlobPhysical(t, unboundChangedBlob))
	require.NoError(t, err)
	unboundPrune, err := fixture.catalog.PruneContentVersions(t.Context(), unboundNode.ID, unboundHead.Revision,
		store.VersionPruneSelector{VersionIDs: []string{unboundVersion.ID}}, true)
	require.NoError(t, err)
	require.Equal(t, 1, unboundPrune.DeletedVersions)
	_, err = fixture.catalog.ContentVersionByID(t.Context(), unboundVersion.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
	_, _, err = service.mediaTranscriptNode(t.Context(), unboundVersion.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
	t.Logf("catalog liveness: directory_kind=%s directory_versions=ErrNotFile source_bound_before_replace=%s bound_prune=DependencyRetained source_bound_after_prune=%s unbound_pruned_content_version=ErrNotFound owner_lookup=ErrNotFound",
		directoryNode.Kind, boundRead.EvidenceState, boundAfterPrune.EvidenceState)
}

func TestMediaTranscriptRechecksAuthority(t *testing.T) {
	fixture := newPublicationFixture(t)
	base := newRemoteRecordingTestService(t, fixture, "operator:transcript-race", 0, nil)
	remote := loomRemoteForTest(t, base, "transcript-race")
	video := mediatest.H264AACMP4()
	videoReceipt, err := base.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: uuid.New().String(), SourceID: remote.SourceID, OccurrenceID: remote.OccurrenceID,
		Kind: "media", Origin: "supplied", Provider: "synthetic", Filename: "call.mp4", MediaType: "video/mp4",
		SHA256: processingSHA256(video), ByteLength: int64(len(video)), Content: bytes.NewReader(video),
	})
	require.NoError(t, err)
	srt := []byte("1\n00:00:00,000 --> 00:00:01,000\nrace cue\n")
	caption, err := base.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: uuid.New().String(), SourceID: remote.SourceID, OccurrenceID: remote.OccurrenceID,
		Kind: "caption", Origin: "supplied", Provider: "synthetic", Filename: "call.srt",
		MediaType: "application/x-subrip", SHA256: processingSHA256(srt), ByteLength: int64(len(srt)),
		Content: bytes.NewReader(srt),
	})
	require.NoError(t, err)
	name, profile, err := NewSuppliedCaptionProfile(fixture.catalog, fixture.blobs, base.principal)
	require.NoError(t, err)
	service, err := NewService(ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
		Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Principal: base.principal,
		Profiles: map[string]ProfileConfig{name: profile}})
	require.NoError(t, err)
	version, err := fixture.catalog.ContentVersionByID(t.Context(), videoReceipt.ContentVersionID)
	require.NoError(t, err)
	selector := Selector{NodeID: version.NodeID, ContentVersionID: version.ID, Profile: name}
	plan, err := service.Plan(t.Context(), selector)
	require.NoError(t, err)
	_, err = service.GrantConsent(t.Context(), ConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
	require.NoError(t, err)
	queued, err := service.RetryMedia(t.Context(), uuid.New().String(), remote.SourceID,
		MediaProcessingRequest{Profile: name, SuppliedInputID: caption.SuppliedInputID})
	require.NoError(t, err)
	runLoomRenditionJob(t, service, queued.JobID)

	request := MediaTranscriptRequest{SourceID: remote.SourceID, SourceVersionID: queued.SourceVersionID,
		ContentVersionID: queued.ContentVersionID}
	opened := make(chan struct{})
	release := make(chan struct{})
	serviceReader := &mediaTranscriptBarrierReader{delegate: fixture.blobs, opened: opened, release: release}
	readCtx, cancel := context.WithTimeout(withMediaTranscriptBlobReader(t.Context(), serviceReader), 10*time.Second)
	defer cancel()
	type transcriptReadResult struct {
		value MediaTranscript
		err   error
	}
	readDone := make(chan transcriptReadResult, 1)
	go func() {
		value, readErr := service.MediaTranscript(readCtx, request)
		readDone <- transcriptReadResult{value: value, err: readErr}
	}()
	select {
	case <-opened:
	case <-readCtx.Done():
		close(release)
		t.Fatalf("transcript read did not reach the blob barrier: %v", readCtx.Err())
	}

	node, err := fixture.catalog.NodeByID(t.Context(), version.NodeID)
	require.NoError(t, err)
	changed := append([]byte(nil), video...)
	changed[len(changed)-1]++
	changedBlob, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(changed))
	require.NoError(t, err)
	_, _, err = fixture.catalog.ReplaceContent(t.Context(), node.ID, node.Revision,
		changedBlob.Hash, changedBlob.Size, "video/mp4", processingBlobPhysical(t, changedBlob))
	require.NoError(t, err)
	close(release)

	select {
	case result := <-readDone:
		require.NoError(t, result.err)
		require.Equal(t, mediaTranscriptEvidenceStale, result.value.EvidenceState)
		require.Nil(t, result.value.Transcript)
		t.Logf("read race: barrier=normalized_open mutation=node_replace outcome=%s text=nil", result.value.EvidenceState)
	case <-readCtx.Done():
		t.Fatalf("transcript read did not finish after authority mutation: %v", readCtx.Err())
	}
}

func TestMediaTranscriptMismatchedContentVersionReturnsStaleWithoutText(t *testing.T) {
	fixture := newPublicationFixture(t)
	raw := mediatest.WAV()
	written, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(raw))
	require.NoError(t, err)
	node, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), "call.wav", written.Hash, written.Size,
		"audio/wav", processingBlobPhysical(t, written))
	require.NoError(t, err)
	service, err := NewService(ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
		Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Principal: "operator:transcript"})
	require.NoError(t, err)
	retained, err := service.SubmitSuppliedMedia(t.Context(), SuppliedMediaRequest{
		OperationID: uuid.New().String(), Filename: "call.wav", MediaType: "audio/wav", SHA256: written.Hash,
		ByteLength: written.Size, ExistingContentVersionID: node.CurrentVersionID,
		Occurrence: MediaOccurrenceInput{Ref: "call", Revision: "1"},
	})
	require.NoError(t, err)
	wrongContentVersionID := uuid.New().String()
	result, err := service.MediaTranscript(t.Context(), MediaTranscriptRequest{
		SourceID: retained.SourceID, SourceVersionID: retained.SourceVersionID,
		ContentVersionID: wrongContentVersionID,
	})
	require.NoError(t, err)
	require.Equal(t, "stale", result.EvidenceState)
	require.Nil(t, result.Transcript)
	require.Equal(t, wrongContentVersionID, result.ContentVersionID)
}

func TestMediaTranscriptCoverageAfterRetry(t *testing.T) {
	fixture := newPublicationFixture(t)
	base := newRemoteRecordingTestService(t, fixture, "operator:transcript-read", 0, nil)
	remote := loomRemoteForTest(t, base, "transcript-read")
	video := mediatest.H264AACMP4()
	videoReceipt, err := base.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: uuid.New().String(), SourceID: remote.SourceID, OccurrenceID: remote.OccurrenceID,
		Kind: "media", Origin: "supplied", Provider: "synthetic", Filename: "call.mp4", MediaType: "video/mp4",
		SHA256: processingSHA256(video), ByteLength: int64(len(video)), Content: bytes.NewReader(video),
	})
	require.NoError(t, err)
	srt := []byte("1\n00:00:00,000 --> 00:00:01,000\nsynthetic cue\n")
	caption, err := base.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: uuid.New().String(), SourceID: remote.SourceID, OccurrenceID: remote.OccurrenceID,
		Kind: "caption", Origin: "supplied", Provider: "synthetic", Filename: "call.srt",
		MediaType: "application/x-subrip", SHA256: processingSHA256(srt), ByteLength: int64(len(srt)), Content: bytes.NewReader(srt),
	})
	require.NoError(t, err)
	initial, err := base.MediaTranscript(t.Context(), MediaTranscriptRequest{
		SourceID: remote.SourceID, SourceVersionID: videoReceipt.SourceVersionID,
		ContentVersionID: videoReceipt.ContentVersionID,
	})
	require.NoError(t, err)
	require.Equal(t, mediaTranscriptEvidenceUnavailable, initial.EvidenceState)
	require.Equal(t, "unprocessed", initial.CoverageState)
	name, profile, err := NewSuppliedCaptionProfile(fixture.catalog, fixture.blobs, base.principal)
	require.NoError(t, err)
	captionService, err := NewService(ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
		Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Principal: base.principal,
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
		MediaProcessingRequest{Profile: name, SuppliedInputID: caption.SuppliedInputID})
	require.NoError(t, err)
	pending, err := captionService.MediaTranscript(t.Context(), MediaTranscriptRequest{
		SourceID: remote.SourceID, SourceVersionID: queued.SourceVersionID, ContentVersionID: queued.ContentVersionID,
	})
	require.NoError(t, err)
	require.Equal(t, mediaTranscriptEvidencePending, pending.EvidenceState)
	require.Equal(t, "queued", pending.OperationState)
	t.Logf("availability: no_evidence=unavailable coverage=unprocessed queued=%s operation=%s",
		pending.EvidenceState, pending.OperationState)
	runLoomRenditionJob(t, captionService, queued.JobID)
	result, err := captionService.MediaTranscript(t.Context(), MediaTranscriptRequest{
		SourceID: remote.SourceID, SourceVersionID: queued.SourceVersionID, ContentVersionID: queued.ContentVersionID,
	})
	require.NoError(t, err)
	require.Equal(t, mediaTranscriptEvidenceReady, result.EvidenceState)
	require.NotNil(t, result.Transcript)
	require.Equal(t, "supplied", result.Transcript.Origin)
	require.Equal(t, "synthetic", result.Transcript.Provider)
	require.Len(t, result.Transcript.Units, 1)
	require.Equal(t, "synthetic cue", result.Transcript.Units[0].Text)
	require.NotNil(t, result.Transcript.Units[0].StartMS)
	require.Equal(t, int64(0), *result.Transcript.Units[0].StartMS)
	require.Equal(t, "transcribed", result.CoverageState)
	require.Equal(t, "succeeded", result.OperationState)

	invalid := []byte("1\n00:00:00,000 --> 00:00:01,000\ncaf\xe9\n")
	badCaption, err := base.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: uuid.New().String(), SourceID: remote.SourceID, OccurrenceID: remote.OccurrenceID,
		Kind: "caption", Origin: "supplied", Provider: "synthetic", Filename: "bad.srt",
		MediaType: "application/x-subrip", SHA256: processingSHA256(invalid), ByteLength: int64(len(invalid)),
		Content: bytes.NewReader(invalid),
	})
	require.NoError(t, err)
	failedQueued, err := captionService.RetryMedia(t.Context(), uuid.New().String(), remote.SourceID,
		MediaProcessingRequest{Profile: name, SuppliedInputID: badCaption.SuppliedInputID})
	require.NoError(t, err)
	runLoomRenditionJob(t, captionService, failedQueued.JobID)
	failed, err := captionService.MediaTranscript(t.Context(), MediaTranscriptRequest{
		SourceID: remote.SourceID, SourceVersionID: queued.SourceVersionID, ContentVersionID: queued.ContentVersionID,
	})
	require.NoError(t, err)
	require.Equal(t, mediaTranscriptEvidenceReady, failed.EvidenceState)
	require.Equal(t, "transcribed", failed.CoverageState)
	require.Equal(t, "failed", failed.OperationState)
	require.NotNil(t, failed.Transcript)
	require.Equal(t, "synthetic cue", failed.Transcript.Units[0].Text)

	secondary, err := base.DeclareMediaOccurrence(t.Context(), uuid.New().String(), remote.SourceID,
		MediaOccurrenceInput{Ref: "transcript-read-secondary", Revision: "1", Filename: "call.mp4"})
	require.NoError(t, err)
	secondarySRT := []byte("1\n00:00:00,000 --> 00:00:01,000\nsecondary cue\n")
	secondaryCaption, err := base.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: uuid.New().String(), SourceID: remote.SourceID, OccurrenceID: secondary.OccurrenceID,
		Kind: "caption", Origin: "supplied", Provider: "synthetic", Filename: "secondary.srt",
		MediaType: "application/x-subrip", SHA256: processingSHA256(secondarySRT), ByteLength: int64(len(secondarySRT)),
		Content: bytes.NewReader(secondarySRT),
	})
	require.NoError(t, err)
	secondaryQueued, err := captionService.RetryMedia(t.Context(), uuid.New().String(), remote.SourceID,
		MediaProcessingRequest{Profile: name, SuppliedInputID: secondaryCaption.SuppliedInputID})
	require.NoError(t, err)
	runLoomRenditionJob(t, captionService, secondaryQueued.JobID)
	secondaryResult, err := captionService.MediaTranscript(t.Context(), MediaTranscriptRequest{
		SourceID: remote.SourceID, SourceVersionID: queued.SourceVersionID, ContentVersionID: queued.ContentVersionID,
	})
	require.NoError(t, err)
	require.Equal(t, mediaTranscriptEvidenceReady, secondaryResult.EvidenceState)
	require.Equal(t, "secondary cue", secondaryResult.Transcript.Units[0].Text)
	_, err = base.RevokeMediaOccurrence(t.Context(), uuid.New().String(), secondary.OccurrenceID, "1")
	require.NoError(t, err)
	revokedInput, err := captionService.MediaTranscript(t.Context(), MediaTranscriptRequest{
		SourceID: remote.SourceID, SourceVersionID: queued.SourceVersionID, ContentVersionID: queued.ContentVersionID,
	})
	require.NoError(t, err)
	require.Equal(t, mediaTranscriptEvidenceUnavailable, revokedInput.EvidenceState)
	require.Nil(t, revokedInput.Transcript)

	node, err := fixture.catalog.NodeByID(t.Context(), version.NodeID)
	require.NoError(t, err)
	changed := append([]byte(nil), video...)
	changed[len(changed)-1]++
	changedBlob, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(changed))
	require.NoError(t, err)
	previousRevision := node.Revision
	previousVersionID := node.CurrentVersionID
	replacedNode, _, err := fixture.catalog.ReplaceContent(t.Context(), node.ID, node.Revision,
		changedBlob.Hash, changedBlob.Size, "video/mp4", processingBlobPhysical(t, changedBlob))
	require.NoError(t, err)
	require.Greater(t, replacedNode.Revision, previousRevision)
	require.NotEqual(t, previousVersionID, replacedNode.CurrentVersionID)
	currentVersion, err := fixture.catalog.ContentVersionByID(t.Context(), replacedNode.CurrentVersionID)
	require.NoError(t, err)
	require.NotEqual(t, version.ID, currentVersion.ID)
	stale, err := captionService.MediaTranscript(t.Context(), MediaTranscriptRequest{
		SourceID: remote.SourceID, SourceVersionID: queued.SourceVersionID, ContentVersionID: queued.ContentVersionID,
	})
	require.NoError(t, err)
	require.Equal(t, mediaTranscriptEvidenceStale, stale.EvidenceState)
	require.Nil(t, stale.Transcript)

	node, err = fixture.catalog.NodeByID(t.Context(), node.ID)
	require.NoError(t, err)
	_, _, err = fixture.catalog.Trash(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	trashed, err := captionService.MediaTranscript(t.Context(), MediaTranscriptRequest{
		SourceID: remote.SourceID, SourceVersionID: queued.SourceVersionID, ContentVersionID: queued.ContentVersionID,
	})
	require.NoError(t, err)
	require.Equal(t, mediaTranscriptEvidenceUnavailable, trashed.EvidenceState)
	require.Nil(t, trashed.Transcript)
	t.Logf("coverage and liveness: failed_retry coverage=%s operation=%s replacement=%s text=nil trash=%s text=nil",
		failed.CoverageState, failed.OperationState, stale.EvidenceState, trashed.EvidenceState)
}

func TestMediaTranscriptSharedSourceVersionsKeepForeignCaptionOut(t *testing.T) {
	fixture := newPublicationFixture(t)
	base := newRemoteRecordingTestService(t, fixture, "operator:transcript-shared", 0, nil)
	remote := loomRemoteForTest(t, base, "shared-source-versions")
	video := mediatest.H264AACMP4()
	videoHash := processingSHA256(video)
	videoReceipt, err := base.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: uuid.New().String(), SourceID: remote.SourceID, OccurrenceID: remote.OccurrenceID,
		Kind: "media", Origin: "supplied", Provider: "synthetic", Filename: "call.mp4", MediaType: "video/mp4",
		SHA256: videoHash, ByteLength: int64(len(video)), Content: bytes.NewReader(video),
	})
	require.NoError(t, err)
	captionABytes := []byte("1\n00:00:00,000 --> 00:00:01,000\ncaption A\n")
	captionA, err := base.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: uuid.New().String(), SourceID: remote.SourceID, OccurrenceID: remote.OccurrenceID,
		Kind: "caption", Origin: "supplied", Provider: "synthetic", Filename: "a.srt",
		MediaType: "application/x-subrip", SHA256: processingSHA256(captionABytes), ByteLength: int64(len(captionABytes)),
		Content: bytes.NewReader(captionABytes),
	})
	require.NoError(t, err)
	name, profile, err := NewSuppliedCaptionProfile(fixture.catalog, fixture.blobs, base.principal)
	require.NoError(t, err)
	captionService, err := NewService(ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
		Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Principal: base.principal,
		Profiles: map[string]ProfileConfig{name: profile}})
	require.NoError(t, err)
	version, err := fixture.catalog.ContentVersionByID(t.Context(), videoReceipt.ContentVersionID)
	require.NoError(t, err)
	selector := Selector{NodeID: version.NodeID, ContentVersionID: version.ID, Profile: name}
	plan, err := captionService.Plan(t.Context(), selector)
	require.NoError(t, err)
	_, err = captionService.GrantConsent(t.Context(), ConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
	require.NoError(t, err)
	queuedA, err := captionService.RetryMedia(t.Context(), uuid.New().String(), remote.SourceID,
		MediaProcessingRequest{Profile: name, SuppliedInputID: captionA.SuppliedInputID})
	require.NoError(t, err)
	runLoomRenditionJob(t, captionService, queuedA.JobID)

	occurrenceBID := "occurrence-shared-b"
	_, err = fixture.catalog.RecordMediaOccurrence(t.Context(), store.MediaOperation{
		ID: uuid.New().String(), Principal: base.principal, Verb: "declare_occurrence",
		RequestSHA256: processingSHA256([]byte("shared-source-version-b")), SourceID: remote.SourceID,
	}, store.MediaOccurrenceInput{ID: occurrenceBID, SourceID: remote.SourceID, Principal: base.principal,
		Ref: "shared-source-version-b", Revision: "1", Filename: "call.mp4", MessageJSON: "{}"})
	require.NoError(t, err)
	sourceVersionBID := "source-version-shared-b"
	require.NoError(t, fixture.catalog.PublishMediaSourceVersion(t.Context(), store.MediaSourceVersionInput{
		ID: sourceVersionBID, SourceID: remote.SourceID, Revision: 2, ExpectedHeadRevision: 1,
		ContentVersionID: videoReceipt.ContentVersionID, SourceSHA256: videoHash, SourceBytes: int64(len(video)),
		CaptureJSON: "{}", ClaimSHA256: processingSHA256([]byte("{}")), BindOccurrenceIDs: []string{occurrenceBID},
	}))
	captionBBytes := []byte("1\n00:00:00,000 --> 00:00:01,000\ncaption B\n")
	captionB, err := base.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: uuid.New().String(), SourceID: remote.SourceID, OccurrenceID: occurrenceBID,
		Kind: "caption", Origin: "supplied", Provider: "synthetic", Filename: "b.srt",
		MediaType: "application/x-subrip", SHA256: processingSHA256(captionBBytes), ByteLength: int64(len(captionBBytes)),
		Content: bytes.NewReader(captionBBytes),
	})
	require.NoError(t, err)
	queuedB, err := captionService.RetryMedia(t.Context(), uuid.New().String(), remote.SourceID,
		MediaProcessingRequest{Profile: name, SuppliedInputID: captionB.SuppliedInputID})
	require.NoError(t, err)
	require.Equal(t, sourceVersionBID, queuedB.SourceVersionID)
	runLoomRenditionJob(t, captionService, queuedB.JobID)

	aRequest := MediaTranscriptRequest{SourceID: remote.SourceID, SourceVersionID: queuedA.SourceVersionID,
		ContentVersionID: videoReceipt.ContentVersionID}
	bRequest := MediaTranscriptRequest{SourceID: remote.SourceID, SourceVersionID: sourceVersionBID,
		ContentVersionID: videoReceipt.ContentVersionID}
	aBeforeRevoke, err := captionService.MediaTranscript(t.Context(), aRequest)
	require.NoError(t, err)
	require.Equal(t, mediaTranscriptEvidenceStale, aBeforeRevoke.EvidenceState)
	require.Nil(t, aBeforeRevoke.Transcript)
	bBeforeRevoke, err := captionService.MediaTranscript(t.Context(), bRequest)
	require.NoError(t, err)
	require.Equal(t, mediaTranscriptEvidenceReady, bBeforeRevoke.EvidenceState)
	require.Equal(t, "caption B", bBeforeRevoke.Transcript.Units[0].Text)

	_, err = captionService.RevokeMediaOccurrence(t.Context(), uuid.New().String(), occurrenceBID, "1")
	require.NoError(t, err)
	_, err = captionService.MediaTranscript(t.Context(), bRequest)
	require.ErrorIs(t, err, store.ErrNotFound)
	aAfterRevoke, err := captionService.MediaTranscript(t.Context(), aRequest)
	require.NoError(t, err)
	require.Equal(t, mediaTranscriptEvidenceStale, aAfterRevoke.EvidenceState)
	require.Nil(t, aAfterRevoke.Transcript)
	t.Logf("shared bytes: before_revoke A=%s text=nil B=%s text=%q after_revoke A=%s text=nil B=404",
		aBeforeRevoke.EvidenceState, bBeforeRevoke.EvidenceState,
		bBeforeRevoke.Transcript.Units[0].Text, aAfterRevoke.EvidenceState)
}

type mediaTranscriptArtifactReader struct {
	payload []byte
	opens   int
}

func (reader *mediaTranscriptArtifactReader) OpenStreamContext(
	_ context.Context, _ string,
) (packstore.VerifiedReadCloser, int64, error) {
	reader.opens++
	return mediaTranscriptArtifactStream{ReadCloser: io.NopCloser(bytes.NewReader(reader.payload))},
		int64(len(reader.payload)), nil
}

type mediaTranscriptArtifactStream struct{ io.ReadCloser }

func (mediaTranscriptArtifactStream) Verify() error  { return nil }
func (mediaTranscriptArtifactStream) Verified() bool { return true }

type mediaTranscriptBarrierReader struct {
	delegate retrieval.MediaEvidenceBlobReader
	opened   chan struct{}
	release  <-chan struct{}
	once     sync.Once
}

func (reader *mediaTranscriptBarrierReader) OpenStreamContext(
	ctx context.Context, hash string,
) (packstore.VerifiedReadCloser, int64, error) {
	stream, size, err := reader.delegate.OpenStreamContext(ctx, hash)
	if err != nil {
		return nil, 0, err
	}
	reader.once.Do(func() { close(reader.opened) })
	select {
	case <-reader.release:
		return stream, size, nil
	case <-ctx.Done():
		_ = stream.Close()
		return nil, 0, ctx.Err()
	}
}
