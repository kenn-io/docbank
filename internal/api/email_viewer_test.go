package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEmailViewerBrowserReadsRejectBroaderAuthority(t *testing.T) {
	base := "/api/v1/versions/11111111-1111-4111-8111-111111111111/email"
	generation := base + "/generations/" + strings.Repeat("a", 64)
	for _, path := range []string{base, generation, generation + "/parts/1.2/body_utf8", generation + "/parts/1/raw_headers", generation + "/parts/1.3/decoded_payload"} {
		assert.True(t, webSessionRequestAllowed(httptest.NewRequest(http.MethodGet, path, nil)), path)
		assert.False(t, webSessionRequestAllowed(httptest.NewRequest(http.MethodPost, path, nil)), path)
		assert.False(t, webSessionRequestAllowed(httptest.NewRequest(http.MethodGet, path+"?unexpected=1", nil)), path)
	}
	for _, path := range []string{base + "/other", base + "/generations/no", generation + "/parts/0/body_utf8", generation + "/parts/1/other", generation + "/parts/1/body_utf8/extra"} {
		assert.False(t, webSessionRequestAllowed(httptest.NewRequest(http.MethodGet, path, nil)), path)
	}
}
