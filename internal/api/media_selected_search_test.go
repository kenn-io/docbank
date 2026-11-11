package api_test

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
	"strings"
	"testing"
	"unicode/utf8"
)

func (f *mediaQualificationFixture) request(profile, mode string, records ...mediaQualificationRecord) api.DocumentSearchRequest {
	request := api.DocumentSearchRequest{Query: "telescope Friday", Mode: mode, Profile: profile, Limit: 10, Fence: api.DocumentSourceFence{VaultUID: f.manifest.VaultUID}, MediaSources: api.MediaSearchSources{}}
	for _, record := range records {
		request.Fence.ContentVersionIDs = append(request.Fence.ContentVersionIDs, record.ContentVersionID)
		source := api.DocumentMediaSourceSelector{SourceID: record.SourceID, SourceVersionID: record.SourceVersionID, ContentVersionID: record.ContentVersionID}
		if record.Origin == "supplied" {
			source.SuppliedInputIDs = []string{record.SuppliedInputID}
		}
		request.MediaSources = append(request.MediaSources, source)
	}
	return request
}

func TestSelectedMediaSearchRealProcessing(t *testing.T) {
	t.Parallel()
	f := newMediaQualificationFixture(t)
	client := daemonconn.New(f.server.URL, f.manifest.APIKey)
	for _, mode := range []string{"lexical", "semantic", "hybrid"} {
		for _, record := range f.manifest.Records {
			t.Run(mode+"/"+record.ID, func(t *testing.T) {
				request := f.request(record.Profile, mode, record)
				if record.ID == "c-v1" {
					request.Query = "grocery"
				}
				if record.ID == "a-v2" {
					request.Query = "Monday"
				}
				report, err := client.SearchDocuments(t.Context(), request)
				require.NoError(t, err)
				require.Equal(t, mode, report.ActualMode)
				require.Equal(t, "complete", report.Coverage.State)
				require.Len(t, report.Results, 1)
				require.NotEmpty(t, report.Results[0].Excerpt)
				require.Len(t, report.MediaSelections, 1)
				require.Equal(t, record.Origin, report.MediaSelections[0].Origin)
				require.NotEmpty(t, report.MediaSelections[0].BuildID)
				for _, evidence := range report.Results[0].Evidence {
					require.Equal(t, report.MediaSelections[0].BuildID, evidence.BuildID)
					require.Len(t, evidence.MediaSources, 1)
					require.Equal(t, record.SourceID, evidence.MediaSources[0].SourceID)
					if evidence.Kind == "embedding" {
						require.NotEmpty(t, evidence.InputID)
						require.NotEmpty(t, evidence.InputGenerationID)
						require.NotEmpty(t, evidence.EmbeddingSetID)
						require.Empty(t, evidence.SegmentID)
					}
				}
			})
		}
	}
}

func TestSelectedMediaSearchProfileGapAndInputFence(t *testing.T) {
	t.Parallel()
	f := newMediaQualificationFixture(t)
	client := daemonconn.New(f.server.URL, f.manifest.APIKey)
	supplied, generated := f.manifest.Records[0], f.manifest.Records[2]
	for _, mode := range []string{"semantic", "hybrid"} {
		report, err := client.SearchDocuments(t.Context(), f.request(supplied.Profile, mode, supplied, generated))
		require.NoError(t, err)
		require.Equal(t, "incomplete", report.Coverage.State)
		require.Equal(t, 2, report.Coverage.ScopedDocuments)
		require.Equal(t, 1, report.Coverage.CompleteDocuments)
		for _, hit := range report.Results {
			for _, evidence := range hit.Evidence {
				if evidence.Kind == "embedding" {
					require.Equal(t, supplied.ContentVersionID, evidence.MediaSources[0].ContentVersionID)
				}
			}
		}
		request := f.request(supplied.Profile, mode, supplied)
		request.MediaSources[0].SuppliedInputIDs = []string{}
		f.encoder.mu.Lock()
		before := f.encoder.calls
		f.encoder.mu.Unlock()
		report, err = client.SearchDocuments(t.Context(), request)
		require.NoError(t, err)
		require.Empty(t, report.Results)
		require.Equal(t, "incomplete", report.Coverage.State)
		require.Equal(t, mode, report.RequestedMode)
		require.Equal(t, "lexical", report.ActualMode)
		require.Contains(t, report.Degradations, "semantic_unavailable")
		f.encoder.mu.Lock()
		require.Equal(t, before, f.encoder.calls, "ineligible scope must not encode query")
		f.encoder.mu.Unlock()
	}
}

