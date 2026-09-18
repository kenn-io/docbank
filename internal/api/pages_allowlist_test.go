package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPageBrowserAllowlist(t *testing.T) {
	job := "/api/v1/pages/jobs/00000000-0000-4000-8000-000000000001"
	for _, path := range []string{"/api/v1/pages/inventory", "/api/v1/pages/jobs", job, job + "/cancel"} {
		require.True(t, webSessionRequestAllowed(httptest.NewRequest(http.MethodPost, path, nil)))
		require.False(t, webSessionRequestAllowed(httptest.NewRequest(http.MethodGet, path, nil)))
		require.False(t, webSessionRequestAllowed(httptest.NewRequest(http.MethodPost, path+"?extra=1", nil)))
	}
	image := "/api/v1/pages/image?node_id=1&revision=1&version_id=v&source_sha256=s&source_size=1&page=1&recipe_sha256=r&frame_sha256=f&image_sha256=i"
	require.True(t, webSessionRequestAllowed(httptest.NewRequest(http.MethodGet, image, nil)))
	for _, path := range []string{image + "&page=2", image + "&extra=1", "/api/v1/pages/image", job + "/other"} {
		require.False(t, webSessionRequestAllowed(httptest.NewRequest(http.MethodGet, path, nil)))
	}
	require.False(t, webSessionRequestAllowed(httptest.NewRequest(http.MethodPost, image, nil)))
}
