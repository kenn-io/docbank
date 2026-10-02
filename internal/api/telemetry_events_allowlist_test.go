package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTelemetryEventBrowserAllowlist(t *testing.T) {
	t.Parallel()
	require.True(t, webSessionRequestAllowed(httptest.NewRequest(http.MethodPost, telemetryEventsPath, nil)))
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, telemetryEventsPath, nil),
		httptest.NewRequest(http.MethodPost, telemetryEventsPath+"?x=1", nil),
		httptest.NewRequest(http.MethodPost, telemetryEventsPath+"/extra", nil),
		httptest.NewRequest(http.MethodPost, "/api/daemon/telemetry", nil),
	} {
		require.False(t, webSessionRequestAllowed(req), "%s %s", req.Method, req.URL)
	}
}
