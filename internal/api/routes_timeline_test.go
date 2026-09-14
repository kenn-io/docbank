package api_test

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

func TestTimelineRebuildRejectsNonCanonicalUUIDv4BeforeStore(t *testing.T) {
	ts, s := newTestServer(t, nil)
	var before bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &before))
	rejected := []string{
		"10000000-0000-1000-8000-000000000001",
		"70000000-0000-7000-8000-000000000007",
		"ABCDEFAB-CDEF-4ABC-8ABC-ABCDEFABCDEF",
		"10000000000040008000000000000001",
	}
	for _, operationID := range rejected {
		t.Run(operationID, func(t *testing.T) {
			response, body := rawJSONRequest(t, ts.URL, http.MethodPost,
				"/api/v1/timeline/rebuilds", map[string]string{"X-Api-Key": testAPIKey},
				`{"operation_id":"`+operationID+`"}`)
			assert.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
			assert.Equal(t, "validation", decodeProblem(t, body).Code)

			response, body = rawJSONRequest(t, ts.URL, http.MethodGet,
				"/api/v1/timeline/rebuilds/"+operationID,
				map[string]string{"X-Api-Key": testAPIKey}, "")
			assert.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
			assert.Equal(t, "validation", decodeProblem(t, body).Code)
		})
	}
	var after bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &after))
	assert.Equal(t, before.Bytes(), after.Bytes(),
		"rejected operation IDs must not create state or rebuild receipts")
}

func TestTimelineRebuildIsIdempotentByOperationID(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	const operationID = "10000000-0000-4000-8000-000000000001"
	requestBody := `{"operation_id":"` + operationID + `"}`

	response, body := rawJSONRequest(t, ts.URL, http.MethodPost,
		"/api/v1/timeline/rebuilds", map[string]string{"X-Api-Key": testAPIKey}, requestBody)
	require.Equal(t, http.StatusAccepted, response.StatusCode, body)
	var first api.TimelineBuild
	require.NoError(t, json.Unmarshal([]byte(body), &first))
	assert.Equal(t, operationID, first.OperationID)
	assert.Equal(t, "running", first.State)
	assert.NotEmpty(t, first.StartedAt)

	response, body = rawJSONRequest(t, ts.URL, http.MethodPost,
		"/api/v1/timeline/rebuilds", map[string]string{"X-Api-Key": testAPIKey}, requestBody)
	require.Equal(t, http.StatusAccepted, response.StatusCode, body)
	var replayed api.TimelineBuild
	require.NoError(t, json.Unmarshal([]byte(body), &replayed))
	assert.Equal(t, first.OperationID, replayed.OperationID)
	assert.Equal(t, first.StartedAt, replayed.StartedAt)

	response, body = rawJSONRequest(t, ts.URL, http.MethodGet,
		"/api/v1/timeline/rebuilds/"+operationID,
		map[string]string{"X-Api-Key": testAPIKey}, "")
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var status api.TimelineBuild
	require.NoError(t, json.Unmarshal([]byte(body), &status))
	assert.Equal(t, replayed, status)

	response, body = rawJSONRequest(t, ts.URL, http.MethodPost,
		"/api/v1/timeline/rebuilds", map[string]string{"X-Api-Key": testAPIKey},
		`{"operation_id":"`+operationID+`","after_version_id":"ignored"}`)
	assert.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	assert.Equal(t, "validation", decodeProblem(t, body).Code)
	response, body = rawJSONRequest(t, ts.URL, http.MethodPost,
		"/api/v1/timeline/rebuilds", map[string]string{"X-Api-Key": testAPIKey},
		`{"operation_id":"not-a-uuid"}`)
	assert.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	assert.Equal(t, "validation", decodeProblem(t, body).Code)
	response, body = rawJSONRequest(t, ts.URL, http.MethodPost,
		"/api/v1/timeline/rebuilds", map[string]string{"X-Api-Key": testAPIKey},
		requestBody+strings.Repeat(" ", 4<<10))
	assert.Equal(t, http.StatusRequestEntityTooLarge, response.StatusCode, body)

	response, body = rawJSONRequest(t, ts.URL, http.MethodGet,
		"/api/v1/timeline/rebuilds/20000000-0000-4000-8000-000000000002",
		map[string]string{"X-Api-Key": testAPIKey}, "")
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
	assert.Equal(t, "not_found", decodeProblem(t, body).Code)

	response, body = rawJSONRequest(t, ts.URL, http.MethodGet,
		"/api/v1/timeline/coverage", map[string]string{"X-Api-Key": testAPIKey}, "")
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var coverage api.DocumentEventCoverage
	require.NoError(t, json.Unmarshal([]byte(body), &coverage))
	assert.Equal(t, coverage.Selected,
		coverage.Pending+coverage.Indexed+coverage.Failed+coverage.Unavailable)
	assert.NotContains(t, body, "processing_coverage")

	c := client.New(ts.URL, testAPIKey)
	clientBuild, err := c.TimelineRebuild(t.Context(), operationID)
	require.NoError(t, err)
	assert.Equal(t, first.StartedAt, clientBuild.StartedAt)
	clientStatus, err := c.TimelineRebuildStatus(t.Context(), operationID)
	require.NoError(t, err)
	assert.Equal(t, clientBuild, clientStatus)
	clientCoverage, err := c.TimelineCoverage(t.Context())
	require.NoError(t, err)
	assert.Equal(t, coverage, clientCoverage)
}

func TestTimelineCoverageRouteDoesNotInitializeState(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	response, body := rawJSONRequest(t, ts.URL, http.MethodGet,
		"/api/v1/timeline/coverage", map[string]string{"X-Api-Key": testAPIKey}, "")
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Contains(t, body, `"selected":0`)
}

func TestTimelineClientRejectsMismatchedOperationID(t *testing.T) {
	const requested = "30000000-0000-4000-8000-000000000003"
	const returned = "40000000-0000-4000-8000-000000000004"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.True(t, r.URL.Path == "/api/v1/timeline/rebuilds" ||
			r.URL.Path == "/api/v1/timeline/rebuilds/"+requested)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(map[bool]int{true: http.StatusAccepted, false: http.StatusOK}[r.Method == http.MethodPost])
		_, _ = w.Write([]byte(`{"operation_id":"` + returned + `","state":"running",` +
			`"deriver_fingerprint":"` + strings.Repeat("a", 64) + `","target_epoch":2,` +
			`"scanned":0,"published":0,"failed":0,"unavailable":0,` +
			`"started_at":"2026-09-12T00:00:00Z","updated_at":"2026-09-12T00:00:00Z"}`))
	}))
	t.Cleanup(ts.Close)
	c := client.New(ts.URL, "")

	_, err := c.TimelineRebuild(t.Context(), requested)
	require.ErrorContains(t, err, "operation ID")
	_, err = c.TimelineRebuildStatus(t.Context(), requested)
	require.ErrorContains(t, err, "operation ID")
}
