package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWebSessionDeniesPersonRoutes(t *testing.T) {
	t.Parallel()
	paths := []struct {
		method string
		path   string
	}{
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

func TestWebSessionDeniesPhotoAlbumRoutes(t *testing.T) {
	t.Parallel()
	album := "/api/v1/photos/albums/11111111-1111-4111-8111-111111111111"
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/photos/albums"},
		{http.MethodPost, "/api/v1/photos/albums"},
		{http.MethodGet, album},
		{http.MethodPut, album},
		{http.MethodDelete, album},
		{http.MethodPut, album + "/cover"},
		{http.MethodPost, album + "/duplicate"},
		{http.MethodPost, album + "/members/add"},
		{http.MethodPost, album + "/members/remove"},
	} {
		require.False(t, webSessionRequestAllowed(httptest.NewRequest(route.method, route.path, nil)), route.method+" "+route.path)
	}
}
