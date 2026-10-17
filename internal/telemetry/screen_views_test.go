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

func screenCollector(t *testing.T, mu *sync.Mutex, events *[]postHogEvent) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		var batch postHogBatch
		assert.NoError(t, json.Unmarshal(body, &batch))
		mu.Lock()
		*events = append(*events, batch.Batch...)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
}

func TestCaptureHandlerRejectsInvalidRequests(t *testing.T) {
	enableTelemetryEnv(t)
	var mu sync.Mutex
	var events []postHogEvent
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
		{"unknown event", "application/json", `{"event":"unknown_action","properties":{"surface":"web","search_type":"text"}}`, http.StatusBadRequest},
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
	for _, event := range []string{"document_added", "document_opened", "search_run"} {
		for _, props := range []string{`{"search_type":"text"}`, `{"surface":"invalid","search_type":"text"}`, `{"surface":42,"search_type":"text"}`} {
			require.Equal(t, 400, postEvent(t, handler, fmt.Sprintf(`{"event":%q,"properties":%s}`, event, props)).Code)
		}
	}
	for _, props := range []string{`{"surface":"web"}`, `{"surface":"web","search_type":"auto"}`, `{"surface":"web","search_type":42}`} {
		require.Equal(t, 400, postEvent(t, handler, `{"event":"search_run","properties":`+props+`}`).Code)
	}
	// Only the daemon confirms additions.
	require.Equal(t, 400, postEvent(t, handler, `{"event":" document_added ","properties":{"surface":"web"}}`).Code)
	require.ErrorIs(t, handler.Capture("document_added", map[string]any{"surface": "agent"}), posthog.ErrInvalidProperty)
	require.NoError(t, reporter.Close())
	require.NoFileExists(t, filepath.Join(dir, screenClaimsFile))
	mu.Lock()
	defer mu.Unlock()
	assert.Empty(t, events)
}

