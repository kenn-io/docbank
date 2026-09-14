package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestWorkspaceQueryRoutesCreateAndPageFrozenResults(t *testing.T) {
	ts, s := newTestServer(t, nil)
	for i := range 51 {
		createFileWithContent(t, ts, s, fmt.Sprintf("/%02d.txt", i), fmt.Sprintf("synthetic-%02d", i))
	}

	for _, pageSize := range []int{50, 100, 250} {
		resp, body := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/workspace/queries",
			map[string]string{"X-Api-Key": testAPIKey}, fmt.Sprintf(
				`{"query":{},"page_size":%d,"facets":["size"]}`, pageSize))
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		var page api.WorkspaceQueryResponse
		require.NoError(t, json.Unmarshal([]byte(body), &page))
		require.True(t, page.Snapshot)
		require.Equal(t, pageSize, page.PageSize)
		require.EqualValues(t, 51, page.Total)
		require.Len(t, page.Rows, min(pageSize, 51))
		require.NotEmpty(t, page.SnapshotID)
		if pageSize != 50 {
			continue
		}
		require.Empty(t, page.PreviousCursor)
		require.NotEmpty(t, page.NextCursor)
		allRows := slices.Clone(page.Rows)

		resp, body = rawJSONRequest(t, ts.URL, http.MethodPost,
			"/api/v1/workspace/queries/"+page.SnapshotID+"/pages",
			map[string]string{"X-Api-Key": testAPIKey}, fmt.Sprintf(`{"cursor":%q}`, page.NextCursor))
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		var last api.WorkspaceQueryResponse
		require.NoError(t, json.Unmarshal([]byte(body), &last))
		require.Len(t, last.Rows, 1)
		require.NotEmpty(t, last.PreviousCursor)
		require.Empty(t, last.NextCursor)
		allRows = append(allRows, last.Rows...)

		resp, body = rawJSONRequest(t, ts.URL, http.MethodPost,
			"/api/v1/workspace/queries/"+page.SnapshotID+"/pages",
			map[string]string{"X-Api-Key": testAPIKey}, fmt.Sprintf(`{"cursor":%q}`, last.PreviousCursor))
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		var firstAgain api.WorkspaceQueryResponse
		require.NoError(t, json.Unmarshal([]byte(body), &firstAgain))
		require.Equal(t, page.Rows, firstAgain.Rows)

		var members strings.Builder
		slices.SortFunc(allRows, func(a, b api.WorkspaceQueryRow) int {
			if a.NodeID < b.NodeID {
				return -1
			}
			if a.NodeID > b.NodeID {
				return 1
			}
			return strings.Compare(a.ContentVersionID, b.ContentVersionID)
		})
		for _, row := range allRows {
			_, _ = fmt.Fprintf(&members, "%d:%s\n", row.NodeID, row.ContentVersionID)
		}
		digest := sha256.Sum256([]byte(members.String()))
		copiedMemberHash := hex.EncodeToString(digest[:])
		require.Equal(t, page.MemberHash, copiedMemberHash)
	}
}

func TestWorkspaceQueryRoutesStrictBodiesAndStableErrors(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	headers := map[string]string{"X-Api-Key": testAPIKey}
	for _, test := range []struct {
		name, path, body, code string
		status                 int
	}{
		{"unknown envelope field", "/api/v1/workspace/queries", `{"query":{},"future":true}`, "validation", 422},
		{"duplicate query field", "/api/v1/workspace/queries", `{"query":{"text":"a","text":"b"}}`, "validation", 400},
		{"positioned invalid query", "/api/v1/workspace/queries", `{"query":{"text":"(","syntax":"advanced"}}`, "invalid_query", 422},
		{"unsupported page size", "/api/v1/workspace/queries", `{"query":{},"page_size":51}`, "validation", 422},
		{"unknown facet", "/api/v1/workspace/queries", `{"query":{},"facets":["future"]}`, "validation", 422},
		{"missing snapshot", "/api/v1/workspace/queries/00000000000000000000000000000000/pages", `{"cursor":"eA"}`, "snapshot_gone", 410},
		{"empty cursor", "/api/v1/workspace/queries/00000000000000000000000000000000/pages", `{"cursor":""}`, "validation", 422},
	} {
		t.Run(test.name, func(t *testing.T) {
			resp, body := rawJSONRequest(t, ts.URL, http.MethodPost, test.path, headers, test.body)
			require.Equal(t, test.status, resp.StatusCode, body)
			problem := decodeProblem(t, body)
			assert.Equal(t, test.code, problem.Code)
			if test.name == "positioned invalid query" {
				require.NotNil(t, problem.Position)
				assert.Positive(t, problem.Position.End)
			}
		})
	}

	createdResp, createdBody := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/workspace/queries",
		headers, `{"query":{},"page_size":50}`)
	require.Equal(t, http.StatusOK, createdResp.StatusCode, createdBody)
	var created api.WorkspaceQueryResponse
	require.NoError(t, json.Unmarshal([]byte(createdBody), &created))
	resp, body := rawJSONRequest(t, ts.URL, http.MethodPost,
		"/api/v1/workspace/queries/"+created.SnapshotID+"/pages", headers, `{"cursor":"tampered"}`)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, body)
	assert.Equal(t, "invalid_cursor", decodeProblem(t, body).Code)
}

