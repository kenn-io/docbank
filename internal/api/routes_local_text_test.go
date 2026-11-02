package api_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/plaintext"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/processing"
)

func TestLocalTextSetupRouteReviewsConsentAndImportRace(t *testing.T) {
	t.Parallel()
	var saves, starts atomic.Int64
	var startupFailure, saveFailure atomic.Bool
	startupFailure.Store(true)
	ts, catalog := newTestServer(t, func(deps *api.Deps) {
		renderer, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
		require.NoError(t, err)
		configure := configureProcessingTestServiceWithProviders(t, renderer, newProcessingTestEmbeddingProvider(t), true, document.EmbeddingOptional, "local-text")
		configure(deps)
		setup := &processing.LocalTextSetup{Service: deps.Processing,
			Startup: func(context.Context, bool, string) error {
				starts.Add(1)
				if startupFailure.Load() {
					return errors.New("synthetic startup failure")
				}
				return nil
			},
			Save: func(name, mode string) error {
				assert.Equal(t, "local-text", name)
				assert.Equal(t, "local", mode)
				if saveFailure.Load() {
					return errors.New("synthetic selection path failure")
				}
				saves.Add(1)
				return nil
			},
		}
		setup.Candidate = func(mode string) (*processing.Service, error) {
			assert.Equal(t, "local", mode)
			candidate := *deps
			configure(&candidate)
			return candidate.Processing, nil
		}
		deps.LocalText = setup
	})
	createFileWithContent(t, ts, catalog, "/first.txt", "first synthetic passage\n")
	response, body := do(t, ts, http.MethodPost, "/api/v1/processing/local/plans", nil, api.LocalTextPlanRequest{Server: "local"})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var plan api.LocalTextPlan
	require.NoError(t, json.Unmarshal([]byte(body), &plan))
	require.Equal(t, "this vault and new imports", plan.Scope)
	require.Equal(t, "builds the separate local-text index; embeds inputs it is missing", plan.VectorEffect)
	require.Len(t, plan.Flow, 3)
	require.Equal(t, "query_embedding", plan.Flow[2].Capability)
	require.Zero(t, starts.Load())
	require.Zero(t, saves.Load())
	request := api.LocalTextApplyRequest{Server: plan.Server, PlanFingerprint: plan.Fingerprint}
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/local/setup", nil, request)
	require.Equal(t, http.StatusPreconditionRequired, response.StatusCode, body)
	require.Zero(t, starts.Load())
	request.Consent = true
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/local/setup", nil, request)
	require.Equal(t, http.StatusInternalServerError, response.StatusCode, body)
	require.Zero(t, saves.Load())
	startupFailure.Store(false)
	createFileWithContent(t, ts, catalog, "/second.txt", "second synthetic passage\n")
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/local/setup", nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Equal(t, int64(1), saves.Load(), "an imported document doesn't invalidate reviewed ongoing scope")
	saveFailure.Store(true)
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/local/setup", nil, request)
	require.Equal(t, http.StatusInternalServerError, response.StatusCode, body)
	require.Contains(t, body, "processing consent was saved: document processing failed")
	require.NotContains(t, body, "synthetic selection path failure")
	request.PlanFingerprint = testHash("stale")
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/local/setup", nil, request)
	require.Equal(t, http.StatusConflict, response.StatusCode, body)
	require.Equal(t, int64(1), saves.Load())
}