func TestSelectedMediaSearchReselectsDuringEncoding(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"replace", "hide", "revoke"} {
		t.Run(action, func(t *testing.T) {
			f := newMediaQualificationFixture(t)
			record := f.manifest.Records[0]
			require.NoError(t, f.control(t.Context(), "arm", ""))
			type outcome struct {
				report api.DocumentSearchReport
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				report, err := daemonconn.New(f.server.URL, f.manifest.APIKey).SearchDocuments(t.Context(), f.request(record.Profile, "semantic", record))
				done <- outcome{report, err}
			}()
			select {
			case <-f.encoder.entered:
			case early := <-done:
				require.NoError(t, early.err)
				t.Fatal("query finished before entering the encoder barrier")
			}
			require.NoError(t, f.control(t.Context(), action, record.ID))
			require.NoError(t, f.control(t.Context(), "release", ""))
			result := <-done
			// A changed vector publication can fail closed before a report exists.
			// A returned report must drop the affected source after query encoding.
			require.Empty(t, result.report.Results)
			if result.err != nil {
				require.ErrorContains(t, result.err, "stale_index")
			} else {
				require.Equal(t, "incomplete", result.report.Coverage.State)
				require.Zero(t, result.report.Coverage.CompleteDocuments)
			}
		})
	}
}

func TestSelectedMediaSearchCoverageIntersectsLateSelection(t *testing.T) {
	t.Parallel()
	f := newMediaQualificationFixture(t)
	supplied, generated := f.manifest.Records[0], f.manifest.Records[2]
	require.NoError(t, f.control(t.Context(), "arm", ""))
	type outcome struct {
		report api.DocumentSearchReport
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		report, err := daemonconn.New(f.server.URL, f.manifest.APIKey).SearchDocuments(t.Context(), f.request(generated.Profile, "semantic", supplied, generated))
		done <- outcome{report, err}
	}()
	select {
	case <-f.encoder.entered:
	case early := <-done:
		require.NoError(t, early.err)
		t.Fatal("query finished before entering the encoder barrier")
	}
	require.NoError(t, f.control(t.Context(), "revoke", generated.ID))
	require.NoError(t, f.control(t.Context(), "release", ""))
	result := <-done
	require.NoError(t, result.err)
	require.Empty(t, result.report.Results)
	require.Equal(t, 2, result.report.Coverage.ScopedDocuments)
	require.Zero(t, result.report.Coverage.CompleteDocuments, "the surviving transcript and the embedded build belong to different sources")
	require.Equal(t, "incomplete", result.report.Coverage.State)
}

func TestSelectedMediaSearchRequiresQueryConsent(t *testing.T) {
	t.Parallel()
	f := newMediaQualificationFixture(t)
	_, err := f.service.RevokeConsent(t.Context())
	require.NoError(t, err)
	f.encoder.mu.Lock()
	before := f.encoder.calls
	f.encoder.mu.Unlock()
	record := f.manifest.Records[0]
	_, err = daemonconn.New(f.server.URL, f.manifest.APIKey).SearchDocuments(t.Context(), f.request(record.Profile, "semantic", record))
	require.Error(t, err)
	require.Contains(t, err.Error(), "consent")
	f.encoder.mu.Lock()
	defer f.encoder.mu.Unlock()
	require.Equal(t, before, f.encoder.calls, "denied query must not reach encoder")
}

func TestSelectedMediaSearchBoundsAuthenticatedMultibyteExcerpt(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("观测星空🌌 ", 300)
	f := newMediaQualificationFixture(t, mediaQualificationOptions{TranscriptA: text, ChunkTokens: 8192})
	record := f.manifest.Records[0]
	client := daemonconn.New(f.server.URL, f.manifest.APIKey)
	for _, mode := range []string{"semantic", "hybrid"} {
		report, err := client.SearchDocuments(t.Context(), f.request(record.Profile, mode, record))
		require.NoError(t, err)
		require.Len(t, report.Results, 1)
		excerpt := report.Results[0].Excerpt
		require.NotEmpty(t, excerpt)
		require.True(t, utf8.ValidString(excerpt))
		require.LessOrEqual(t, utf8.RuneCountInString(excerpt), 512)
		require.LessOrEqual(t, len(excerpt), 2048)
		require.True(t, strings.HasPrefix(text, excerpt))
		require.Equal(t, "embedding", report.Results[0].Evidence[0].Kind)
		require.Equal(t, report.MediaSelections[0].BuildID, report.Results[0].Evidence[0].BuildID)
		require.Nil(t, report.Results[0].Evidence[0].TimeSpan, "untimed source stays untimed")
	}
	transcript, err := f.service.MediaTranscript(t.Context(), processing.MediaTranscriptRequest{SourceID: record.SourceID, SourceVersionID: record.SourceVersionID, ContentVersionID: record.ContentVersionID})
	require.NoError(t, err)
	require.Equal(t, text, transcript.Transcript.Units[0].Text, "display bound must not truncate retained evidence")
}

