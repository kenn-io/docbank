package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBatesBrowserRuleAllowsOnlyReviewedWorkflow(t *testing.T) {
	for _, request := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/bates/namespaces?limit=250"},
		{http.MethodPost, "/api/v1/bates/namespaces"},
		{http.MethodPost, "/api/v1/bates/preview"},
		{http.MethodPost, "/api/v1/bates/allocations"},
		{http.MethodGet, "/api/v1/bates/allocations/11111111-1111-4111-8111-111111111111"},
		{http.MethodGet, "/api/v1/bates/exports?limit=50"},
		{http.MethodPost, "/api/v1/bates/exports"},
		{http.MethodGet, "/api/v1/bates/exports/11111111-1111-4111-8111-111111111111"},
		{http.MethodPost, "/api/v1/bates/exports/11111111-1111-4111-8111-111111111111/download"},
	} {
		require.True(t, webSessionRequestAllowed(httptest.NewRequest(request.method, request.path, nil)), request.method+" "+request.path)
	}
	for _, request := range []struct{ method, path string }{
		{http.MethodDelete, "/api/v1/bates/allocations/11111111-1111-4111-8111-111111111111"},
		{http.MethodGet, "/api/v1/bates/exports/11111111-1111-4111-8111-111111111111/content"},
		{http.MethodPost, "/api/v1/bates/preview?unsafe=1"},
		{http.MethodPost, "/api/v1/bates/exports/11111111-1111-4111-8111-111111111111"},
		{http.MethodGet, "/api/v1/bates/exports/a/b/c"},
	} {
		require.False(t, webSessionRequestAllowed(httptest.NewRequest(request.method, request.path, nil)), request.method+" "+request.path)
	}
}
