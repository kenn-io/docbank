package processing

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/retrieval"
	"go.kenn.io/docbank/internal/store"
)

func TestMediaSearchSelectsCoveringBuildBeforeLimits(t *testing.T) {
	t.Parallel()
	f := newMediaStateFixture(t)
	name, supplied, err := NewSuppliedMediaProfile(f.catalog, f.blobs, f.service.principal)
	require.NoError(t, err)
	service, err := NewService(ServiceConfig{Catalog: f.catalog, Blobs: f.blobs,
		Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Principal: f.service.principal,
		Profiles: map[string]ProfileConfig{
			"speech": {Profile: f.service.profiles["speech"].portable, RenditionProvider: f.provider},
			name:     supplied,
		}})
	require.NoError(t, err)
	var ids []string
	var sources []retrieval.MediaSourceSelector
	var selectedBuild string
	var generatedSource retrieval.MediaSourceSelector
	var generatedCandidate, suppliedCandidate MediaReceipt
	for i := range 101 {
		version, selector := f.version, f.selector
		if i != 0 {
			raw := mediatest.WAV()
			raw[len(raw)-1] = byte(i)
			version, selector = f.addWAV(t, fmt.Sprintf("source-%03d.wav", i), raw)
		}
		for _, profile := range []string{"speech", name} {
			selector.Profile = profile
			plan, err := service.Plan(t.Context(), selector)
			require.NoError(t, err)
			_, err = service.GrantConsent(t.Context(), ConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
			require.NoError(t, err)
		}
		retained, err := service.SubmitSuppliedMedia(t.Context(), SuppliedMediaRequest{
			OperationID: uuid.New().String(), Filename: fmt.Sprintf("source-%03d.wav", i), MediaType: "audio/wav",
			SHA256: version.BlobHash, ByteLength: version.Size, ExistingContentVersionID: version.ID,
			Occurrence: MediaOccurrenceInput{Ref: fmt.Sprintf("message-%03d", i), Revision: "1"},
			Processing: &MediaProcessingRequest{Profile: "speech"}})
		require.NoError(t, err)
		runLoomRenditionJob(t, service, retained.JobID)
		if i == 100 {
			generatedSource = retrieval.MediaSourceSelector{SourceID: retained.SourceID,
				SourceVersionID: retained.SourceVersionID, ContentVersionID: version.ID}
			occurrence := "second-source-occurrence"
			_, err := f.catalog.RecordMediaOccurrence(t.Context(), store.MediaOperation{ID: uuid.New().String(),
				Principal: service.principal, Verb: "declare_occurrence", RequestSHA256: processingSHA256([]byte(occurrence)),
				SourceID: retained.SourceID}, store.MediaOccurrenceInput{ID: occurrence, SourceID: retained.SourceID,
				Principal: service.principal, Ref: occurrence, Revision: "1", Filename: "source.wav", MessageJSON: "{}"})
			require.NoError(t, err)
			retained.SourceVersionID, retained.OccurrenceID = "second-source-version", occurrence
			require.NoError(t, f.catalog.PublishMediaSourceVersion(t.Context(), store.MediaSourceVersionInput{
				ID: retained.SourceVersionID, SourceID: retained.SourceID, Revision: 2, ExpectedHeadRevision: 1,
				ContentVersionID: version.ID, CaptureJSON: "{}", BindOccurrenceIDs: []string{occurrence}}))
			retained, err = service.SubmitSuppliedMedia(t.Context(), SuppliedMediaRequest{
				OperationID: uuid.New().String(), Filename: "second.wav", MediaType: "audio/wav",
				SHA256: version.BlobHash, ByteLength: version.Size, ExistingContentVersionID: version.ID,
				Occurrence: MediaOccurrenceInput{Ref: "second-submit", Revision: "1"}})
			require.NoError(t, err)
		}
		phrase := "no selected match"
		if i == 100 {
			phrase = "Synthetic worker output " + strings.Repeat("longer selected transcript ", 30)
		}
		text := []byte(phrase)
		input, err := service.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
			OperationID: uuid.New().String(), SourceID: retained.SourceID, OccurrenceID: retained.OccurrenceID,
			Kind: "transcript", Filename: "transcript.txt", MediaType: "text/plain",
			SHA256: processingSHA256(text), ByteLength: int64(len(text)), Content: bytes.NewReader(text)})
		require.NoError(t, err)
		queued, err := service.RetryMedia(t.Context(), uuid.New().String(), retained.SourceID,
			MediaProcessingRequest{Profile: name, SuppliedInputID: input.SuppliedInputID})
		require.NoError(t, err)
		runLoomRenditionJob(t, service, queued.JobID)
		ids = append(ids, version.ID)
		sources = append(sources, retrieval.MediaSourceSelector{SourceID: retained.SourceID,
			SourceVersionID: queued.SourceVersionID, ContentVersionID: version.ID})
		if i == 0 {
			generatedCandidate = retained
		}
		if i == 100 {
			suppliedCandidate = retained
			active, err := f.catalog.ActiveRendition(t.Context(), version.ID, service.profiles[name].record.Fingerprint)
			require.NoError(t, err)
			selectedBuild = active.Build.ID
		}
	}
	hits, _, err := f.catalog.SearchExplainedLexicalCandidates(t.Context(), "Synthetic worker output", 100,
		store.SearchOptions{ContentVersionIDs: ids}, true)
	require.NoError(t, err)
	require.Len(t, hits, 100)
	for _, hit := range hits {
		require.NotEqual(t, selectedBuild, hit.BuildID, "the selected match is starved by nonselected heads")
	}
	raw := mediatest.WAV()
	raw[len(raw)-1] = 102
	pendingVersion, pendingSelector := f.addWAV(t, "pending.wav", raw)
	pending, err := service.SubmitSuppliedMedia(t.Context(), SuppliedMediaRequest{OperationID: uuid.New().String(),
		Filename: "pending.wav", MediaType: "audio/wav", SHA256: pendingVersion.BlobHash,
		ByteLength: pendingVersion.Size, ExistingContentVersionID: pendingVersion.ID,
		Occurrence: MediaOccurrenceInput{Ref: "pending", Revision: "1"}})
	require.NoError(t, err)
	text := []byte("pending unrelated text")
	input, err := service.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{OperationID: uuid.New().String(),
		SourceID: pending.SourceID, OccurrenceID: pending.OccurrenceID, Kind: "transcript", Filename: "pending.txt",
		MediaType: "text/plain", SHA256: processingSHA256(text), ByteLength: int64(len(text)), Content: bytes.NewReader(text)})
	require.NoError(t, err)
	pendingSelector.Profile = name
	plan, err := service.Plan(t.Context(), pendingSelector)
	require.NoError(t, err)
	_, err = service.GrantConsent(t.Context(), ConsentGrantRequest{Selector: pendingSelector, PlanFingerprint: plan.Fingerprint})
	require.NoError(t, err)
	pendingJob, err := service.RetryMedia(t.Context(), uuid.New().String(), pending.SourceID,
		MediaProcessingRequest{Profile: name, SuppliedInputID: input.SuppliedInputID})
	require.NoError(t, err)
	ids = append(ids, pendingVersion.ID)
	sources = append(sources, retrieval.MediaSourceSelector{SourceID: pending.SourceID, SourceVersionID: pending.SourceVersionID,
		ContentVersionID: pendingVersion.ID})
	last, _, err := f.catalog.SearchExplainedLexicalCandidates(t.Context(), "Synthetic worker output", 1,
		store.SearchOptions{ContentVersionIDs: ids[100:]}, true)
	require.NoError(t, err)
	require.Len(t, last, 1)
	require.NotEqual(t, selectedBuild, last[0].BuildID, "the older short head wins node deduplication")
	report, err := service.Search(t.Context(), SearchRequest{Query: "Synthetic worker output", Mode: "lexical",
		Profile: name, Limit: 100, Fence: SourceFence{VaultUID: f.catalog.VaultID(), ContentVersionIDs: ids},
		MediaSources: sources})
	require.NoError(t, err)
	require.True(t, report.MediaSourceSelection)
	require.Len(t, report.Results, 1)
	require.Equal(t, selectedBuild, report.Results[0].Evidence[0].BuildID)
	require.Equal(t, []retrieval.MediaSource{sources[100].Identity()}, report.Results[0].Evidence[0].MediaSources)
	require.Equal(t, "supplied", report.Results[0].Evidence[0].Origin)
	require.NotEmpty(t, report.Results[0].Evidence[0].SuppliedInputID)
	require.Equal(t, retrieval.CoverageIncomplete, report.Coverage.State)
	require.Equal(t, 102, report.Coverage.ScopedDocuments)
	require.Equal(t, 101, report.Coverage.CompleteDocuments)
	runLoomRenditionJob(t, service, pendingJob.JobID)
	empty, err := service.Search(t.Context(), SearchRequest{Query: "absentphrase", Mode: "lexical", Profile: name,
		Fence: SourceFence{VaultUID: f.catalog.VaultID(), ContentVersionIDs: ids}, MediaSources: sources})
	require.NoError(t, err)
	require.True(t, empty.MediaSourceSelection)
	require.Empty(t, empty.Results)
	require.Equal(t, retrieval.CoverageComplete, empty.Coverage.State)
	shared, err := service.Search(t.Context(), SearchRequest{Query: "Synthetic worker output", Mode: "auto", Profile: name,
		Fence:        SourceFence{VaultUID: f.catalog.VaultID(), ContentVersionIDs: ids[100:]},
		MediaSources: []retrieval.MediaSourceSelector{generatedSource, sources[100]}})
	require.NoError(t, err)
	require.Len(t, shared.Results, 2, "distinct selected builds on one node survive retrieval")
	require.NotEqual(t, shared.Results[0].Evidence[0].BuildID, shared.Results[1].Evidence[0].BuildID)

	queueTranscript := func(receipt MediaReceipt, text string) MediaReceipt {
		t.Helper()
		input, err := service.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{OperationID: uuid.New().String(), SourceID: receipt.SourceID, OccurrenceID: receipt.OccurrenceID, Kind: "transcript", Filename: "current.txt", MediaType: "text/plain", SHA256: processingSHA256([]byte(text)), ByteLength: int64(len(text)), Content: strings.NewReader(text)})
		require.NoError(t, err)
		queued, err := service.RetryMedia(t.Context(), uuid.New().String(), receipt.SourceID, MediaProcessingRequest{Profile: name, SuppliedInputID: input.SuppliedInputID})
		require.NoError(t, err)
		return queued
	}
	selectedA := queueTranscript(suppliedCandidate, "Synthetic")
	runLoomRenditionJob(t, service, selectedA.JobID)
	otherOccurrence := suppliedCandidate
	otherOccurrence.OccurrenceID = "second-source-occurrence"
	pendingB := queueTranscript(otherOccurrence, "replacement transcript")
	_, err = service.RetryMedia(t.Context(), uuid.New().String(), generatedCandidate.SourceID, MediaProcessingRequest{Profile: "speech"})
	require.NoError(t, err)
	queueTranscript(generatedCandidate, "queued provider transcript")
	for _, tc := range []struct {
		name     string
		allowed  []string
		supplied bool
	}{{"omitted", nil, true}, {"empty", []string{}, false}, {"pending_B", []string{pendingB.SuppliedInputID}, false}, {"shared_inputs", []string{pendingB.SuppliedInputID, selectedA.SuppliedInputID}, true}} {
		t.Run(tc.name, func(t *testing.T) {
			eligible := []retrieval.MediaSourceSelector{sources[0], sources[100]}
			eligible[0].SuppliedInputIDs = []string{}
			eligible[1].SuppliedInputIDs = tc.allowed
			report, err := service.Search(t.Context(), SearchRequest{Query: "Synthetic", Mode: "lexical", Profile: name, Limit: 1, Fence: SourceFence{VaultUID: f.catalog.VaultID(), ContentVersionIDs: []string{ids[0], ids[100]}}, MediaSources: eligible})
			require.NoError(t, err)
			require.Len(t, report.Results, 1)
			evidence := report.Results[0].Evidence[0]
			if tc.supplied {
				require.Equal(t, selectedA.SuppliedInputID, evidence.SuppliedInputID, "the stronger selected A wins when eligible")
				require.Equal(t, retrieval.CoverageComplete, report.Coverage.State)
			} else {
				require.Equal(t, "generated", evidence.Origin, "queued supplied work preserves ASR and excluded A consumes no rank budget")
				require.Equal(t, ids[0], report.Results[0].Document.ContentVersionID)
				require.Equal(t, retrieval.CoverageIncomplete, report.Coverage.State)
				require.Equal(t, 1, report.Coverage.CompleteDocuments)
			}
		})
	}
	bad := SearchRequest{Query: "Synthetic", Mode: "lexical", Profile: name, Fence: SourceFence{VaultUID: f.catalog.VaultID(), ContentVersionIDs: ids}, MediaSources: []retrieval.MediaSourceSelector{sources[0]}}
	bad.MediaSources[0].ContentVersionID = uuid.New().String()
	_, err = service.Search(t.Context(), bad)
	require.ErrorIs(t, err, ErrMediaSearchInvalid)
	bad.MediaSources = []retrieval.MediaSourceSelector{}
	_, err = service.Search(t.Context(), bad)
	require.ErrorIs(t, err, ErrMediaSearchInvalid)
	bad.MediaSources = slices.Concat(sources[:1], sources[:1])
	bad.MediaSources[1].SuppliedInputIDs = []string{processingHash("another-input")}
	_, err = service.Search(t.Context(), bad)
	require.ErrorIs(t, err, ErrMediaSearchInvalid)
}

