package telemetry

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/telemetry/posthog"
)

func screenBody(screen, surface string) string {
	return fmt.Sprintf(`{"event":"screen_viewed","properties":{"screen":%q,"surface":%q,"query":"private synthetic text"}}`, screen, surface)
}

func screenCollector(t *testing.T, mu *sync.Mutex, events *[]map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		var batch postHogBatch
		assert.NoError(t, json.Unmarshal(body, &batch))
		mu.Lock()
		for _, event := range batch.Batch {
			*events = append(*events, event.Properties)
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
}

func TestCaptureHandlerRejectsInvalidRequests(t *testing.T) {
	enableTelemetryEnv(t)
	var mu sync.Mutex
	var events []map[string]any
	collector := screenCollector(t, &mu, &events)
	defer collector.Close()
	dir := t.TempDir()
	reporter := New(Options{Dir: dir, endpoint: collector.URL, Logger: discardLogger()})
	handler := CaptureHandler(reporter, dir)
	for _, tc := range []struct {
		name, media, body string
		status            int
	}{
		{"unknown screen", "application/json", screenBody("unknown", "web"), http.StatusBadRequest},
		{"unknown surface", "application/json", screenBody("browse", "unknown"), http.StatusBadRequest},
		{"missing properties", "application/json", `{"event":"screen_viewed"}`, http.StatusBadRequest},
		{"properties not an object", "application/json", `{"event":"screen_viewed","properties":[]}`, http.StatusBadRequest},
		{"event key in another case", "application/json", `{"Event":"app_opened"}`, http.StatusBadRequest},
		{"duplicate event", "application/json", `{"event":"app_opened","event":"screen_viewed"}`, http.StatusBadRequest},
		{"duplicate property", "application/json", `{"event":"screen_viewed","properties":{"screen":"browse","screen":"help","surface":"web"}}`, http.StatusBadRequest},
		{"unknown event", "application/json", `{"event":"search_run"}`, http.StatusBadRequest},
		{"blank event", "application/json", `{"event":" "}`, http.StatusBadRequest},
		{"trailing data", "application/json", screenBody("browse", "web") + `{}`, http.StatusBadRequest},
		{"truncated", "application/json", `{"event":"screen_viewed"`, http.StatusBadRequest},
		{"text body", "text/plain", screenBody("browse", "web"), http.StatusUnsupportedMediaType},
		{"too large", "application/json", screenBody("browse", "web") + strings.Repeat(" ", maxCaptureBodyBytes), http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/daemon/telemetry/events", strings.NewReader(tc.body))
			request.Header.Set("Content-Type", tc.media)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			assert.Equal(t, tc.status, response.Code, response.Body.String())
		})
	}
	require.NoError(t, reporter.Close())
	require.NoFileExists(t, filepath.Join(dir, screenClaimsFile))
	mu.Lock()
	defer mu.Unlock()
	assert.Empty(t, events)
}

func TestScreenClaimsAcrossInterfacesRestartsAndDays(t *testing.T) {
	enableTelemetryEnv(t)
	var mu sync.Mutex
	var events []map[string]any
	collector := screenCollector(t, &mu, &events)
	defer collector.Close()
	dir := t.TempDir()
	makeHandler := func() (*Reporter, *captureHandler) {
		reporter := New(Options{Dir: dir, endpoint: collector.URL, Logger: discardLogger()})
		handler, ok := CaptureHandler(reporter, dir).(*captureHandler)
		require.True(t, ok)
		return reporter, handler
	}
	reporter, handler := makeHandler()
	now := time.Date(2026, 1, 2, 23, 59, 0, 0, time.UTC)
	handler.now = func() time.Time { return now }
	for range 2 {
		for _, surface := range []string{"web", "tui"} {
			require.Equal(t, 202, postEvent(t, handler, screenBody("browse", surface)).Code)
		}
	}
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Go(func() {
			surface := "web"
			if i%2 == 0 {
				surface = "tui"
			}
			assert.Equal(t, 202, postEvent(t, handler, screenBody("browse", surface)).Code)
		})
	}
	wg.Wait()
	require.Equal(t, 202, postEvent(t, handler, screenBody("search", "tui")).Code)
	require.NoError(t, reporter.Close())
	reporter, handler = makeHandler()
	handler.now = func() time.Time { return now }
	require.Equal(t, 202, postEvent(t, handler, screenBody("browse", "tui")).Code)
	now = now.Add(time.Minute)
	require.Equal(t, 202, postEvent(t, handler, screenBody("browse", "tui")).Code)
	require.NoError(t, reporter.Close())
	require.NoError(t, os.Remove(filepath.Join(dir, posthog.InstallFileName)))
	reporter, handler = makeHandler()
	handler.now = func() time.Time { return now }
	require.Equal(t, 202, postEvent(t, handler, screenBody("browse", "web")).Code)
	require.NoError(t, reporter.Close())
	for i, directory := range []bool{false, true} {
		dir = t.TempDir()
		reporter, handler = makeHandler()
		path := filepath.Join(dir, screenClaimsFile)
		if directory {
			require.NoError(t, os.Mkdir(path, 0o700))
		} else {
			require.NoError(t, os.WriteFile(path, []byte("garbage"), 0o600))
		}
		require.Equal(t, 202, postEvent(t, handler, screenBody("browse", "web")).Code)
		require.Equal(t, 202, postEvent(t, handler, screenBody("browse", "web")).Code)
		require.NoError(t, reporter.Close())
		mu.Lock()
		assert.Len(t, events, 6+i)
		assert.Equal(t, "browse", events[len(events)-1]["screen"])
		assert.Equal(t, "web", events[len(events)-1]["surface"])
		mu.Unlock()
	}
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, events, 7)
	assert.Equal(t, "browse", events[0]["screen"])
	assert.Equal(t, "web", events[0]["surface"])
	assert.Equal(t, "browse", events[1]["screen"])
	assert.Equal(t, "tui", events[1]["surface"])
	for _, props := range events {
		assert.NotContains(t, props, "query")
	}
}
