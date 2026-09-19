package daemonconn_test

import (
	"encoding/json/v2"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

// These wire examples are also exercised by frontend/src/api.processing.test.ts.
func TestSharedProcessingResponseContract(t *testing.T) {
	type responseCase struct {
		Name  string         `json:"name"`
		Patch map[string]any `json:"patch"`
		Valid bool           `json:"valid"`
	}
	var fixture struct {
		Job         api.ProcessingJob         `json:"job"`
		Status      map[string]any            `json:"status"`
		Request     api.DocumentSearchRequest `json:"request"`
		Report      api.DocumentSearchReport  `json:"report"`
		StatusCases []responseCase            `json:"status_cases"`
		SearchCases []responseCase            `json:"search_cases"`
	}
	data, err := os.ReadFile("testdata/processing_responses.json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &fixture))
	for _, tc := range fixture.StatusCases {
		t.Run("status/"+tc.Name, func(t *testing.T) {
			status := maps.Clone(fixture.Status)
			maps.Copy(status, tc.Patch)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					assert.NoError(t, json.MarshalWrite(w, status))
					return
				}
				w.Header().Set("Content-Type", "application/x-ndjson")
				assert.NoError(t, json.MarshalWrite(w, api.ProcessingJobEvent{Sequence: 1, Type: "job", Job: &fixture.Job}))
				assert.NoError(t, json.MarshalWrite(w, map[string]any{"sequence": 2, "type": "status", "terminal": true, "status": status}))
			}))
			t.Cleanup(server.Close)
			c := daemonconn.New(server.URL, serverKey)
			got, err := c.ProcessingStatus(t.Context(), fixture.Job.ID)
			if tc.Valid {
				require.NoError(t, err)
				assert.Equal(t, status["state"], got.State)
			} else {
				require.Error(t, err)
				assert.Empty(t, got)
			}
			stream, err := c.StartProcessingStream(t.Context(), api.StartProcessingRequest{Selector: api.ProcessingSelector{ContentVersionID: fixture.Job.ContentVersionID}}, fixture.Job.ProfileFingerprint)
			require.NoError(t, err)
			defer func() { _ = stream.Close() }()
			_, err = stream.Next()
			require.NoError(t, err)
			event, err := stream.Next()
			if !tc.Valid {
				require.ErrorContains(t, err, "malformed terminal status")
				assert.Nil(t, event.Status)
			} else {
				require.NotNil(t, event.Status, "valid failure states retain their status alongside an error")
				assert.Equal(t, status["state"], event.Status.State)
			}
		})
	}
	for _, tc := range fixture.SearchCases {
		t.Run("search/"+tc.Name, func(t *testing.T) {
			encoded, err := json.Marshal(fixture.Report.Results[0])
			require.NoError(t, err)
			var result map[string]any
			require.NoError(t, json.Unmarshal(encoded, &result))
			maps.Copy(result, tc.Patch)
			report := fixture.Report
			encoded, err = json.Marshal(result)
			require.NoError(t, err)
			report.Results = make([]api.DocumentSearchResult, 1)
			require.NoError(t, json.Unmarshal(encoded, &report.Results[0]))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				assert.NoError(t, json.MarshalWrite(w, report))
			}))
			t.Cleanup(server.Close)
			_, err = daemonconn.New(server.URL, serverKey).SearchDocuments(t.Context(), fixture.Request)
			if tc.Valid {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "search response")
			}
		})
	}
}

func TestSharedSimilarResponseContract(t *testing.T) {
	var fixture struct {
		Request api.DocumentSimilarRequest `json:"similar_request"`
		Report  map[string]any             `json:"similar_report"`
		Cases   []struct {
			Name  string         `json:"name"`
			Patch map[string]any `json:"patch"`
			Valid bool           `json:"valid"`
		} `json:"similar_cases"`
	}
	data, err := os.ReadFile("testdata/processing_responses.json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &fixture))
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			report := maps.Clone(fixture.Report)
			maps.Copy(report, tc.Patch)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/v1/search/similar", r.URL.Path)
				assert.NoError(t, json.MarshalWrite(w, report))
			}))
			t.Cleanup(server.Close)
			_, err := daemonconn.New(server.URL, serverKey).SimilarDocuments(t.Context(), fixture.Request)
			if tc.Valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