func TestWorkspaceQueryErrorsOnlyPositionExpressionOperands(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	for _, payload := range []string{
		`{"text":"alpha","filters":{"paths":["relative"]}}`,
		`{"filters":{"size_min":2,"size_max":1}}`,
		`{"filters":{"tag_ids":["11111111-1111-4111-8111-111111111111"]}}`,
	} {
		t.Run(payload, func(t *testing.T) {
			resp, body := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/workspace/queries",
				map[string]string{"X-Api-Key": testAPIKey}, `{"query":`+payload+`}`)
			require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
			problem := decodeProblem(t, body)
			assert.Equal(t, "invalid_query", problem.Code)
			assert.Nil(t, problem.Position, body)
		})
	}
}

func TestQueryRoutesRejectScalarPrefixWildcards(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	for _, text := range []string{`text_coverage:complete*`, `has_duplicates:true*`} {
		for _, path := range []string{queryParsePath, "/api/v1/workspace/queries"} {
			t.Run(path+"/"+text, func(t *testing.T) {
				payload := fmt.Sprintf(`{"syntax":"advanced","text":%q}`, text)
				if path != queryParsePath {
					payload = `{"query":` + payload + `}`
				}
				resp, body := rawJSONRequest(t, ts.URL, http.MethodPost, path,
					map[string]string{"X-Api-Key": testAPIKey}, payload)
				require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
				problem := decodeProblem(t, body)
				assert.Equal(t, "invalid_query", problem.Code)
				require.NotNil(t, problem.Position)
				assert.Equal(t, strings.IndexByte(text, ':')+1, problem.Position.Offset)
				assert.Equal(t, len(text), problem.Position.End)
			})
		}
	}
}

func TestWorkspaceQueryRoutesReportCanceledAndExpiredRequests(t *testing.T) {
	ts, s := newTestServer(t, nil)
	saved, _ := createSavedQuery(t, ts.URL, "Canceled synthetic query", `{}`)
	for _, test := range []struct{ name, detail string }{
		{"canceled", "canceled"},
		{"deadline", "resource budget"},
	} {
		for _, path := range []string{"/api/v1/workspace/queries", "/api/v1/saved-queries/" + saved.ID + "/runs"} {
			t.Run(test.name+"/"+path, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				if test.name == "deadline" {
					cancel()
					ctx, cancel = context.WithDeadline(t.Context(), time.Unix(0, 0))
				}
				cancel()
				payload := `{"query":{}}`
				if strings.Contains(path, "saved-queries") {
					payload = `{}`
				}
				req := httptest.NewRequestWithContext(ctx, http.MethodPost, path, strings.NewReader(payload))
				req.Header.Set("X-Api-Key", testAPIKey)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("If-Match", `"1"`)
				response := httptest.NewRecorder()
				s.Server.Handler().ServeHTTP(response, req)
				require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
				problem := decodeProblem(t, response.Body.String())
				assert.Equal(t, "snapshot_unavailable", problem.Code)
				assert.Contains(t, problem.Detail, test.detail)
			})
		}
	}
}

