package api_test

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/report"
)

func TestTermReportRoutesWithoutStoreReturnUnavailable(t *testing.T) {
	_, catalog := newTestServer(t, func(d *api.Deps) { d.Store = nil })
	for _, request := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/search-exports", ""},
		{http.MethodGet, "/api/v1/search-exports/example", ""},
		{http.MethodPost, "/api/v1/search-exports/example/dates", `{}`},
		{http.MethodPost, "/api/v1/search-exports/example/revisions", `{"choices":[]}`},
		{http.MethodPost, "/api/v1/search-exports/example/download", `{"format":"csv"}`},
		{http.MethodGet, "/api/v1/search-exports/example/csv", ""},
		{http.MethodGet, "/api/v1/search-exports/example/bundle", ""},
	} {
		t.Run(request.method+request.path, func(t *testing.T) {
			req := httptest.NewRequest(request.method, request.path, strings.NewReader(request.body))
			req.Header.Set("X-Api-Key", testAPIKey)
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			require.NotPanics(t, func() { catalog.Server.Handler().ServeHTTP(response, req) })
			require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
		})
	}
}

func TestTermReportRoutesFreezeSummaryDatesAndDownload(t *testing.T) {
	ts, s := newTestServer(t, nil)
	createFileWithContent(t, ts, s, "/synthetic-alpha.txt", "synthetic alpha")
	request := report.Request{Version: 1, AllDocuments: true, Timezone: "UTC",
		CoverageMode: "available_only", Terms: []report.Term{{Number: 1,
			Expression: "alpha", Syntax: "simple", Dates: report.DateRange{Start: "2026-01-01", End: "2026-12-31"}}}}
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	endpoint := "/api/v1/search-exports"
	resp, body := rawJSONRequest(t, ts.URL, http.MethodPost, endpoint,
		map[string]string{"X-Api-Key": ""}, string(encoded))
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, body)
	resp, body = rawJSONRequest(t, ts.URL, http.MethodPost, endpoint,
		map[string]string{"X-Api-Key": testAPIKey}, string(encoded))
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var summary report.Summary
	require.NoError(t, json.Unmarshal([]byte(body), &summary))
	require.Equal(t, "complete", summary.State)
	require.NotEmpty(t, summary.ID)
	require.Len(t, summary.Counts, 1)
	require.Equal(t, int64(1), summary.Counts[0].Hits)
	require.NotContains(t, body, "synthetic-alpha.txt")
	resp, body = get(t, ts, endpoint, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var history struct {
		Items []struct {
			Request report.Request `json:"request"`
			Summary report.Summary `json:"summary"`
		} `json:"items"`
		Total int `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &history))
	require.Equal(t, 1, history.Total)
	require.Len(t, history.Items, 1)
	require.Equal(t, request, history.Items[0].Request)
	require.Equal(t, summary, history.Items[0].Summary)

	resp, body = rawJSONRequest(t, ts.URL, http.MethodPost, endpoint+"/"+summary.ID+"/dates",
		map[string]string{"X-Api-Key": testAPIKey}, `{}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var page report.DatePage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Len(t, page.Members, 1)
	require.NotEmpty(t, page.Members[0].Candidates)

	resp, body = get(t, ts, endpoint+"/"+summary.ID+"/csv", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Equal(t, summary.CSVBytes, int64(len(body)))
	require.Contains(t, body, "Unique Families")
	resp, bundle := get(t, ts, endpoint+"/"+summary.ID+"/bundle", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, summary.BundleBytes, int64(len(bundle)))
	verification, err := report.VerifyBundle(t.Context(), report.NewBudget(64<<20), bytes.NewReader([]byte(bundle)), int64(len(bundle)))
	require.NoError(t, err)
	require.True(t, verification.InternallyConsistent)
	require.False(t, verification.SourceVerified)

	resp, body = get(t, ts, endpoint+"/"+summary.ID+"-wrong", nil)
	require.Equal(t, http.StatusGone, resp.StatusCode, body)
}

func TestTermReportRejectsInvalidRequestAsClientError(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	request := `{"version":1,"all_documents":true,"timezone":"Invalid/Zone","coverage_mode":"strict",` +
		`"terms":[{"number":1,"expression":"alpha","syntax":"simple",` +
		`"dates":{"start":"2026-01-01","end":"2026-12-31"}}]}`
	response, body := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/search-exports",
		map[string]string{"X-Api-Key": testAPIKey}, request)
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	require.Contains(t, body, "invalid_report_request")

	request = `{"version":1,"all_documents":false,"collection_ids":["11111111-1111-4111-8111-111111111111"],` +
		`"timezone":"UTC","coverage_mode":"strict","terms":[{"number":1,"expression":"alpha",` +
		`"syntax":"simple","dates":{"start":"2026-01-01","end":"2026-12-31"}}]}`
	response, body = rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/search-exports",
		map[string]string{"X-Api-Key": testAPIKey}, request)
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	require.Contains(t, body, "invalid_report_scope")
}

