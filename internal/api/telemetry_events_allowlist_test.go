package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTelemetryEventBrowserAllowlist(t *testing.T) {
	t.Parallel()
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodPost, telemetryEventsPath, nil),
		httptest.NewRequest(http.MethodDelete, webSessionPath, nil),
		httptest.NewRequest(http.MethodDelete, webSessionPath+"?duration_bucket=1_to_5m", nil),
	} {
		require.True(t, webSessionRequestAllowed(req), "%s %s", req.Method, req.URL)
	}
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodDelete, webSessionPath+"?extra=1", nil),
		httptest.NewRequest(http.MethodDelete, webSessionPath+"?duration_bucket=1_to_5m&extra=1", nil),
		httptest.NewRequest(http.MethodDelete, webSessionPath+"?duration_bucket=1_to_5m&duration_bucket=over_30m", nil),
		httptest.NewRequest(http.MethodDelete, webSessionPath+"?duration_bucket=1_to_5m&x=%ZZ", nil),
		httptest.NewRequest(http.MethodGet, telemetryEventsPath, nil),
		httptest.NewRequest(http.MethodPost, telemetryEventsPath+"?x=1", nil),
		httptest.NewRequest(http.MethodPost, telemetryEventsPath+"/extra", nil),
		httptest.NewRequest(http.MethodPost, "/api/daemon/telemetry", nil),
	} {
		require.False(t, webSessionRequestAllowed(req), "%s %s", req.Method, req.URL)
	}
}
