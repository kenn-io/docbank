package api_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestProductionGateSelectionUsesAuthenticatedRevisionRoute(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	response, body := do(t, ts, http.MethodPost, "/api/v1/productions/sets", nil,
		map[string]any{"operation_id": "7a000000-0000-4000-8000-000000000001",
			"name": "Synthetic gate selection", "instructions": "Review synthetic members."})
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
	var created api.ProductionSetCreated
	require.NoError(t, json.Unmarshal([]byte(body), &created))
	path := "/api/v1/productions/sets/" + created.Set.ID + "/revisions/1/gate-authority"
	response, body = do(t, ts, http.MethodPut, path, map[string]string{"X-Api-Key": ""},
		api.ProductionGateSelectionRequest{})
	require.Equal(t, http.StatusUnauthorized, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPut, path, nil, api.ProductionGateSelectionRequest{})
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
}
