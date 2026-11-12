package api_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"testing"
	_ "time/tzdata"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

const queryParsePath = "/api/v1/queries/parse"
const queryHighlightsPath = "/api/v1/queries/highlights"

func TestQueryCompilePreviewRetainsIntentAndDependencies(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	saved, _ := createSavedQuery(t, ts.URL, "Choice", `{"syntax":"advanced","text":"alpha OR beta"}`)
	resp, body := rawJSONRequest(t, ts.URL, http.MethodPost, queryParsePath,
		map[string]string{"X-Api-Key": testAPIKey},
		`{"syntax":"advanced","text":"  name:gamma OR saved:Choice  ","filters":{"size_min":7}}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var preview struct {
		Query struct {
			Text    string         `json:"text"`
			Filters map[string]any `json:"filters"`
		} `json:"query"`
		Fingerprint  string `json:"query_fingerprint"`
		Dependencies []struct {
			Kind     string `json:"kind"`
			ID       string `json:"id"`
			Revision int64  `json:"revision"`
		} `json:"dependencies"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &preview))
	require.Equal(t, "  name:gamma OR saved:Choice  ", preview.Query.Text)
	require.Len(t, preview.Query.Filters, 1)
	require.InDelta(t, float64(7), preview.Query.Filters["size_min"], 0)
	require.True(t, strings.HasPrefix(preview.Fingerprint, "sha256:"))
	require.Len(t, preview.Dependencies, 1)
	require.Equal(t, saved.ID, preview.Dependencies[0].ID)
	require.Equal(t, int64(1), preview.Dependencies[0].Revision)
	require.Equal(t, "saved", preview.Dependencies[0].Kind)
	require.NotContains(t, body, "SELECT ")
}

