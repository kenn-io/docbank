package api_test

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/report"
)

func createReleaseReport(t *testing.T, ts *httptest.Server,
	catalog *testStore, review bool,
) report.Summary {
	t.Helper()
	content := "alpha. Document dated 2024-05-06."
	if review {
		content += " Document dated 2024-06-07."
	}
	node := createFileWithContent(t, ts, catalog, "/synthetic-alpha.txt", content)
	require.NoError(t, catalog.RecordExtraction(t.Context(), store.ExtractionResult{
		BlobHash: node.BlobHash, Extractor: "synthetic-native", ExtractorVersion: 1,
		Status: store.ExtractionOK, Text: content,
	}))
	request := report.Request{Version: 1, AllDocuments: true, Timezone: "UTC",
		CoverageMode: "available_only", Terms: []report.Term{{Number: 1,
			Expression: "alpha", Syntax: "simple",
			Dates: report.DateRange{Start: "2024-01-01", End: "2024-12-31"}}}}
	response, body := do(t, ts, http.MethodPost, "/api/v1/search-exports", nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var summary report.Summary
	require.NoError(t, json.Unmarshal([]byte(body), &summary))
	return summary
}

func TestTermReportReleaseRoutes(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"complete", "needs_review"} {
		t.Run(state, func(t *testing.T) {
			ts, catalog := newTestServer(t, nil)
			summary := createReleaseReport(t, ts, catalog, state == "needs_review")
			require.Equal(t, state, summary.State)
			_, history := get(t, ts, "/api/v1/search-exports", nil)
			path := "/api/v1/search-exports/" + summary.ID
			response, body := rawJSONRequest(t, ts.URL, http.MethodDelete, path,
				map[string]string{"X-Api-Key": ""}, "")
			require.Equal(t, http.StatusUnauthorized, response.StatusCode, body)
			response, body = do(t, ts, http.MethodDelete, path, nil, nil)
			require.Equal(t, http.StatusNoContent, response.StatusCode, body)
			require.Empty(t, body)
			for _, request := range []struct{ method, suffix, body string }{
				{http.MethodDelete, "", ""}, {http.MethodGet, "", ""},
				{http.MethodPost, "/dates", `{}`},
				{http.MethodPost, "/revisions", `{"choices":[]}`},
				{http.MethodGet, "/bundle", ""}, {http.MethodGet, "/csv", ""},
			} {
				response, body := rawJSONRequest(t, ts.URL, request.method, path+request.suffix,
					map[string]string{"X-Api-Key": testAPIKey}, request.body)
				require.Equal(t, http.StatusGone, response.StatusCode, body)
				require.Contains(t, body, `"code":"report_unavailable"`)
			}
			_, retainedHistory := get(t, ts, "/api/v1/search-exports", nil)
			require.Equal(t, history, retainedHistory, "release/refused revision must not alter history")
		})
	}
	ts, _ := newTestServer(t, nil)
	for _, id := range []string{"invalid", strings.Repeat("a", 47), strings.Repeat("A", 48)} {
		response, body := do(t, ts, http.MethodDelete, "/api/v1/search-exports/"+id, nil, nil)
		require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	}
	response, body := do(t, ts, http.MethodDelete,
		"/api/v1/search-exports/"+strings.Repeat("a", 48), nil, nil)
	require.Equal(t, http.StatusGone, response.StatusCode, body)
	unavailable, _ := newTestServer(t, func(d *api.Deps) { d.Store = nil })
	response, body = do(t, unavailable, http.MethodDelete,
		"/api/v1/search-exports/"+strings.Repeat("a", 48), nil, nil)
	require.Equal(t, http.StatusServiceUnavailable, response.StatusCode, body)
	require.Contains(t, body, `"code":"report_unavailable"`)
}

type reportDownloadBarrier struct {
	*httptest.ResponseRecorder
	once    sync.Once
	entered chan struct{}
	resume  chan struct{}
}

func (w *reportDownloadBarrier) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.entered); <-w.resume })
	return w.ResponseRecorder.Write(data)
}

func TestTermReportReleaseDuringDownload(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, nil)
	summary := createReleaseReport(t, ts, catalog, false)
	path := "/api/v1/search-exports/" + summary.ID
	writer := &reportDownloadBarrier{ResponseRecorder: httptest.NewRecorder(),
		entered: make(chan struct{}), resume: make(chan struct{})}
	resume := sync.OnceFunc(func() { close(writer.resume) })
	finished := make(chan struct{})
	t.Cleanup(func() { resume(); <-finished })
	request := httptest.NewRequest(http.MethodGet, path+"/bundle", nil)
	request.Header.Set("X-Api-Key", testAPIKey)
	go func() {
		defer close(finished)
		catalog.Server.Handler().ServeHTTP(writer, request)
	}()
	<-writer.entered
	response, body := do(t, ts, http.MethodDelete, path, nil, nil)
	require.Equal(t, http.StatusConflict, response.StatusCode, body)
	require.Contains(t, body, `"code":"report_retained"`)
	resume()
	<-finished
	require.Equal(t, http.StatusOK, writer.Code)
	budget := report.NewBudget(16 << 20)
	defer func() { require.NoError(t, budget.Close()) }()
	verified, err := report.VerifyBundle(t.Context(), budget,
		bytes.NewReader(writer.Body.Bytes()), int64(writer.Body.Len()))
	require.NoError(t, err)
	require.True(t, verified.InternallyConsistent)
	response, body = do(t, ts, http.MethodDelete, path, nil, nil)
	require.Equal(t, http.StatusNoContent, response.StatusCode, body)
}
