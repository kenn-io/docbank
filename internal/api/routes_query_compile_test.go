package api_test

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

const queryParsePath = "/api/v1/queries/parse"
const queryHighlightsPath = "/api/v1/queries/highlights"

func TestQueryCompilePreviewRetainsIntentAndDependencies(t *testing.T) {
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
	ts, s := newTestServer(t, nil)
	resp, body := rawJSONRequest(t, ts.URL, http.MethodPost, queryParsePath, nil, `{}`)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, body)
	for _, text := range []string{`{"syntax":"advanced","text":"😀 AND"}`, `{"syntax":"advanced","text":"NOT tag:missing"}`} {
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

func TestQueryCompilePreviewBrowserCapability(t *testing.T) {
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
}

func TestQueryHighlightPreviewIsAvailableToBoundedBrowserSessions(t *testing.T) {
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