func TestQueryCompilePreviewErrorsAndAuthentication(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	resp, body := rawJSONRequest(t, ts.URL, http.MethodPost, queryParsePath, nil, `{}`)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, body)
	for _, text := range []string{`{"syntax":"advanced","text":"😀 AND"}`, `{"syntax":"advanced","text":"NOT tag:missing"}`, `{"syntax":"advanced","text":"unevaluated:false"}`, `{"syntax":"advanced","text":"unevaluated:\"false\""}`, `{"syntax":"advanced","text":"unevaluated:(true OR false)"}`} {
		resp, body = rawJSONRequest(t, ts.URL, http.MethodPost, queryParsePath,
			map[string]string{"X-Api-Key": testAPIKey}, text)
		require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
		var problem struct {
			Code     string `json:"code"`
			Position *struct {
				Offset int `json:"offset"`
				End    int `json:"end"`
			} `json:"position"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &problem))
		require.Equal(t, "invalid_query", problem.Code)
		require.NotNil(t, problem.Position)
		require.Greater(t, problem.Position.End, problem.Position.Offset)
		if strings.Contains(text, "😀") {
			require.Equal(t, 5, problem.Position.Offset)
			require.Equal(t, 8, problem.Position.End)
		}
	}
	for _, input := range []string{`{"text":"a","text":"b"}`, `{"filters":{"invented":true}}`} {
		resp, body = rawJSONRequest(t, ts.URL, http.MethodPost, queryParsePath,
			map[string]string{"X-Api-Key": testAPIKey}, input)
		require.GreaterOrEqual(t, resp.StatusCode, 400, body)
		require.Less(t, resp.StatusCode, 500, body)
	}
	resp, body = rawJSONRequest(t, ts.URL, http.MethodPost, queryParsePath,
		map[string]string{"X-Api-Key": testAPIKey}, "{"+strings.Repeat(" ", 128<<10)+"}")
	require.GreaterOrEqual(t, resp.StatusCode, 400, body)
	require.Less(t, resp.StatusCode, 500, body)
	require.NoError(t, s.Close())
	resp, body = rawJSONRequest(t, ts.URL, http.MethodPost, queryParsePath,
		map[string]string{"X-Api-Key": testAPIKey}, `{"syntax":"advanced","text":"tag:known"}`)
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode, body)
	require.Equal(t, "internal", decodeProblem(t, body).Code)
}

func TestQueryCompilePreviewFilterErrorsHaveNoExpressionPosition(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	for _, field := range []string{"tag_ids", "exclude_tag_ids", "collection_ids", "exclude_collection_ids"} {
		for _, text := range []string{"alpha", ""} {
			t.Run(field+"/"+text, func(t *testing.T) {
				resp, body := rawJSONRequest(t, ts.URL, http.MethodPost, queryParsePath,
					map[string]string{"X-Api-Key": testAPIKey}, fmt.Sprintf(
						`{"text":%q,"filters":{%q:["11111111-1111-4111-8111-111111111111"]}}`, text, field))
				require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
				var problem api.Error
				require.NoError(t, json.Unmarshal([]byte(body), &problem))
				require.Equal(t, "invalid_query", problem.Code)
				require.Nil(t, problem.Position, body)
			})
		}
	}
}

func TestQueryCompilePreviewBrowserCapability(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	resp, body := do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var session struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &session))
	headers := map[string]string{api.WebSessionHeader: session.Token}
	resp, body = rawJSONRequest(t, ts.URL, http.MethodPost, queryParsePath, headers, `{"text":"alpha"}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	resp, body = rawJSONRequest(t, ts.URL, http.MethodPost, queryParsePath+"?unrecognized=1", headers, `{}`)
	require.Equal(t, http.StatusForbidden, resp.StatusCode, body)
}

func TestQueryHighlightPreviewReturnsOnlyCompilerDerivedPositiveTextTerms(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	saved, _ := createSavedQuery(t, ts.URL, "Choice",
		`{"syntax":"advanced","text":"nested OR NOT excluded OR name:caption"}`)
	resp, body := rawJSONRequest(t, ts.URL, http.MethodPost, queryHighlightsPath,
		map[string]string{"X-Api-Key": testAPIKey},
		`{"syntax":"advanced","text":"alpha AND NOT hidden AND extension:pdf AND saved:Choice"}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var preview struct {
		QueryFingerprint string `json:"query_fingerprint"`
		Dependencies     []struct {
			Kind     string `json:"kind"`
			ID       string `json:"id"`
			Revision int64  `json:"revision"`
		} `json:"dependencies"`
		Terms []string `json:"terms"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &preview))
	require.True(t, strings.HasPrefix(preview.QueryFingerprint, "sha256:"))
	require.Equal(t, []string{"alpha", "nested"}, preview.Terms)
	require.Len(t, preview.Dependencies, 1)
	require.Equal(t, saved.ID, preview.Dependencies[0].ID)
	require.Equal(t, int64(1), preview.Dependencies[0].Revision)
	require.Equal(t, "saved", preview.Dependencies[0].Kind)
	require.NotContains(t, body, "hidden")
	require.NotContains(t, body, "caption")

	// All three receipts identify submitted intent even when execution defaults a zone.
	input := `{"syntax":"advanced","text":"date:2026-03-01 AND alpha"}`
	var fingerprint string
	for _, path := range []string{queryParsePath, queryHighlightsPath} {
		response, payload := rawJSONRequest(t, ts.URL, http.MethodPost, path, map[string]string{"X-Api-Key": testAPIKey}, input)
		require.Equal(t, http.StatusOK, response.StatusCode, payload)
		var receipt struct {
			QueryFingerprint string `json:"query_fingerprint"`
		}
		require.NoError(t, json.Unmarshal([]byte(payload), &receipt))
		if fingerprint == "" {
			fingerprint = receipt.QueryFingerprint
		} else {
			require.Equal(t, fingerprint, receipt.QueryFingerprint)
		}
	}
	response, payload := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/workspace/queries", map[string]string{"X-Api-Key": testAPIKey}, `{"query":`+input+`}`)
	require.Equal(t, http.StatusOK, response.StatusCode, payload)
	var snapshot api.WorkspaceQueryResponse
	require.NoError(t, json.Unmarshal([]byte(payload), &snapshot))
	require.Equal(t, fingerprint, snapshot.QueryFingerprint)
	require.Equal(t, "UTC", snapshot.Timezone)
	require.True(t, snapshot.TimezoneDefaulted)

	saved, etag := createSavedQuery(t, ts.URL, "Pinned date", input)
	response, payload = rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/saved-queries/"+saved.ID+"/runs", map[string]string{"X-Api-Key": testAPIKey, "If-Match": etag}, `{}`)
	require.Equal(t, http.StatusOK, response.StatusCode, payload)
	var run api.SavedQueryRunResult
	require.NoError(t, json.Unmarshal([]byte(payload), &run))
	require.Equal(t, "UTC", run.Snapshot.Timezone)
	require.False(t, run.Snapshot.TimezoneDefaulted)
	require.Equal(t, saved.Fingerprint, run.Snapshot.QueryFingerprint)
}

