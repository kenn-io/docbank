package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTermReportBrowserAllowlistRequiresExactRoutes(t *testing.T) {
	id := strings.Repeat("a", 48)
	for _, item := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/term-reports"},
		{http.MethodGet, "/api/v1/term-reports/" + id},
		{http.MethodPost, "/api/v1/term-reports/" + id + "/dates"},
		{http.MethodPost, "/api/v1/term-reports/" + id + "/revisions"},
		{http.MethodPost, "/api/v1/term-reports/" + id + "/download"},
	} {
		require.True(t, webSessionRequestAllowed(httptest.NewRequest(item.method, item.path, nil)), item.path)
		require.False(t, webSessionRequestAllowed(httptest.NewRequest(item.method, item.path+"?extra=1", nil)), item.path)
	}
	require.True(t, webSessionRequestAllowed(httptest.NewRequest(http.MethodGet, "/api/v1/term-reports", nil)))
	require.True(t, webSessionRequestAllowed(httptest.NewRequest(http.MethodGet, "/api/v1/term-reports?offset=20&limit=20", nil)))
	require.False(t, webSessionRequestAllowed(httptest.NewRequest(http.MethodGet, "/api/v1/term-reports?other=1", nil)))
	for _, item := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/term-reports/" + id + "/csv"},
		{http.MethodGet, "/api/v1/term-reports/" + id + "/bundle"},
		{http.MethodPost, "/api/v1/term-reports/" + strings.Repeat("z", 48) + "/dates"},
		{http.MethodPost, "/api/v1/term-reports/" + id + "/dates/extra"},
	} {
		require.False(t, webSessionRequestAllowed(httptest.NewRequest(item.method, item.path, nil)), item.path)
	}
}
