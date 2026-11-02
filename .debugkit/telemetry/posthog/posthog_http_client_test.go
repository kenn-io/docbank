package posthog

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostEventResults(t *testing.T) {
	t.Run("missing client", func(t *testing.T) {
		status, err := PostEvent(t.Context(), nil, "http://localhost/events", "screen_viewed", nil)
		require.ErrorContains(t, err, "HTTP client is required")
		assert.Empty(t, status)
	})
	for _, tc := range []struct {
		name, body string
		code       int
		want       Status
	}{
		{"queued", `{"status":"queued"}`, 202, StatusQueued},
		{"skipped", `{"status":"skipped"}`, 202, StatusSkipped},
		{"disabled", `{"status":"disabled"}`, 202, StatusDisabled},
		{"http failure", `{"status":"queued"}`, 500, ""},
		{"unexpected http", `{"status":"queued"}`, 200, ""},
		{"invalid json", "{", 202, ""},
		{"trailing json", `{"status":"queued"} {}`, 202, ""},
		{"unknown status", `{"status":"later"}`, 202, ""},
		{"missing status", `{}`, 202, ""},
		{"oversized", strings.Repeat(" ", maxPostHogCaptureBodyBytes) + `{}`, 202, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
				var request postHogCaptureRequest
				if err := json.NewDecoder(r.Body).Decode(&request); !assert.NoError(t, err) {
					http.Error(w, "invalid event", http.StatusBadRequest)
					return
				}
				assert.Equal(t, "screen_viewed", request.Event)
				assert.Equal(t, map[string]any{"screen": "queue"}, request.Properties)
				w.WriteHeader(tc.code)
				_, err := w.Write([]byte(tc.body))
				assert.NoError(t, err)
			}))
			t.Cleanup(srv.Close)
			status, err := PostEvent(t.Context(), srv.Client(), srv.URL, "screen_viewed", map[string]any{"screen": "queue"})
			assert.Equal(t, tc.want, status)
			if tc.want == "" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if tc.name == "http failure" {
				srv.Close()
				status, err = PostEvent(t.Context(), srv.Client(), srv.URL, "screen_viewed", map[string]any{"screen": "queue"})
				require.Error(t, err)
				assert.Empty(t, status)
			}
		})
	}
	t.Run("same-day route results", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "days.json")
		r, client, _ := newInstallAgeTestReporter(t, postHogTestStart,
			WithAllowedEvent("screen_viewed", RequireProperty("screen", AllowStringValues("queue"))),
			WithDailyEvent("screen_viewed", "screen", NewDailyClaims(path)))
		h := NewCaptureHandler(r)
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		props := map[string]any{"screen": "queue"}
		status, err := PostEvent(t.Context(), srv.Client(), srv.URL, "screen_viewed", props)
		require.NoError(t, err)
		assert.Equal(t, StatusQueued, status)
		status, err = PostEvent(t.Context(), srv.Client(), srv.URL, "screen_viewed", props)
		require.NoError(t, err)
		assert.Equal(t, StatusSkipped, status)
		require.Len(t, client.messages, 1)
	})
}