func TestQueryHighlightPreviewIsAvailableToBoundedBrowserSessions(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	resp, body := do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var session struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &session))

	headers := map[string]string{api.WebSessionHeader: session.Token}
	resp, body = rawJSONRequest(t, ts.URL, http.MethodPost, queryHighlightsPath,
		headers, `{"text":"alpha beta"}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	resp, body = rawJSONRequest(t, ts.URL, http.MethodPost,
		queryHighlightsPath+"?unrecognized=1", headers, `{}`)
	require.Equal(t, http.StatusForbidden, resp.StatusCode, body)
}

func TestQueryCompileTimelineErrors(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	for _, tc := range []struct {
		input, code string
		positioned  bool
	}{
		{`{"timezone":"Local"}`, "invalid_timezone", false},
		{`{"syntax":"advanced","text":"date:2026-02-30"}`, "invalid_date", true},
		{`{"syntax":"advanced","text":"date:2011-12-30","timezone":"Pacific/Apia"}`, "date_not_in_zone", true},
		{`{"filters":{"document_date":{"from":"2026-02-30","to":"2026-03-01"}}}`, "invalid_date", false},
		{`{"filters":{"document_date":{"from":"0001-01-01","to":"0001-01-01"}},"timezone":"Asia/Singapore"}`, "invalid_date", false},
		{`{"filters":{"document_date":{"from":"0000-01-01","to":"0000-01-01"}}}`, "invalid_date", false},
		{`{"filters":{"document_date":{"from":"9999-12-31","to":"9999-12-31"}},"timezone":"UTC"}`, "invalid_date", false},
	} {
		for _, path := range []string{queryParsePath, queryHighlightsPath} {
			resp, body := rawJSONRequest(t, ts.URL, http.MethodPost, path, map[string]string{"X-Api-Key": testAPIKey}, tc.input)
			require.Equal(t, http.StatusBadRequest, resp.StatusCode, body)
			problem := decodeProblem(t, body)
			require.Equal(t, tc.code, problem.Code)
			if tc.positioned {
				require.NotNil(t, problem.Position)
				require.Equal(t, 0, problem.Position.Offset)
				require.Equal(t, len("date:2026-02-30"), problem.Position.End)
			}
		}
	}
	for _, path := range []string{queryParsePath, queryHighlightsPath} {
		resp, body := rawJSONRequest(t, ts.URL, http.MethodPost, path, map[string]string{"X-Api-Key": testAPIKey}, `{"syntax":"advanced","text":"alpha AND date:2026-03-31..2026-03-01"}`)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, body)
		problem := decodeProblem(t, body)
		require.Equal(t, "invalid_date", problem.Code)
		require.Equal(t, "date range is reversed", problem.Detail)
		require.NotNil(t, problem.Position)
		require.Equal(t, len("alpha AND "), problem.Position.Offset)
		require.Equal(t, len("alpha AND date:2026-03-31..2026-03-01"), problem.Position.End)
	}
	response, body := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/saved-queries", map[string]string{"X-Api-Key": testAPIKey}, `{"name":"Invalid date","kind":"query","payload":{"filters":{"document_date":{"from":"2026-02-30","to":"2026-03-01"}}}}`)
	require.Equal(t, http.StatusBadRequest, response.StatusCode, body)
	require.Equal(t, "invalid_date", decodeProblem(t, body).Code)
}

func TestFullDateDisclosureRequiresNonBrowserAPIKey(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	resp, body := do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var session struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &session))
	input := `{"query":{},"disclosure":"full"}`
	for _, headers := range []map[string]string{
		{api.WebSessionHeader: session.Token},
	} {
		resp, body = rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/workspace/queries", headers, input)
		require.Equal(t, http.StatusForbidden, resp.StatusCode, body)
	}
	resp, body = rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/workspace/queries", map[string]string{"X-Api-Key": testAPIKey}, input)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
}
