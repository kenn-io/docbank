package processing

import (
	"bytes"
	"context"
	"fmt"
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
	var sources []retrieval.MediaSource
	var selectedBuild string
	var generatedSource retrieval.MediaSource
	for i := 0; i < 101; i++ {
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
			generatedSource = retrieval.MediaSource{SourceID: retained.SourceID,
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
		sources = append(sources, retrieval.MediaSource{SourceID: retained.SourceID,
			SourceVersionID: queued.SourceVersionID, ContentVersionID: version.ID})
		if i == 100 {
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
	sources = append(sources, retrieval.MediaSource{SourceID: pending.SourceID, SourceVersionID: pending.SourceVersionID,
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
	require.Equal(t, []retrieval.MediaSource{sources[100]}, report.Results[0].Evidence[0].MediaSources)
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
		MediaSources: []retrieval.MediaSource{generatedSource, sources[100]}})
	require.NoError(t, err)
	require.Len(t, shared.Results, 2, "distinct selected builds on one node survive retrieval")
	require.NotEqual(t, shared.Results[0].Evidence[0].BuildID, shared.Results[1].Evidence[0].BuildID)
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

func TestMediaSearchRejectsChangedAndUnauthorizedSelections(t *testing.T) {
	f := newCaptionFixture(t, "search-authority")
	queued := f.caption(t, f.remote.OccurrenceID, "selected cue")
	runLoomRenditionJob(t, f.captions, queued.JobID)
	request := SearchRequest{Query: "selected", Mode: "lexical", Profile: f.profile,
		Fence: SourceFence{VaultUID: f.fixture.catalog.VaultID(), ContentVersionIDs: []string{f.videoReceipt.ContentVersionID}},
		MediaSources: []retrieval.MediaSource{{SourceID: f.remote.SourceID, SourceVersionID: queued.SourceVersionID,
			ContentVersionID: f.videoReceipt.ContentVersionID}}}
	bad := request
	bad.MediaSources = []retrieval.MediaSource{{SourceID: f.remote.SourceID, SourceVersionID: queued.SourceVersionID,
		ContentVersionID: uuid.New().String()}}
	_, err := f.captions.Search(t.Context(), bad)
	require.ErrorIs(t, err, ErrMediaSearchInvalid)
	bad.MediaSources = []retrieval.MediaSource{{SourceID: "hidden-source", SourceVersionID: queued.SourceVersionID,
		ContentVersionID: f.videoReceipt.ContentVersionID}}
	_, err = f.captions.Search(t.Context(), bad)
	require.ErrorIs(t, err, store.ErrNotFound)
	bad.MediaSources = append(request.MediaSources, request.MediaSources...)
	_, err = f.captions.Search(t.Context(), bad)
	require.ErrorIs(t, err, ErrMediaSearchInvalid)
	prepared, err := f.captions.prepareSearch(request, f.captions.profiles[f.profile])
	require.NoError(t, err)
	prepared.searcher, err = retrieval.NewSearcher(retrieval.SearcherConfig{Owner: "search-authority", LeaseDuration: time.Minute,
		Backend: mediaSearchChangingBackend{Store: f.fixture.catalog, change: func() {
			next := f.caption(t, f.remote.OccurrenceID, "replacement cue")
			runLoomRenditionJob(t, f.captions, next.JobID)
		}}})
	require.NoError(t, err)
	_, err = f.captions.searchMediaSources(t.Context(), request, request.Fence.ContentVersionIDs, prepared)
	require.ErrorIs(t, err, ErrMediaSearchUnavailable)
}
