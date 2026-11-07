package api_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/plaintext"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/processing"
)

func TestLocalTextSetupRouteReviewsConsentAndMapsErrors(t *testing.T) {
	t.Parallel()
	var startupFailure, saveFailure, saved atomic.Bool
	var startupCalls atomic.Int64
	startupFailure.Store(true)
	ts, _ := newTestServer(t, func(deps *api.Deps) {
		renderer, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
		require.NoError(t, err)
		configure := configureProcessingTestServiceWithProviders(t, renderer, newProcessingTestEmbeddingProvider(t), true, document.EmbeddingOptional, "local-text")
		configure(deps)
		setup := &processing.LocalTextSetup{Service: deps.Processing,
			Startup: func(context.Context, bool, string) error {
				startupCalls.Add(1)
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
				saved.Store(true)
				return nil
			},
		}
		setup.Candidate = func(mode, _ string) (*processing.Service, error) {
			assert.Equal(t, "local", mode)
			candidate := *deps
			configure(&candidate)
			return candidate.Processing, nil
		}
		deps.LocalText = setup
		deps.LocalTextStatus = func(context.Context) (api.LocalTextStatus, error) {
			return api.LocalTextStatus{RestartRequired: saved.Load()}, nil
		}
	})
	headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts)}
	response, body := do(t, ts, http.MethodPost, "/api/v1/processing/local/plans", headers, api.LocalTextPlanRequest{Server: "local"})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var plan api.LocalTextPlan
	require.NoError(t, json.Unmarshal([]byte(body), &plan))
	require.Equal(t, "this vault and new imports", plan.Scope)
	require.NotEmpty(t, plan.VectorEffect)
	require.Len(t, plan.Flow, 3)
	require.Equal(t, "query_embedding", plan.Flow[2].Capability)
	request := api.LocalTextApplyRequest{Server: plan.Server, PlanFingerprint: plan.Fingerprint}
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/local/setup", headers, request)
	require.Equal(t, http.StatusPreconditionRequired, response.StatusCode, body)
	require.Zero(t, startupCalls.Load())
	request.Consent = true
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/local/setup", headers, request)
	require.Equal(t, http.StatusInternalServerError, response.StatusCode, body)
	startupFailure.Store(false)
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/local/setup", headers, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = do(t, ts, http.MethodGet, "/api/v1/processing/local/status", headers, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var status api.LocalTextStatus
	require.NoError(t, json.Unmarshal([]byte(body), &status))
	require.True(t, status.RestartRequired)
	saveFailure.Store(true)
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/local/setup", headers, request)
	require.Equal(t, http.StatusInternalServerError, response.StatusCode, body)
	require.Contains(t, body, "processing consent was saved: document processing failed")
	require.NotContains(t, body, "synthetic selection path failure")
	beforeStale := startupCalls.Load()
	request.PlanFingerprint = testHash("stale")
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/local/setup", headers, request)
	require.Equal(t, http.StatusConflict, response.StatusCode, body)
	require.Equal(t, beforeStale, startupCalls.Load())
}

func TestLocalTextStatusDuringSetup(t *testing.T) {
	t.Parallel()
	started, release := make(chan struct{}), make(chan struct{})
	ts, _ := newTestServer(t, func(deps *api.Deps) {
		renderer, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
		require.NoError(t, err)
		configure := configureProcessingTestServiceWithProviders(t, renderer, newProcessingTestEmbeddingProvider(t), true, document.EmbeddingOptional, "local-text")
		configure(deps)
		deps.LocalText = &processing.LocalTextSetup{Service: deps.Processing,
			Startup: func(ctx context.Context, _ bool, _ string) error {
				close(started)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			},
			Save: func(string, string) error { return nil },
			Candidate: func(string, string) (*processing.Service, error) {
				candidate := *deps
				configure(&candidate)
				return candidate.Processing, nil
			},
		}
		deps.LocalTextStatus = func(context.Context) (api.LocalTextStatus, error) {
			return api.LocalTextStatus{DownloadBytes: 295000000}, nil
		}
	})
	headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts)}
	response, body := do(t, ts, http.MethodPost, "/api/v1/processing/local/plans", headers, api.LocalTextPlanRequest{Server: "auto"})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var plan api.LocalTextPlan
	require.NoError(t, json.Unmarshal([]byte(body), &plan))
	data, err := json.Marshal(api.LocalTextApplyRequest{Server: "auto", PlanFingerprint: plan.Fingerprint, Consent: true})
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/api/v1/processing/local/setup", strings.NewReader(string(data)))
	require.NoError(t, err)
	request.Header.Set(api.WebSessionHeader, headers[api.WebSessionHeader])
	request.Header.Set("Content-Type", "application/json")
	type setupResult struct {
		response *http.Response
		err      error
	}
	done := make(chan setupResult, 1)
	go func() {
		response, err := ts.Client().Do(request)
		if err == nil {
			err = response.Body.Close()
		}
		done <- setupResult{response, err}
	}()
	<-started
	response, body = do(t, ts, http.MethodGet, "/api/v1/processing/local/status", headers, nil)
	close(release)
	result := <-done
	require.NoError(t, result.err)
	require.Equal(t, http.StatusOK, result.response.StatusCode)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var status api.LocalTextStatus
	require.NoError(t, json.Unmarshal([]byte(body), &status))
	require.Equal(t, int64(295000000), status.DownloadBytes)
}