func TestTermReportRejectsMalformedRevisionAsClientError(t *testing.T) {
	ts, s := newTestServer(t, nil)
	createFileWithContent(t, ts, s, "/synthetic-alpha.txt", "synthetic alpha")
	request := `{"version":1,"all_documents":true,"timezone":"UTC","coverage_mode":"available_only",` +
		`"terms":[{"number":1,"expression":"alpha","syntax":"simple",` +
		`"dates":{"start":"2026-01-01","end":"2026-12-31"}}]}`
	response, body := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/search-exports",
		map[string]string{"X-Api-Key": testAPIKey}, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var summary report.Summary
	require.NoError(t, json.Unmarshal([]byte(body), &summary))
	malformed, err := json.Marshal(struct {
		Choices []report.DateChoice `json:"choices"`
	}{Choices: []report.DateChoice{{Document: report.Identity{NodeID: 1, VersionID: "v1",
		SHA256: strings.Repeat("a", 64)}, CandidateID: "synthetic-date",
		EvidenceSHA256: strings.Repeat("b", 64), Reason: " ", Action: "select"}}})
	require.NoError(t, err)
	response, body = rawJSONRequest(t, ts.URL, http.MethodPost,
		"/api/v1/search-exports/"+summary.ID+"/revisions", map[string]string{"X-Api-Key": testAPIKey},
		string(malformed))
	require.Equal(t, http.StatusBadRequest, response.StatusCode, body)
	require.Contains(t, body, "invalid_report_choice")
	stale := strings.Replace(string(malformed), `"reason":" "`, `"reason":"Reviewed source"`, 1)
	response, body = rawJSONRequest(t, ts.URL, http.MethodPost,
		"/api/v1/search-exports/"+summary.ID+"/revisions", map[string]string{"X-Api-Key": testAPIKey}, stale)
	require.Equal(t, http.StatusConflict, response.StatusCode, body)
	require.Contains(t, body, "stale_evidence")
}

func TestTermReportDateLimitIdentifiesDocument(t *testing.T) {
	ts, catalog := newTestServer(t, nil)
	content := strings.Repeat("Document dated 2024-05-06. ", 257)
	node := createFileWithContent(t, ts, catalog, "/many-dates.txt", content)
	require.NoError(t, catalog.RecordExtraction(t.Context(), store.ExtractionResult{
		BlobHash: node.BlobHash, Extractor: "synthetic-native", ExtractorVersion: 1,
		Status: store.ExtractionOK, Text: content,
	}))
	request := `{"version":1,"all_documents":true,"timezone":"UTC","coverage_mode":"available_only",` +
		`"terms":[{"number":1,"expression":"alpha","syntax":"simple",` +
		`"dates":{"start":"2026-01-01","end":"2026-12-31"}}]}`
	response, body := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/search-exports",
		map[string]string{"X-Api-Key": testAPIKey}, request)
	require.Equal(t, http.StatusRequestEntityTooLarge, response.StatusCode, body)
	require.Contains(t, body, "report_limit")
	require.Contains(t, body, "document "+strconv.FormatInt(node.ID, 10))
}

func TestTermReportNativeTextLimitReturnsClientError(t *testing.T) {
	ts, catalog := newTestServer(t, nil)
	content := strings.Repeat("x", (16<<20)+1)
	node := createFileWithContent(t, ts, catalog, "/large-text.txt", content)
	require.NoError(t, catalog.RecordExtraction(t.Context(), store.ExtractionResult{
		BlobHash: node.BlobHash, Extractor: "synthetic-native", ExtractorVersion: 1,
		Status: store.ExtractionOK, Text: content,
	}))
	request := `{"version":1,"all_documents":true,"timezone":"UTC","coverage_mode":"available_only",` +
		`"terms":[{"number":1,"expression":"alpha","syntax":"simple",` +
		`"dates":{"start":"2026-01-01","end":"2026-12-31"}}]}`
	response, body := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/search-exports",
		map[string]string{"X-Api-Key": testAPIKey}, request)
	require.Equal(t, http.StatusRequestEntityTooLarge, response.StatusCode, body)
	require.Contains(t, body, "report_limit")
}

