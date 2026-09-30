package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestTermReportDownloadTimeoutBoundary(t *testing.T) {
	t.Parallel()
	base := "/api/v1/search-exports/" + strings.Repeat("a", 48)
	for _, tc := range []struct {
		method, path string
		exempt       bool
	}{
		{http.MethodGet, base + "/csv", true},
		{http.MethodGet, base + "/bundle", true},
		{http.MethodPost, base + "/csv", false},
		{http.MethodPost, base + "/bundle", false},
		{http.MethodPost, "/api/v1/search-exports", false},
		{http.MethodPost, base + "/revisions", false},
		{http.MethodGet, base, false},
		{http.MethodGet, base + "/extra/csv", false},
		{http.MethodGet, base + "/bundle/extra", false},
		{http.MethodGet, "/api/v1/search-exports//csv", false},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			parent, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			parentDeadline, _ := parent.Deadline()
			timeoutMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				deadline, ok := r.Context().Deadline()
				assert.True(t, ok)
				if tc.exempt {
					assert.Equal(t, parentDeadline, deadline, "download must preserve the caller's own deadline")
				} else {
					assert.True(t, deadline.Before(parentDeadline), "other routes retain the ordinary request deadline")
				}
				cancel()
				assert.ErrorIs(t, r.Context().Err(), context.Canceled)
			})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(parent, tc.method, tc.path, nil))
		})
	}
}
