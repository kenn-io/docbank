package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDocumentFactsBrowserAllowlist(t *testing.T) {
	t.Parallel()
	base := "/api/v1/versions/00000000-0000-4000-8000-000000000000/"
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{http.MethodGet, base + "dates", true},
		{http.MethodGet, base + "people?disclosure=safe", true},
		{http.MethodGet, base + "dates?disclosure=full", true},
		{http.MethodGet, base + "people?disclosure=full&disclosure=safe", false},
		{http.MethodGet, base + "dates?other=1", false},
		{http.MethodGet, base + "people?disclosure=bad", false},
		{http.MethodGet, "/api/v1/versions/not-a-uuid/dates", false},
		{http.MethodGet, base + "people/extra", false},
		{http.MethodPost, base + "people", false},
	} {
		if got := webSessionRequestAllowed(httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, nil)); got != tc.want {
			t.Errorf("%s %s: allowed = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}
