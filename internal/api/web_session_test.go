package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.kenn.io/docbank/internal/config"

	"github.com/stretchr/testify/require"
)

func TestWebSessionDeniesPersonRoutes(t *testing.T) {
	t.Parallel()
	paths := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/v1/photos/versions/11111111-1111-4111-8111-111111111111/faces"},
		{http.MethodGet, "/api/v1/photos/versions/11111111-1111-4111-8111-111111111111/faces"},
		{http.MethodPost, "/api/v1/people"},
		{http.MethodGet, "/api/v1/people/by-id/11111111-1111-4111-8111-111111111111"},
		{http.MethodPatch, "/api/v1/people/by-id/11111111-1111-4111-8111-111111111111"},
		{http.MethodPost, "/api/v1/people/by-id/11111111-1111-4111-8111-111111111111/retire"},
		{http.MethodPost, "/api/v1/people/by-id/11111111-1111-4111-8111-111111111111/merge"},
		{http.MethodPost, "/api/v1/people/by-id/11111111-1111-4111-8111-111111111111/split"},
	}
	for _, path := range paths {
		require.False(t, webSessionRequestAllowed(httptest.NewRequest(path.method, path.path, nil)), path.method+" "+path.path)
	}
}

func TestWebSessionAllowsPhotoAlbumAndAuthoredRoutes(t *testing.T) {
	t.Parallel()
	album := "/api/v1/photos/albums/11111111-1111-4111-8111-111111111111"
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/photos/albums"},
		{http.MethodPost, "/api/v1/photos/albums"},
		{http.MethodPut, album},
		{http.MethodDelete, album},
		{http.MethodPut, album + "/cover"},
		{http.MethodPost, album + "/duplicate"},
		{http.MethodPost, album + "/members/add"},
		{http.MethodPost, album + "/members/remove"},
		{http.MethodGet, "/api/v1/photos/assets/11111111-1111-4111-8111-111111111111/suggestions"},
		{http.MethodPost, "/api/v1/photos/suggestions/11111111-1111-4111-8111-111111111111/response"},
		{http.MethodPost, "/api/v1/photos/authored/prepare"},
		{http.MethodPost, "/api/v1/photos/authored/edit"},
		{http.MethodPost, "/api/v1/photos/authored/40000000-0000-4000-8000-000000000001/undo"},
	} {
		require.True(t, webSessionRequestAllowed(httptest.NewRequest(route.method, route.path, nil)), route.method+" "+route.path)
		require.False(t, webSessionRequestAllowed(httptest.NewRequest(route.method, route.path+"?extra=1", nil)))
		if strings.Contains(route.path, "suggestions") {
			for _, path := range []string{route.path + "/extra", strings.Replace(route.path, "11111111-1111-4111-8111-111111111111", "invalid", 1)} {
				require.False(t, webSessionRequestAllowed(httptest.NewRequest(route.method, path, nil)), path)
			}
		}
		if route.method == http.MethodPost && route.path != "/api/v1/photos/albums" {
			require.False(t, webSessionRequestAllowed(httptest.NewRequest(http.MethodGet, route.path, nil)))
		}
	}
	for _, path := range []string{album + "/extra", album + "/members/add/extra", "/api/v1/photos/authored/photos/undo"} {
		require.False(t, webSessionRequestAllowed(httptest.NewRequest(http.MethodPost, path, nil)), path)
	}
	for _, path := range []string{"/api/v1/photos/albums/invalid", "/api/v1/photos/albums/11111111-1111-4111-8111-11111111111A", "/api/v1/photos/albums/"} {
		require.False(t, webSessionRequestAllowed(httptest.NewRequest(http.MethodPut, path, nil)), path)
	}
	require.False(t, webSessionRequestAllowed(httptest.NewRequest(http.MethodGet, album, nil)))
	require.False(t, webSessionRequestAllowed(httptest.NewRequest(http.MethodPatch, album, nil)))
	require.False(t, webSessionRequestAllowed(httptest.NewRequest(http.MethodPost, "/api/v1/photos/assets", nil)))
}

func TestPhotoAlbumPages(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	registerWeb(mux, true, "http://localhost", "", config.MapTilesConfig{}, nil)
	for _, tc := range []struct {
		path    string
		allowed bool
	}{
		{"/photos/albums", true},
		{"/photos/albums/11111111-1111-4111-8111-111111111111", true},
		{"/photos/albums/invalid", false},
	} {
		require.Equal(t, tc.allowed, authExempt(tc.path), tc.path)
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tc.path, nil))
		status := http.StatusNotFound
		if tc.allowed {
			status = http.StatusOK
			require.Contains(t, response.Header().Get("Content-Type"), "text/html")
		}
		require.Equal(t, status, response.Code, tc.path)
	}
}