func TestTermReportOldDownloadStaysFrozenAfterSourceChange(t *testing.T) {
	ts, s := newTestServer(t, nil)
	node := createFileWithContent(t, ts, s, "/alpha.txt", "synthetic original")
	request := report.Request{Version: 1, AllDocuments: true, Timezone: "UTC",
		CoverageMode: "available_only", Terms: []report.Term{{Number: 1, Expression: "alpha",
			Syntax: "simple", Dates: report.DateRange{Start: "2026-01-01", End: "2026-12-31"}}}}
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	create := func() report.Summary {
		response, body := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/search-exports",
			map[string]string{"X-Api-Key": testAPIKey}, string(encoded))
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		var summary report.Summary
		require.NoError(t, json.Unmarshal([]byte(body), &summary))
		return summary
	}
	first := create()
	require.Equal(t, int64(1), first.Counts[0].Hits)
	response, frozenBundle := get(t, ts, "/api/v1/search-exports/"+first.ID+"/bundle", nil)
	require.Equal(t, http.StatusOK, response.StatusCode)
	_, _, err = s.Trash(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	createFileWithContent(t, ts, s, "/beta.txt", "synthetic replacement")
	second := create()
	require.NotEqual(t, first.ID, second.ID)
	require.Zero(t, second.Counts[0].Hits)
	response, stillFrozen := get(t, ts, "/api/v1/search-exports/"+first.ID+"/bundle", nil)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, frozenBundle, stillFrozen)
}

func TestTermReportBrowserTicketIsOneUseAndOwnerBound(t *testing.T) {
	ts, s := newTestServer(t, nil)
	createFileWithContent(t, ts, s, "/synthetic-alpha.txt", "synthetic alpha")
	sessionRequest, err := http.NewRequest(http.MethodPost, ts.URL+"/api/daemon/web-session", nil)
	require.NoError(t, err)
	sessionResponse, err := ts.Client().Do(sessionRequest)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, sessionResponse.StatusCode)
	var session struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.UnmarshalRead(sessionResponse.Body, &session))
	require.NoError(t, sessionResponse.Body.Close())
	webRequest := func(method, path, body string) (*http.Response, string) {
		t.Helper()
		request, requestErr := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		require.NoError(t, requestErr)
		request.Header["X-Api-Key"] = []string{""}
		request.Header.Set("X-Docbank-Web-Session", session.Token)
		if body != "" {
			request.Header.Set("Content-Type", "application/json")
		}
		response, requestErr := ts.Client().Do(request)
		require.NoError(t, requestErr)
		content, requestErr := io.ReadAll(response.Body)
		require.NoError(t, requestErr)
		require.NoError(t, response.Body.Close())
		return response, string(content)
	}
	request := report.Request{Version: 1, AllDocuments: true, Timezone: "UTC",
		CoverageMode: "available_only", Terms: []report.Term{{Number: 1,
			Expression: "alpha", Syntax: "simple", Dates: report.DateRange{Start: "2026-01-01", End: "2026-12-31"}}}}
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	resp, body := webRequest(http.MethodPost, "/api/v1/search-exports", string(encoded))
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var summary report.Summary
	require.NoError(t, json.Unmarshal([]byte(body), &summary))
	resp, body = webRequest(http.MethodGet, "/api/v1/search-exports/"+summary.ID+"/csv", "")
	require.Equal(t, http.StatusForbidden, resp.StatusCode, body)
	resp, body = webRequest(http.MethodPost, "/api/v1/search-exports/"+summary.ID+"/download", `{"format":"csv"}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var ticket struct {
		URL string `json:"url"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &ticket))
	require.Contains(t, ticket.URL, "ticket=")
	first, err := http.Get(ts.URL + ticket.URL)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, first.StatusCode)
	content, err := io.ReadAll(first.Body)
	require.NoError(t, err)
	require.NoError(t, first.Body.Close())
	require.Equal(t, summary.CSVBytes, int64(len(content)))
	second, err := http.Get(ts.URL + ticket.URL)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, second.StatusCode)
	require.NoError(t, second.Body.Close())
}
