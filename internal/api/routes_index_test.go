package api_test

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/processing"
)

func TestIndexRoutesExposeTruthfulDisabledProjectionAndUnavailableCoordinator(t *testing.T) {
	t.Run("coordinator unavailable", func(t *testing.T) {
		ts, _ := newTestServer(t, nil)
		response, body := get(t, ts, "/api/v1/index/status", nil)
		assert.Equal(t, http.StatusServiceUnavailable, response.StatusCode, body)
		assert.Contains(t, body, `"code":"index_unavailable"`)
	})

	t.Run("disabled projection", func(t *testing.T) {
		backend, err := processing.NewDisabledIndexBackend(processing.IndexTarget{Kind: processing.IndexMap})
		require.NoError(t, err)
		coordinator, err := processing.NewIndexCoordinator(processing.IndexCoordinatorConfig{
			Backends: []processing.IndexProjectionBackend{backend},
		})
		require.NoError(t, err)
		ts, _ := newTestServer(t, func(deps *api.Deps) { deps.Indexes = coordinator })

		response, body := get(t, ts, "/api/v1/index/status", nil)
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		assert.Contains(t, body, `"kind":"map"`)
		assert.Contains(t, body, `"state":"disabled"`)

		response, body = do(t, ts, http.MethodPost, "/api/v1/index/repair-plans", nil,
			map[string]any{"targets": []map[string]any{{"kind": "map"}}})
		assert.Equal(t, http.StatusConflict, response.StatusCode, body)
		assert.Contains(t, body, `"code":"index_disabled"`)
	})
}

func TestIndexRoutesRepairEmptyLexicalProjectionEndToEnd(t *testing.T) {
	ts, _ := newTestServer(t, func(deps *api.Deps) {
		backend, err := processing.NewLexicalIndexBackend(processing.LexicalIndexBackendConfig{
			Store:             deps.Store,
			Mutate:            func(_ context.Context, fn func() error) error { return fn() },
			RollbackRetention: time.Hour,
		})
		require.NoError(t, err)
		deps.Indexes, err = processing.NewIndexCoordinator(processing.IndexCoordinatorConfig{
			Backends: []processing.IndexProjectionBackend{backend},
		})
		require.NoError(t, err)
	})

	response, body := do(t, ts, http.MethodPost, "/api/v1/index/repair-plans", nil,
		map[string]any{"targets": []map[string]any{{"kind": "lexical"}}})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var plan processing.IndexRepairPlan
	require.NoError(t, json.Unmarshal([]byte(body), &plan))
	require.Len(t, plan.Projections, 1)

	response, body = do(t, ts, http.MethodPost, "/api/v1/index/repairs", nil,
		map[string]any{
			"targets":             []map[string]any{{"kind": "lexical"}},
			"plan_fingerprint":    plan.Fingerprint,
			"allow_provider_work": false,
		})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var repaired processing.IndexRepairReport
	require.NoError(t, json.Unmarshal([]byte(body), &repaired))
	require.Len(t, repaired.Projections, 1)
	require.Len(t, repaired.Projections[0].Generation, 64)

	response, body = get(t, ts, "/api/v1/index/status?require_fresh=true&wait_ms=100", nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var status processing.IndexStatusReport
	require.NoError(t, json.Unmarshal([]byte(body), &status))
	require.True(t, status.Fresh)
	require.Equal(t, processing.IndexStateQueryable, status.Projections[0].State)
}
