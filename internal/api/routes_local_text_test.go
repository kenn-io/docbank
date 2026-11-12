package api_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
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
	var startupFailure, saveFailure atomic.Bool
	startupFailure.Store(true)
	ts, _ := newTestServer(t, func(deps *api.Deps) {
		renderer, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
		require.NoError(t, err)
		configure := configureProcessingTestServiceWithProviders(t, renderer, newProcessingTestEmbeddingProvider(t), true, document.EmbeddingOptional, "local-text")
		configure(deps)
		setup := &processing.LocalTextSetup{Service: deps.Processing,
			Startup: func(context.Context, bool, string) error {
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
			return api.LocalTextStatus{DownloadBytes: 295000000}, nil
		}
	})
	response, body := do(t, ts, http.MethodPost, "/api/v1/processing/local/plans", nil, api.LocalTextPlanRequest{Server: "local"})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var plan api.LocalTextPlan
	require.NoError(t, json.Unmarshal([]byte(body), &plan))
	require.Equal(t, "this vault and new imports", plan.Scope)
	require.NotEmpty(t, plan.VectorEffect)
	require.Len(t, plan.Flow, 3)
	require.Equal(t, "query_embedding", plan.Flow[2].Capability)
	request := api.LocalTextApplyRequest{Server: plan.Server, PlanFingerprint: plan.Fingerprint}
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/local/setup", nil, request)
	require.Equal(t, http.StatusPreconditionRequired, response.StatusCode, body)
	request.Consent = true
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/local/setup", nil, request)
	require.Equal(t, http.StatusInternalServerError, response.StatusCode, body)
	startupFailure.Store(false)
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/local/setup", nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = do(t, ts, http.MethodGet, "/api/v1/processing/local/status", nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var status api.LocalTextStatus
	require.NoError(t, json.Unmarshal([]byte(body), &status))
	require.Equal(t, int64(295000000), status.DownloadBytes)
	saveFailure.Store(true)
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/local/setup", nil, request)
	require.Equal(t, http.StatusInternalServerError, response.StatusCode, body)
	require.Contains(t, body, "processing consent was saved: document processing failed")
	require.NotContains(t, body, "synthetic selection path failure")
	request.PlanFingerprint = testHash("stale")
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/local/setup", nil, request)
	require.Equal(t, http.StatusConflict, response.StatusCode, body)
}

func TestRestartDaemonBrowserSession(t *testing.T) {
	t.Parallel()
	var calls []bool
	var failure bool
	ts, _ := newTestServer(t, func(deps *api.Deps) {
		deps.LocalTextStatus = func(context.Context) (api.LocalTextStatus, error) { return api.LocalTextStatus{}, nil }
		deps.RestartDaemon = func(_ context.Context, openTab bool) error {
			calls = append(calls, openTab)
			if failure {
				return errors.New("spawn failed")
			}
			return nil
		}
	})
	token := issueWebSession(t, ts)
	headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: token, "Origin": strings.TrimSuffix(testWebURL, "/")}
	response, body := do(t, ts, http.MethodGet, "/api/v1/processing/local/status", headers, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Contains(t, body, `"restart_available":true`)
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/local/restart", headers, nil)
	require.Equal(t, http.StatusAccepted, response.StatusCode, body)
	require.Empty(t, body)
	require.Equal(t, []bool{true}, calls)
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/local/restart", nil, nil)
	require.Equal(t, http.StatusForbidden, response.StatusCode, body)
	failure = true
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/local/restart", headers, nil)
	require.Equal(t, http.StatusInternalServerError, response.StatusCode, body)
}

func TestRestartDaemonUnavailable(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, func(deps *api.Deps) {
		deps.LocalTextStatus = func(context.Context) (api.LocalTextStatus, error) { return api.LocalTextStatus{}, nil }
	})
	headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts), "Origin": strings.TrimSuffix(testWebURL, "/")}
	response, body := do(t, ts, http.MethodGet, "/api/v1/processing/local/status", headers, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Contains(t, body, `"restart_available":false`)
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/local/restart", headers, nil)
	require.Equal(t, http.StatusConflict, response.StatusCode, body)
}

func TestRestartDaemonPublicOrigin(t *testing.T) {
	t.Parallel()
	var called bool
	_, catalog := newTestServer(t, func(deps *api.Deps) {
		deps.WebLoginEnabled = true
		deps.Cfg.Web.PublicOrigin = signInOrigin
		deps.RestartDaemon = func(_ context.Context, openTab bool) error { called = true; assert.False(t, openTab); return nil }
	})
	status, cookie := loginBrowser(t, catalog.Server)
	response := signInCall(t, catalog.Server, http.MethodPost, "/api/v1/processing/local/restart", "", status.Session.Token, cookie)
	require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
	require.Empty(t, response.Body.String())
	require.True(t, called)
}

func TestRestartDaemonPerRunKeyLogin(t *testing.T) {
	t.Parallel()
	var opened bool
	_, catalog := newTestServer(t, func(deps *api.Deps) {
		deps.WebLoginEnabled = true
		deps.RestartDaemon = func(_ context.Context, openTab bool) error {
			opened = openTab
			return nil
		}
	})
	origin := strings.TrimSuffix(testWebURL, "/")
	call := func(method, path, body, token string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), method, origin+path, strings.NewReader(body))
		req.Header.Set("Origin", origin)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(api.WebSessionHeader, token)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		out := httptest.NewRecorder()
		catalog.Server.Handler().ServeHTTP(out, req)
		return out
	}
	login := call(http.MethodPost, "/api/daemon/web-auth/login", `{"api_key":"`+testAPIKey+`"}`, "", nil)
	require.Equal(t, http.StatusOK, login.Code, login.Body.String())
	var status signInStatus
	require.NoError(t, json.Unmarshal(login.Body.Bytes(), &status))
	require.NotNil(t, status.Session)
	cookies := login.Result().Cookies()
	require.Len(t, cookies, 1)
	response := call(http.MethodPost, "/api/v1/processing/local/restart", "", status.Session.Token, cookies[0])
	require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
	require.True(t, opened)
}
