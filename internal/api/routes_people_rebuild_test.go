package api_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestPeopleRebuildRoutesReplayAndCoverage(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	const operationID = "10000000-0000-4000-8000-000000000011"
	body := map[string]any{"operation_id": operationID}

	response, raw := do(t, ts, http.MethodPost, "/api/v1/people/rebuilds", nil, body)
	require.Equal(t, http.StatusAccepted, response.StatusCode, raw)
	var first api.PeopleBuild
	require.NoError(t, json.Unmarshal([]byte(raw), &first))
	require.Equal(t, operationID, first.OperationID)
	require.Equal(t, "running", first.State)

	response, raw = do(t, ts, http.MethodPost, "/api/v1/people/rebuilds", nil, body)
	require.Equal(t, http.StatusAccepted, response.StatusCode, raw)
	var replay api.PeopleBuild
	require.NoError(t, json.Unmarshal([]byte(raw), &replay))
	require.Equal(t, first, replay)

	response, raw = get(t, ts, "/api/v1/people/rebuilds/"+operationID, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, raw)
	var status api.PeopleBuild
	require.NoError(t, json.Unmarshal([]byte(raw), &status))
	require.Equal(t, replay, status)

	response, raw = get(t, ts, "/api/v1/people/coverage", nil)
	require.Equal(t, http.StatusOK, response.StatusCode, raw)
	var coverage api.PeopleCoverage
	require.NoError(t, json.Unmarshal([]byte(raw), &coverage))
	require.Equal(t, int64(0), coverage.Published+coverage.Pending+coverage.Failed+coverage.Unavailable)
}
