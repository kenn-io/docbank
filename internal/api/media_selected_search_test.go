package api_test

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"testing"
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
					require.Equal(t, supplied.ContentVersionID, evidence.ContentVersionID)
				}
			}
		}
		request := f.request(supplied.Profile, mode, supplied)
		request.MediaSources[0].SuppliedInputIDs = []string{}
		report, err = client.SearchDocuments(t.Context(), request)
		require.NoError(t, err)
		require.Empty(t, report.Results)
		require.Equal(t, "incomplete", report.Coverage.State)
	}
}