func TestWorkspaceQueryRoutesBindMasterAndRevocableBrowserOwners(t *testing.T) {
	ts, s := newTestServer(t, nil)
	for i := range 51 {
		_, err := s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("owner-%02d.txt", i),
			testHash(fmt.Sprintf("owner-%02d", i)), 1, "text/plain")
		require.NoError(t, err)
	}
	issued, body := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/daemon/web-session",
		map[string]string{"X-Api-Key": testAPIKey}, "")
	require.Equal(t, http.StatusCreated, issued.StatusCode, body)
	var session struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &session))
	web := map[string]string{"X-Api-Key": "", api.WebSessionHeader: session.Token}

	createdResp, createdBody := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/workspace/queries",
		web, `{"query":{},"page_size":50}`)
	require.Equal(t, http.StatusOK, createdResp.StatusCode, createdBody)
	var browserPage api.WorkspaceQueryResponse
	require.NoError(t, json.Unmarshal([]byte(createdBody), &browserPage))

	resp, responseBody := rawJSONRequest(t, ts.URL, http.MethodPost,
		"/api/v1/workspace/queries/"+browserPage.SnapshotID+"/pages",
		map[string]string{"X-Api-Key": testAPIKey}, `{"cursor":"tampered"}`)
	require.Equal(t, http.StatusGone, resp.StatusCode, responseBody, "wrong owners are concealed before cursor parsing")

	resp, responseBody = rawJSONRequest(t, ts.URL, http.MethodDelete, "/api/daemon/web-session", web, "")
	require.Equal(t, http.StatusNoContent, resp.StatusCode, responseBody)
	issued, body = rawJSONRequest(t, ts.URL, http.MethodPost, "/api/daemon/web-session",
		map[string]string{"X-Api-Key": testAPIKey}, "")
	require.Equal(t, http.StatusCreated, issued.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &session))
	web = map[string]string{"X-Api-Key": "", api.WebSessionHeader: session.Token}
	resp, responseBody = rawJSONRequest(t, ts.URL, http.MethodPost,
		"/api/v1/workspace/queries/"+browserPage.SnapshotID+"/pages", web, `{"cursor":"tampered"}`)
	require.Equal(t, http.StatusGone, resp.StatusCode, responseBody)

	// Master-first precedence means a request carrying both credentials remains
	// in the daemon's master owner bucket and survives browser revocation.
	createdResp, createdBody = rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/workspace/queries",
		map[string]string{"X-Api-Key": testAPIKey, api.WebSessionHeader: session.Token},
		`{"query":{},"page_size":50}`)
	require.Equal(t, http.StatusOK, createdResp.StatusCode, createdBody)
	var masterPage api.WorkspaceQueryResponse
	require.NoError(t, json.Unmarshal([]byte(createdBody), &masterPage))
	require.NotEmpty(t, masterPage.NextCursor)
	resp, responseBody = rawJSONRequest(t, ts.URL, http.MethodDelete, "/api/daemon/web-session", web, "")
	require.Equal(t, http.StatusNoContent, resp.StatusCode, responseBody)
	resp, responseBody = rawJSONRequest(t, ts.URL, http.MethodPost,
		"/api/v1/workspace/queries/"+masterPage.SnapshotID+"/pages",
		map[string]string{"X-Api-Key": testAPIKey}, fmt.Sprintf(`{"cursor":%q}`, masterPage.NextCursor))
	require.Equal(t, http.StatusOK, resp.StatusCode, responseBody)
}

func TestSavedQueryRunRouteRequiresRevisionAndUsesSavedDefinition(t *testing.T) {
	ts, s := newTestServer(t, nil)
	createFileWithContent(t, ts, s, "/matching.txt", "needle")
	resp, body := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/saved-queries/not-a-uuid/runs",
		map[string]string{"X-Api-Key": testAPIKey, "If-Match": `"1"`}, `{"page_size":50}`)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	assert.Equal(t, "invalid_saved_query_run", decodeProblem(t, body).Code)

	saved, _ := createSavedQuery(t, ts.URL, "All synthetic files", `{}`)
	path := "/api/v1/saved-queries/" + saved.ID + "/runs"

	resp, body = rawJSONRequest(t, ts.URL, http.MethodPost, path,
		map[string]string{"X-Api-Key": testAPIKey}, `{"page_size":50}`)
	require.Equal(t, http.StatusPreconditionRequired, resp.StatusCode, body)

	resp, body = rawJSONRequest(t, ts.URL, http.MethodPost, path,
		map[string]string{"X-Api-Key": testAPIKey, "If-Match": `"1"`}, `{"page_size":50}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var result api.SavedQueryRunResult
	require.NoError(t, json.Unmarshal([]byte(body), &result))
	assert.Equal(t, saved.ID, result.Run.SavedQueryID)
	assert.EqualValues(t, 1, result.Run.SavedQueryRevision)
	assert.Nil(t, result.Run.PreviousTotal)
	require.Len(t, result.Snapshot.Rows, 1)
	assert.Equal(t, "matching.txt", result.Snapshot.Rows[0].Name)
	assert.Equal(t, result.Run.SnapshotID, result.Snapshot.SnapshotID)

	resp, body = rawJSONRequest(t, ts.URL, http.MethodPost, path,
		map[string]string{"X-Api-Key": testAPIKey, "If-Match": `"2"`}, `{"page_size":50}`)
	require.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, body)
	assert.Equal(t, "stale_revision", decodeProblem(t, body).Code)
}

func TestSavedQueryRunRouteConcealsMissingAndHighlightDefinitions(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp, body := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/saved-queries",
		map[string]string{"X-Api-Key": testAPIKey},
		`{"name":"Synthetic highlights","kind":"highlight_set","payload":{"terms":[{"text":"needle","color":"#123456"}]}}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var highlight api.SavedQuery
	require.NoError(t, json.Unmarshal([]byte(body), &highlight))
	for _, id := range []string{"11111111-1111-4111-8111-111111111111", highlight.ID} {
		t.Run(id, func(t *testing.T) {
			resp, body := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/saved-queries/"+id+"/runs",
				map[string]string{"X-Api-Key": testAPIKey, "If-Match": `"1"`}, `{"page_size":50}`)
			require.Equal(t, http.StatusNotFound, resp.StatusCode, body)
			assert.Equal(t, "not_found", decodeProblem(t, body).Code)
		})
	}
}

