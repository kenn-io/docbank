package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDocumentFactsBrowserAllowlist(t *testing.T) {
	t.Parallel()
	base := "/api/v1/versions/00000000-0000-4000-8000-000000000000/dates"
	for _, suffix := range []string{"", "?disclosure=safe", "?disclosure=full"} {
		if !webSessionRequestAllowed(httptest.NewRequestWithContext(t.Context(), http.MethodGet, base+suffix, nil)) {
			t.Errorf("denied %q", suffix)
		}
	}
	for _, suffix := range []string{"?disclosure=full&disclosure=safe", "?other=1", "?disclosure=bad"} {
		if webSessionRequestAllowed(httptest.NewRequestWithContext(t.Context(), http.MethodGet, base+suffix, nil)) {
			t.Errorf("allowed %q", suffix)
		}
	}
}
