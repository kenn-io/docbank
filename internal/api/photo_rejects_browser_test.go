package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPhotoRejectsBrowserAllowlist(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/api/v1/photos/rejects/preflight", "/api/v1/photos/rejects/trash"} {
		assert.True(t, webSessionRequestAllowed(httptest.NewRequest(http.MethodPost, path, nil)))
		assert.False(t, webSessionRequestAllowed(httptest.NewRequest(http.MethodGet, path, nil)))
		assert.False(t, webSessionRequestAllowed(httptest.NewRequest(http.MethodPost, path+"?extra=1", nil)))
	}
}