func TestWorkspaceQueryServerShutdownClearsSnapshotAuthority(t *testing.T) {
	ts, s := newTestServer(t, nil)
	for i := range 51 {
		_, err := s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("shutdown-%02d.txt", i),
			testHash(fmt.Sprintf("shutdown-%02d", i)), 9, "text/plain")
		require.NoError(t, err)
	}
	resp, body := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/workspace/queries",
		map[string]string{"X-Api-Key": testAPIKey}, `{"query":{},"page_size":50}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var page api.WorkspaceQueryResponse
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.NotEmpty(t, page.NextCursor)

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, s.Server.Shutdown(ctx))

	resp, body = rawJSONRequest(t, ts.URL, http.MethodPost,
		"/api/v1/workspace/queries/"+page.SnapshotID+"/pages",
		map[string]string{"X-Api-Key": testAPIKey}, fmt.Sprintf(`{"cursor":%q}`, page.NextCursor))
	require.Equal(t, http.StatusGone, resp.StatusCode, body)
	assert.Equal(t, "snapshot_gone", decodeProblem(t, body).Code)
	resp, body = rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/workspace/queries",
		map[string]string{"X-Api-Key": testAPIKey}, `{"query":{}}`)
	require.Equal(t, http.StatusGone, resp.StatusCode, body)
}

func TestWorkspaceQueryPageKeepsFrozenVersionAcrossConcurrentReplacement(t *testing.T) {
	ts, s := newTestServer(t, nil)
	for i := range 51 {
		_, err := s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("cross-%02d.txt", i),
			testHash(fmt.Sprintf("cross-%02d", i)), int64(i+1), "text/plain")
		require.NoError(t, err)
	}
	last, err := s.NodeByPath(t.Context(), "/cross-50.txt")
	require.NoError(t, err)
	originalVersion := last.CurrentVersionID

	resp, body := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/workspace/queries",
		map[string]string{"X-Api-Key": testAPIKey}, `{"query":{},"page_size":50}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var first api.WorkspaceQueryResponse
	require.NoError(t, json.Unmarshal([]byte(body), &first))
	require.NotEmpty(t, first.NextCursor)
	last, replacement, err := s.ReplaceContent(t.Context(), last.ID, last.Revision,
		testHash("cross-replacement"), 999, "text/plain")
	require.NoError(t, err)
	require.NotEqual(t, originalVersion, replacement.ID)

	resp, body = rawJSONRequest(t, ts.URL, http.MethodPost,
		"/api/v1/workspace/queries/"+first.SnapshotID+"/pages",
		map[string]string{"X-Api-Key": testAPIKey}, fmt.Sprintf(`{"cursor":%q}`, first.NextCursor))
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var frozenLast api.WorkspaceQueryResponse
	require.NoError(t, json.Unmarshal([]byte(body), &frozenLast))
	require.Len(t, frozenLast.Rows, 1)
	assert.Equal(t, originalVersion, frozenLast.Rows[0].ContentVersionID)

	resp, body = rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/workspace/queries",
		map[string]string{"X-Api-Key": testAPIKey}, `{"query":{},"page_size":50}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var refreshedFirst api.WorkspaceQueryResponse
	require.NoError(t, json.Unmarshal([]byte(body), &refreshedFirst))
	resp, body = rawJSONRequest(t, ts.URL, http.MethodPost,
		"/api/v1/workspace/queries/"+refreshedFirst.SnapshotID+"/pages",
		map[string]string{"X-Api-Key": testAPIKey}, fmt.Sprintf(`{"cursor":%q}`, refreshedFirst.NextCursor))
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var refreshedLast api.WorkspaceQueryResponse
	require.NoError(t, json.Unmarshal([]byte(body), &refreshedLast))
	require.Len(t, refreshedLast.Rows, 1)
	assert.Equal(t, last.CurrentVersionID, refreshedLast.Rows[0].ContentVersionID)
}