type mediaSearchChangingBackend struct {
	*store.Store

	change func()
}

func (backend mediaSearchChangingBackend) SearchExplainedLexicalCandidates(ctx context.Context, query string, limit int,
	options store.SearchOptions, contentFirst bool) ([]store.ExplainedLexicalCandidate, bool, error) {
	hits, truncated, err := backend.Store.SearchExplainedLexicalCandidates(ctx, query, limit, options, contentFirst)
	backend.change()
	return hits, truncated, err
}

func TestMediaSearchKeepsHealthyMatchesDuringSourceChanges(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"unknown", "hidden", "deleted", "pending_to_ready", "changed_head", "shared_build_revoke"} {
		t.Run(change, func(t *testing.T) {
			f := newMediaStateFixture(t)
			healthy, err := f.service.SubmitSuppliedMedia(t.Context(), f.suppliedRequest(uuid.New().String(), &MediaProcessingRequest{Profile: "speech"}))
			require.NoError(t, err)
			require.NoError(t, f.run(t, healthy.JobID))
			version := f.version
			var changing MediaReceipt
			if change == "shared_build_revoke" {
				sourceID, err := store.MediaSourceKey("remote_recording", f.catalog.VaultID(), "synthetic", "shared", "second")
				require.NoError(t, err)
				identity := processingHash("shared-source")
				_, err = f.catalog.RetainMediaReference(t.Context(), store.MediaReferencePublicationRequest{Operation: store.MediaOperation{ID: uuid.New().String(), Principal: f.service.principal, Verb: "submit_remote_recording", RequestSHA256: identity, SourceID: sourceID}, Provider: "synthetic", OriginScope: "shared", IdentitySHA256: identity, Occurrence: store.MediaOccurrenceInput{ID: "second-occurrence", SourceID: sourceID, Principal: f.service.principal, Ref: "second", Revision: "1", Filename: "second.wav", MessageJSON: "{}"}})
				require.NoError(t, err)
				require.NoError(t, f.catalog.PublishMediaSourceVersion(t.Context(), store.MediaSourceVersionInput{ID: "second-version", SourceID: sourceID, ContentVersionID: healthy.ContentVersionID, CaptureJSON: "{}", Revision: 1, BindOccurrenceIDs: []string{"second-occurrence"}}))
				op := store.MediaOperation{ID: uuid.New().String(), Principal: f.service.principal, Verb: "retry_media", RequestSHA256: identity, SourceID: sourceID}
				_, err = f.catalog.QueueMediaRetry(t.Context(), op, store.MediaPublicationReceipt{VaultUID: f.catalog.VaultID(), SourceID: sourceID, SourceVersionID: "second-version", ContentVersionID: healthy.ContentVersionID, OccurrenceID: "second-occurrence", OperationID: op.ID, JobID: healthy.JobID, OperationState: "queued", CoverageState: "pending", ProcessingNodeID: f.version.NodeID, ProcessingProfile: "speech", ProcessingProfileFingerprint: f.service.profiles["speech"].record.Fingerprint})
				require.NoError(t, err)
				changing = MediaReceipt{SourceID: sourceID, SourceVersionID: "second-version", ContentVersionID: healthy.ContentVersionID}
			} else {
				raw := mediatest.WAV()
				raw[len(raw)-1] = 77
				var selector Selector
				version, selector = f.addWAV(t, "changing.wav", raw)
				plan, err := f.service.Plan(t.Context(), selector)
				require.NoError(t, err)
				_, err = f.service.GrantConsent(t.Context(), ConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
				require.NoError(t, err)
				changing, err = f.service.SubmitSuppliedMedia(t.Context(), SuppliedMediaRequest{OperationID: uuid.New().String(), Filename: "changing.wav", MediaType: "audio/wav", SHA256: version.BlobHash, ByteLength: version.Size, ExistingContentVersionID: version.ID, Occurrence: MediaOccurrenceInput{Ref: "changing", Revision: "1"}, Processing: &MediaProcessingRequest{Profile: "speech"}})
				require.NoError(t, err)
				if change != "pending_to_ready" {
					require.NoError(t, f.run(t, changing.JobID))
				}
			}
			request := SearchRequest{Query: "Synthetic worker output", Mode: "lexical", Profile: "speech", Fence: SourceFence{VaultUID: f.catalog.VaultID(), ContentVersionIDs: []string{healthy.ContentVersionID, changing.ContentVersionID}}, MediaSources: []retrieval.MediaSourceSelector{
				{SourceID: healthy.SourceID, SourceVersionID: healthy.SourceVersionID, ContentVersionID: healthy.ContentVersionID},
				{SourceID: changing.SourceID, SourceVersionID: changing.SourceVersionID, ContentVersionID: changing.ContentVersionID}}}
			if change == "unknown" {
				request.MediaSources[1].SourceID = "unknown"
			}
			if change == "shared_build_revoke" {
				request.Fence.ContentVersionIDs = []string{healthy.ContentVersionID}
			}
			prepared, err := f.service.prepareSearch(request, f.service.profiles["speech"])
			require.NoError(t, err)
			prepared.searcher, err = retrieval.NewSearcher(retrieval.SearcherConfig{Owner: "changing-source", LeaseDuration: time.Minute, Backend: mediaSearchChangingBackend{Store: f.catalog, change: func() {
				switch change {
				case "shared_build_revoke":
					_, err := f.service.RevokeMediaOccurrence(t.Context(), uuid.New().String(), healthy.OccurrenceID, "1")
					require.NoError(t, err)
				case "hidden":
					_, err := f.service.RevokeMediaOccurrence(t.Context(), uuid.New().String(), changing.OccurrenceID, "1")
					require.NoError(t, err)
				case "deleted":
					_, _, err := f.catalog.Trash(t.Context(), version.NodeID, store.UnconditionalRev)
					require.NoError(t, err)
					_, err = f.catalog.TrashEmpty(t.Context(), 0, true)
					require.NoError(t, err)
				case "pending_to_ready":
					require.NoError(t, f.run(t, changing.JobID))
				case "changed_head":
					view, err := f.catalog.ActiveRendition(t.Context(), version.ID, f.service.profiles["speech"].record.Fingerprint)
					require.NoError(t, err)
					view.Build.ID = processingHash("replacement-build")
					view.Build.ProviderOperationID = "synthetic-replacement"
					require.NoError(t, f.catalog.StageRenditionBuild(t.Context(), view.Build))
					generation, err := f.catalog.StageLexicalGeneration(t.Context(), processingHash("replacement-generation"))
					require.NoError(t, err)
					view.Attachment.ID, view.Attachment.BuildID = processingHash("replacement-attachment"), view.Build.ID
					view.Head.AttachmentID = view.Attachment.ID
					require.NoError(t, f.catalog.PublishRenditionAndLexicalHeads(t.Context(), view.Attachment, view.Head, generation.ID))
				}
			}}})
			require.NoError(t, err)
			report, err := f.service.searchMediaSources(t.Context(), request, request.Fence.ContentVersionIDs, prepared)
			require.NoError(t, err)
			require.Len(t, report.Results, 1)
			require.Equal(t, healthy.ContentVersionID, report.Results[0].Document.ContentVersionID)
			require.Equal(t, 1, report.Results[0].Rank)
			require.Equal(t, retrieval.CoverageIncomplete, report.Coverage.State)
			if change == "shared_build_revoke" {
				require.Equal(t, []retrieval.MediaSource{request.MediaSources[1].Identity()}, report.Results[0].Evidence[0].MediaSources)
				require.Zero(t, report.Coverage.CompleteDocuments)
			} else {
				require.Equal(t, 2, report.Coverage.ScopedDocuments)
				require.Equal(t, 1, report.Coverage.CompleteDocuments)
			}
		})
	}
}
