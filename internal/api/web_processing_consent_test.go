package api_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestWebSessionCanRevokeAndRenewProcessingConsent(t *testing.T) {
	ts, catalog := newTestServer(t, configureProcessingTestService(t))
	node := createFileWithContent(t, ts, catalog, "/browser-consent.txt", "synthetic browser consent\n")
	response, body := do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
	var session struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &session))
	webHeaders := map[string]string{"X-Api-Key": "", api.WebSessionHeader: session.Token}
	selector := api.ProcessingSelector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: "private"}
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/plans", webHeaders,
		api.ProcessingPlanRequest{Selector: selector})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var plan api.ProcessingPlan
	require.NoError(t, json.Unmarshal([]byte(body), &plan))
	require.Equal(t, "required", plan.ConsentState)

	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/jobs", webHeaders,
		api.StartProcessingRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	processingJobFromStream(t, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/plans", webHeaders,
		api.ProcessingPlanRequest{Selector: selector})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &plan))
	require.Equal(t, "active", plan.ConsentState)

	for _, path := range []string{
		"/api/v1/processing/consent/grants", "/api/v1/processing/consent/revocations",
	} {
		for _, request := range []struct{ method, path string }{
			{http.MethodGet, path},
			{http.MethodPost, path + "?profile=private"},
			{http.MethodPost, path + "/extra"},
		} {
			response, body = do(t, ts, request.method, request.path, webHeaders,
				api.ProcessingConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
			require.Equal(t, http.StatusForbidden, response.StatusCode, request.path+": "+body)
		}
	}

	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/consent/revocations", webHeaders, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)

	second := createFileWithContent(t, ts, catalog, "/browser-revoked.txt", "synthetic revoked consent\n")
	selector = api.ProcessingSelector{NodeID: second.ID, ContentVersionID: second.CurrentVersionID, Profile: "private"}
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/plans", webHeaders,
		api.ProcessingPlanRequest{Selector: selector})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &plan))
	require.Equal(t, "revoked", plan.ConsentState)
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/jobs", webHeaders,
		api.StartProcessingRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
	require.Equal(t, http.StatusPreconditionFailed, response.StatusCode, body)
	require.Contains(t, body, `"code":"processing_consent_revoked"`)

	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/consent/grants", webHeaders,
		api.ProcessingConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/jobs", webHeaders,
		api.StartProcessingRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	job := processingJobFromStream(t, body)
	response, body = get(t, ts, "/api/v1/processing/jobs/"+job.ID, webHeaders)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Contains(t, body, `"state":"completed"`)
}