func TestCaptureClassifiesSearchesAndClaimsAcrossInterfacesRestartsAndDays(t *testing.T) {
	enableTelemetryEnv(t)
	var mu sync.Mutex
	var events []postHogEvent
	collector := screenCollector(t, &mu, &events)
	defer collector.Close()
	dir := t.TempDir()
	makeHandler := func() (*Reporter, *Capture) {
		reporter := New(Options{Dir: dir, endpoint: collector.URL, Logger: discardLogger()})
		handler := CaptureHandler(reporter, dir)
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
	for _, surface := range []string{"web", "tui", "cli"} {
		// Each import reports its first addition once; a replay keeps the first surface.
		id := "synthetic-import-" + surface
		handler.TrackImport(id, surface)
		handler.TrackImport(id, "web")
		for range 12 {
			wg.Go(func() {
				handler.TrackImport(id, "web")
				handler.ImportAdded(id)
			})
		}
		wg.Wait()
		handler.TrackImport(id, "web")
		handler.ImportAdded(id)
		handler.ForgetImport(id)
	}
	handler.TrackImport("synthetic-finished", "web")
	handler.ForgetImport("synthetic-finished")
	handler.ImportAdded("synthetic-finished")
	handler.ImportAdded("synthetic-untracked")
	for _, event := range []string{"document_opened", "search_run"} {
		for _, surface := range []string{"web", "tui", "cli"} {
			values := []struct{ mode, searchType string }{{"", ""}}
			if event == "search_run" {
				values = []struct{ mode, searchType string }{
					{"text", "text"}, {"semantic", "semantic"}, {"hybrid", "hybrid"},
					{"lexical", "text"}, {"similar", "semantic"},
				}
			}
			for _, value := range values {
				if event == "search_run" {
					require.Equal(t, value.searchType, SearchType(value.mode))
				}
				body := fmt.Sprintf(`{"event":%q,"properties":{"surface":%q,"search_type":%q}}`, event, surface, value.searchType)
				for range 2 {
					require.Equal(t, 202, postEvent(t, handler, body).Code)
				}
			}
		}
	}
	require.NoError(t, handler.Capture("document_opened", map[string]any{"surface": "web"}))
	require.NoError(t, reporter.Close())
	reporter, handler = makeHandler()
	handler.now = func() time.Time { return now }
	require.Equal(t, 202, postEvent(t, handler, screenBody("browse", "tui")).Code)
	now = now.Add(time.Minute)
	require.NoError(t, handler.Capture("search_run", map[string]any{"surface": "web", "search_type": "semantic"}))
	require.NoError(t, handler.Capture("document_opened", map[string]any{"surface": "web"}))
	require.Equal(t, 202, postEvent(t, handler, screenBody("browse", "tui")).Code)
	require.NoError(t, reporter.Close())
	require.NoError(t, os.Remove(filepath.Join(dir, posthog.InstallFileName)))
	reporter, handler = makeHandler()
	handler.now = func() time.Time { return now }
	require.Equal(t, 202, postEvent(t, handler, screenBody("browse", "web")).Code)
	require.NoError(t, reporter.Close())
	for _, directory := range []bool{false, true} {
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
		assert.Equal(t, "screen_viewed", events[len(events)-1].Event)
		assert.Equal(t, "browse", events[len(events)-1].Properties["screen"])
		assert.Equal(t, "web", events[len(events)-1].Properties["surface"])
		mu.Unlock()
	}
	mu.Lock()
	defer mu.Unlock()
	counts := make(map[string]int)
	for _, event := range events {
		props := event.Properties
		surface, ok := props["surface"].(string)
		require.True(t, ok, "surface must be a string")
		key := event.Event + "/" + surface
		switch event.Event {
		case "screen_viewed":
			screen, ok := props["screen"].(string)
			require.True(t, ok, "screen must be a string")
			key += "/" + screen
		case "search_run":
			searchType, ok := props["search_type"].(string)
			require.True(t, ok, "search_type must be a string")
			key += "/" + searchType
		}
		counts[key]++
		assert.NotContains(t, props, "query")
	}
	assert.Equal(t, map[string]int{
		"screen_viewed/web/browse": 4, "screen_viewed/tui/browse": 2, "screen_viewed/tui/search": 1,
		"document_added/web": 1, "document_added/tui": 1, "document_added/cli": 1,
		"document_opened/web": 2, "document_opened/tui": 1, "document_opened/cli": 1,
		"search_run/web/text": 1, "search_run/web/semantic": 2, "search_run/web/hybrid": 1,
		"search_run/tui/text": 1, "search_run/tui/semantic": 1, "search_run/tui/hybrid": 1,
		"search_run/cli/text": 1, "search_run/cli/semantic": 1, "search_run/cli/hybrid": 1,
	}, counts)
}

func TestImportClaimsBoundCompletedRunsAndPreserveActiveOrigins(t *testing.T) {
	t.Setenv(EnabledEnv, "0")
	reporter := New(Options{Dir: t.TempDir()})
	t.Cleanup(func() { require.NoError(t, reporter.Close()) })
	handler := CaptureHandler(reporter, t.TempDir())
	handler.TrackImport("active-mailbox", "web")
	for i := range maxTrackedImports * 2 {
		id := fmt.Sprintf("push:synthetic-run-%d", i)
		handler.TrackImport(id, "cli")
		handler.ImportAdded(id)
		handler.TrackImport(id, "web")
		handler.ImportAdded(id)
		require.Contains(t, handler.imports, id)
		require.Empty(t, handler.imports[id])
		require.LessOrEqual(t, len(handler.imports), maxTrackedImports)
		require.LessOrEqual(t, len(handler.completedImports), maxTrackedImports-1)
	}
	require.Equal(t, "web", handler.imports["active-mailbox"])
	handler.ForgetImport("active-mailbox")
	for i := range maxTrackedImports {
		handler.TrackImport(fmt.Sprintf("active-%d", i), "cli")
	}
	handler.TrackImport("overflow", "web")
	require.NotContains(t, handler.imports, "overflow")
	require.Len(t, handler.imports, maxTrackedImports)
	require.Empty(t, handler.completedImports)
}
