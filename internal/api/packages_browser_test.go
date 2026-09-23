package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPackagesBrowserRuleRefusesChunksAndServerPaths(t *testing.T) {
	require.False(t, packagesBrowserRequestAllowed(httptest.NewRequest(http.MethodPut, "/api/v1/packages/containers/c1/chunks/0", nil)))
	require.True(t, packagesBrowserRequestAllowed(httptest.NewRequest(http.MethodPost, "/api/v1/packages/containers", nil)))
	require.True(t, packagesBrowserRequestAllowed(httptest.NewRequest(http.MethodPost, "/api/v1/packages/containers/c1/preflight", nil)))
	require.True(t, packagesBrowserRequestAllowed(httptest.NewRequest(http.MethodPost, "/api/v1/packages/containers/c1/seal", nil)))
	require.False(t, packagesBrowserRequestAllowed(httptest.NewRequest(http.MethodPost, "/api/v1/packages/imports?source_kind=root", nil)))
	require.True(t, packagesBrowserRequestAllowed(httptest.NewRequest(http.MethodPost, "/api/v1/packages/imports", nil)))
	require.True(t, packagesBrowserRequestAllowed(httptest.NewRequest(http.MethodGet, "/api/v1/packages?limit=50", nil)))
	require.True(t, packagesBrowserRequestAllowed(httptest.NewRequest(http.MethodGet, "/api/v1/packages/field-catalog", nil)))
	require.True(t, packagesBrowserRequestAllowed(httptest.NewRequest(http.MethodGet, "/api/v1/packages/label-candidates?label=EXT000001", nil)))
	require.True(t, packagesBrowserRequestAllowed(httptest.NewRequest(http.MethodGet, "/api/v1/packages/by-id/package-id", nil)))
	require.False(t, packagesBrowserRequestAllowed(httptest.NewRequest(http.MethodGet, "/api/v1/packages/by-id/package-id/members?limit=50", nil)))
	require.False(t, packagesBrowserRequestAllowed(httptest.NewRequest(http.MethodGet, "/api/v1/packages/by-id/package-id/timeline-inputs", nil)))
	require.False(t, packagesBrowserRequestAllowed(httptest.NewRequest(http.MethodPost, "/api/v1/packages/imports/op/resume", nil)))
	require.True(t, packagesBrowserRequestAllowed(httptest.NewRequest(http.MethodGet, "/api/v1/packages/by-id/package-id/records/row-id", nil)))
	require.False(t, packagesBrowserRequestAllowed(httptest.NewRequest(http.MethodPost, "/api/v1/packages/by-id/package-id", nil)))
	require.False(t, packagesBrowserRequestAllowed(httptest.NewRequest(http.MethodGet, "/api/v1/packages/a/b/c/d/e", nil)))
	require.False(t, webSessionRequestAllowed(httptest.NewRequest(http.MethodPost, "/api/v1/packages/preflights", nil)))
}

func TestPackageContainerByteOperationsAreNotCutOffByRequestTimeout(t *testing.T) {
	for _, path := range []string{
		"/api/v1/packages/containers/c1/chunks/0",
		"/api/v1/packages/containers/c1/seal",
		"/api/v1/packages/containers/c1/preflight",
	} {
		method := http.MethodPut
		if strings.HasSuffix(path, "/seal") || strings.HasSuffix(path, "/preflight") {
			method = http.MethodPost
		}
		require.True(t, timeoutExemptRequest(httptest.NewRequest(method, path, nil)), path)
	}
}
