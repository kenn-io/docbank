package telemetry

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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

func TestScreenClaimsAcrossInterfacesRestartsAndDays(t *testing.T) {
	enableTelemetryEnv(t)
	var mu sync.Mutex
	var events []map[string]any
	collector := screenCollector(t, &mu, &events)
	defer collector.Close()
	dir := t.TempDir()
	makeHandler := func() (*Reporter, *screenCapture) {
		reporter := New(Options{Dir: dir, endpoint: collector.URL, Logger: discardLogger()})
		handler, ok := CaptureHandler(reporter, dir).(*screenCapture)
		require.True(t, ok)
		return reporter, handler
	}
	reporter, handler := makeHandler()
	now := time.Date(2026, 1, 2, 23, 59, 0, 0, time.UTC)
	handler.now = func() time.Time { return now }
	for _, body := range []string{screenBody("unknown", "web"), screenBody("browse", "unknown"), `{"event":"screen_viewed"}`} {
		require.Equal(t, 202, postEvent(t, handler, body).Code)
	}
	require.NoFileExists(t, filepath.Join(dir, screenClaimsFile))
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
	require.Equal(t, 202, postEvent(t, handler, `{"Event":" screen_viewed ","properties":{"screen":"browse","surface":"web"}}`).Code)
	require.Equal(t, 202, postEvent(t, handler, `{"event":"screen_viewed","event":null,"properties":{"screen":"browse","surface":"web"}}`).Code)
	require.Equal(t, 202, postEvent(t, handler, `{"event":"app_opened","Event":"screen_viewed","properties":{"screen":"browse","surface":"web"}}`).Code)

	require.Equal(t, 202, postEvent(t, handler, screenBody("search", "tui")).Code)
	// Malformed duplicates retain kit's transport validation.
	for _, body := range []string{screenBody("browse", "web") + `{}`, `{"event":"screen_viewed","properties":[]}`, `{"event":"screen_viewed"`} {
		require.Equal(t, 400, postEvent(t, handler, body).Code)
	}
	for _, tc := range []struct {
		media, body string
		status      int
	}{
		{"text/plain", screenBody("browse", "web"), http.StatusUnsupportedMediaType},
		{"application/json", screenBody("browse", "web") + strings.Repeat(" ", 64<<10), http.StatusRequestEntityTooLarge},
	} {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost/api/daemon/telemetry/events", strings.NewReader(tc.body))
		request.Header.Set("Content-Type", tc.media)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		assert.Equal(t, tc.status, response.Code)
	}
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
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, events, 5)
	assert.Equal(t, "browse", events[0]["screen"])
	assert.Equal(t, "web", events[0]["surface"])
	assert.Equal(t, "browse", events[1]["screen"])
	assert.Equal(t, "tui", events[1]["surface"])
	for _, props := range events {
		assert.NotContains(t, props, "query")
	}
}

func TestScreenCorruptClaimsStillCount(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(strconv.FormatBool(directory), func(t *testing.T) {
			enableTelemetryEnv(t)
			var mu sync.Mutex
			var events []map[string]any
			collector := screenCollector(t, &mu, &events)
			defer collector.Close()
			dir := t.TempDir()
			reporter := New(Options{Dir: dir, endpoint: collector.URL, Logger: discardLogger()})
			handler, ok := CaptureHandler(reporter, dir).(*screenCapture)
			require.True(t, ok)
			path := filepath.Join(dir, screenClaimsFile)
			if directory {
				require.NoError(t, os.Mkdir(path, 0700))
			} else {
				require.NoError(t, os.WriteFile(path, []byte("garbage"), 0600))
			}
			require.Equal(t, 202, postEvent(t, handler, screenBody("browse", "web")).Code)
			require.Equal(t, 202, postEvent(t, handler, screenBody("browse", "web")).Code)
			if !directory {
				assert.True(t, handler.load().Screens["browse|web"])
			}
			require.NoError(t, reporter.Close())
			mu.Lock()
			defer mu.Unlock()
			require.Len(t, events, 1)
			assert.Equal(t, "browse", events[0]["screen"])
			assert.Equal(t, "web", events[0]["surface"])
		})
	}
}

func TestScreenClosedReporterKeepsNoClaim(t *testing.T) {
	enableTelemetryEnv(t)
	dir := t.TempDir()
	reporter := New(Options{Dir: dir, endpoint: "http://127.0.0.1:1", Logger: discardLogger()})
	handler, ok := CaptureHandler(reporter, dir).(*screenCapture)
	require.True(t, ok)
	require.NoError(t, reporter.Close())
	require.Equal(t, 202, postEvent(t, handler, screenBody("browse", "web")).Code)
	assert.Empty(t, handler.load().Screens)
}
