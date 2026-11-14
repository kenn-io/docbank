package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDocumentFactsBrowserAllowlist(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"dates", "people"} {
		base := "/api/v1/versions/00000000-0000-4000-8000-000000000000/" + kind
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
		for _, path := range []string{base + "/extra", "/api/v1/versions/not-a-uuid/" + kind} {
			if webSessionRequestAllowed(httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)) {
				t.Errorf("allowed %q", path)
			}
		}
		if webSessionRequestAllowed(httptest.NewRequestWithContext(t.Context(), http.MethodPost, base, nil)) {
			t.Errorf("allowed POST %q", base)
		}
	}
}

func TestDocumentFactsCacheRoutes(t *testing.T) {
	t.Parallel()
	handler := photoHiddenMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	for _, tc := range []struct {
		path string
		want string
	}{
		{"/api/v1/versions/not-a-uuid/dates", "no-store"},
		{"/api/v1/versions/not-a-uuid/people", "no-store"},
		{"/api/v1/versions/id/people/extra", ""},
		{"/api/v1/versions/id/content", ""},
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, tc.path, nil))
		if got := w.Header().Get("Cache-Control"); got != tc.want {
			t.Errorf("%s: cache control = %q, want %q", tc.path, got, tc.want)
		}
	}
}