func TestSelectedMediaSearchSkipsUnavailableEmbeddings(t *testing.T) {
	t.Parallel()
	f := newMediaQualificationFixture(t)
	client := daemonconn.New(f.server.URL, f.manifest.APIKey)
	supplied, generated := f.manifest.Records[0], f.manifest.Records[2]
	for _, mode := range []string{"semantic", "hybrid"} {
		f.encoder.mu.Lock()
		before := f.encoder.calls
		f.encoder.mu.Unlock()
		report, err := client.SearchDocuments(t.Context(), f.request(supplied.Profile, mode, generated))
		require.NoError(t, err)
		require.Equal(t, mode, report.RequestedMode)
		require.Equal(t, "incomplete", report.Coverage.State)
		require.Zero(t, report.Coverage.CompleteDocuments)
		require.Len(t, report.MediaSelections, 1)
		require.NotEmpty(t, report.MediaSelections[0].BuildID)
		require.Equal(t, "lexical", report.ActualMode)
		require.Contains(t, report.Degradations, "semantic_unavailable")
		require.Len(t, report.Results, 1)
		require.Equal(t, "rendition_segment", report.Results[0].Evidence[0].Kind)
		f.encoder.mu.Lock()
		require.Equal(t, before, f.encoder.calls, "no eligible vector row must skip query encoding")
		f.encoder.mu.Unlock()
	}
	// A scope that cannot perform any retrieval must not acquire query egress.
	_, err := f.service.RevokeConsent(t.Context())
	require.NoError(t, err)
	request := f.request(supplied.Profile, "semantic", supplied)
	request.MediaSources[0].SuppliedInputIDs = []string{}
	report, err := client.SearchDocuments(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, "lexical", report.ActualMode)
	require.Contains(t, report.Degradations, "semantic_unavailable")
}

func TestSelectedMediaSearchFallsBackForUnpersistedEmbeddingDonor(t *testing.T) {
	t.Parallel()
	f := newMediaQualificationFixture(t, mediaQualificationOptions{UnprocessedDonor: true})
	client := daemonconn.New(f.server.URL, f.manifest.APIKey)
	record := f.manifest.Records[0]
	for _, mode := range []string{"semantic", "hybrid"} {
		f.unprocessedEncoder.mu.Lock()
		before := f.unprocessedEncoder.calls
		f.unprocessedEncoder.mu.Unlock()
		report, err := client.SearchDocuments(t.Context(), f.request(f.unprocessedProfile, mode, record))
		require.NoError(t, err)
		require.Equal(t, mode, report.RequestedMode)
		require.Equal(t, "incomplete", report.Coverage.State)
		require.Equal(t, 1, report.Coverage.ScopedDocuments)
		require.Zero(t, report.Coverage.CompleteDocuments)
		require.Equal(t, "lexical", report.ActualMode)
		require.Contains(t, report.Degradations, "semantic_unavailable")
		require.Len(t, report.Results, 1)
		require.Equal(t, "rendition_segment", report.Results[0].Evidence[0].Kind)
		f.unprocessedEncoder.mu.Lock()
		require.Equal(t, before, f.unprocessedEncoder.calls, "an unpersisted donor has no eligible vector rows to query")
		f.unprocessedEncoder.mu.Unlock()
	}
}

func TestSelectedMediaSearchFallsBackWithoutEmbeddingBinding(t *testing.T) {
	t.Parallel()
	f := newMediaQualificationFixture(t)
	client := daemonconn.New(f.server.URL, f.manifest.APIKey)
	for _, mode := range []string{"semantic", "hybrid"} {
		report, err := client.SearchDocuments(t.Context(), f.request("qualification-unbound", mode, f.manifest.Records[0]))
		require.NoError(t, err)
		require.Equal(t, mode, report.RequestedMode)
		require.Equal(t, "lexical", report.ActualMode)
		require.Contains(t, report.Degradations, "semantic_unavailable")
		require.Len(t, report.Results, 1)
		require.Equal(t, "incomplete", report.Coverage.State)
	}
}

func TestSelectedMediaSearchFencesDistractorsBeforeLaneLimit(t *testing.T) {
	t.Parallel()
	f := newMediaQualificationFixture(t, mediaQualificationOptions{LaneLimit: 1, Distractors: 2})
	client := daemonconn.New(f.server.URL, f.manifest.APIKey)
	target := f.manifest.Records[1] // Monday vector is orthogonal; three supplied Friday distractors outrank it.
	for _, mode := range []string{"semantic", "hybrid"} {
		wide := f.request(target.Profile, mode, f.manifest.Records...)
		wide.Limit = 1
		control, err := client.SearchDocuments(t.Context(), wide)
		require.NoError(t, err)
		require.Len(t, control.Results, 1)
		require.NotEqual(t, target.ContentVersionID, control.Results[0].ContentVersionID)
		require.True(t, control.Truncated)
		request := f.request(target.Profile, mode, target)
		request.Limit = 1
		request.Fence = wide.Fence
		report, err := client.SearchDocuments(t.Context(), request)
		require.NoError(t, err)
		require.Len(t, report.Results, 1)
		require.Equal(t, mode, report.ActualMode)
		require.Equal(t, target.ContentVersionID, report.Results[0].ContentVersionID)
		require.Equal(t, "complete", report.Coverage.State)
		require.Equal(t, 1, report.Coverage.ScopedDocuments)
		require.False(t, report.Truncated)
		for _, evidence := range report.Results[0].Evidence {
			require.Equal(t, report.MediaSelections[0].BuildID, evidence.BuildID)
			require.Len(t, evidence.MediaSources, 1)
			require.Equal(t, target.SourceID, evidence.MediaSources[0].SourceID)
		}
	}
}
