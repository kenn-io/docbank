package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProductionSetBrowserAllowlist(t *testing.T) {
	const setID = "88888888-8888-4888-8888-888888888888"
	base := "/api/v1/productions/sets/" + setID
	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/productions/sets"},
		{http.MethodGet, base},
		{http.MethodGet, base + "/revisions/1"},
		{http.MethodGet, base + "/revisions/1/members?limit=1"},
		{http.MethodGet, base + "/revisions/1/decisions?limit=1"},
	} {
		require.True(t, webSessionRequestAllowed(httptest.NewRequest(route.method, route.path, nil)), route.path)
	}
	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/productions/sets?unexpected=1"},
		{http.MethodPost, base},
		{http.MethodGet, base + "/revisions/0"},
		{http.MethodGet, base + "/revisions/1/jobs"},
		{http.MethodGet, base + "/revisions/1/members?limit=201"},
		{http.MethodGet, base + "/revisions/1/members?unexpected=1"},
	} {
		require.False(t, webSessionRequestAllowed(httptest.NewRequest(route.method, route.path, nil)), route.path)
	}
}
