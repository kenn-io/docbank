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
		{http.MethodGet, "/api/v1/productions/recipes"},
		{http.MethodPost, "/api/v1/productions/sets"},
		{http.MethodGet, "/api/v1/productions/sets?limit=1"},
		{http.MethodGet, base},
		{http.MethodGet, base + "/revisions/1"},
		{http.MethodGet, base + "/revisions/1/members?limit=1"},
		{http.MethodGet, base + "/revisions/1/decisions?limit=1"},
		{http.MethodGet, base + "/revisions/1/maps/88888888-8888-4888-8888-888888888887?limit=17"},
		{http.MethodGet, base + "/jobs/88888888-8888-4888-8888-888888888887"},
		{http.MethodPut, base + "/revisions/1/instructions"},
		{http.MethodPost, base + "/revisions/1/changes"},
		{http.MethodPost, base + "/revisions/1/seal"},
		{http.MethodPost, base + "/revisions/1/fork"},
		{http.MethodPost, base + "/revisions/1/members/88888888-8888-4888-8888-888888888887/review"},
	} {
		require.True(t, webSessionRequestAllowed(httptest.NewRequest(route.method, route.path, nil)), route.path)
	}
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/productions/recipes?unexpected=1"},
		{http.MethodPost, "/api/v1/productions/sets?unexpected=1"},
		{http.MethodGet, "/api/v1/productions/sets?limit=201"},
		{http.MethodGet, "/api/v1/productions/sets?unexpected=1"},
		{http.MethodPost, base},
		{http.MethodGet, base + "/revisions/0"},
		{http.MethodGet, base + "/revisions/1/jobs"},
		{http.MethodGet, base + "/jobs/bad"},
		{http.MethodGet, base + "/jobs/88888888-8888-4888-8888-888888888887?unexpected=1"},
		{http.MethodGet, base + "/revisions/1/members?limit=201"},
		{http.MethodGet, base + "/revisions/1/members?unexpected=1"},
		{http.MethodGet, base + "/revisions/1/maps/88888888-8888-4888-8888-888888888887?limit=65537"},
		{http.MethodGet, base + "/revisions/1/maps/88888888-8888-4888-8888-888888888887?unexpected=1"},
		{http.MethodPut, base + "/revisions/1/instructions?unexpected=1"},
		{http.MethodPost, base + "/revisions/1/changes/other"},
		{http.MethodPost, base + "/revisions/1/seal?unexpected=1"},
		{http.MethodPost, base + "/revisions/1/fork?unexpected=1"},
		{http.MethodGet, base + "/revisions/1/fork"},
		{http.MethodPost, base + "/revisions/1/members/bad/review"},
	} {
		require.False(t, webSessionRequestAllowed(httptest.NewRequest(route.method, route.path, nil)), route.path)
	}
}
