package telemetry

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCoreActionsValidateAndForwardEveryReport(t *testing.T) {
	enableTelemetryEnv(t)
	var mu sync.Mutex
	var events []map[string]any
	counts := map[string]int{}
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch postHogBatch
		assert.NoError(t, json.UnmarshalRead(r.Body, &batch))
		mu.Lock()
		defer mu.Unlock()
		for _, event := range batch.Batch {
			events = append(events, event.Properties)
			counts[event.Event]++
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()
	dir := t.TempDir()
	reporter := New(Options{Dir: dir, Version: "v1.0.0", endpoint: collector.URL, Logger: discardLogger()})
	handler := CaptureHandler(reporter, dir)
	for _, event := range []string{"document_added", "document_opened", "search_run"} {
		for _, surface := range []string{"web", "tui", "cli"} {
			properties := fmt.Sprintf(`"surface":%q`, surface)
			if event == "search_run" {
				properties += `,"search_type":"hybrid"`
			}
			for range 2 {
				response := postEvent(t, handler, fmt.Sprintf(`{"event":%q,"properties":{%s,"query":"synthetic private query"}}`, event, properties))
				require.Equal(t, 202, response.Code, response.Body.String())
			}
		}
	}
	for _, event := range []string{"document_added", "document_opened", "search_run"} {
		require.Equal(t, 400, postEvent(t, handler, fmt.Sprintf(`{"event":%q,"properties":{"surface":"other","search_type":"text"}}`, event)).Code)
	}
	for _, body := range []string{
		`{"event":"document_added"}`,
		`{"event":"search_run","properties":{"surface":"web"}}`,
		`{"event":"search_run","properties":{"surface":"web","search_type":"auto"}}`,
		`{"event":"search_run","properties":{"search_type":"text"}}`,
	} {
		require.Equal(t, 400, postEvent(t, handler, body).Code)
	}
	for _, mode := range []string{"text", "semantic"} {
		require.Equal(t, 202, postEvent(t, handler, fmt.Sprintf(`{"event":"search_run","properties":{"surface":"cli","search_type":%q}}`, mode)).Code)
	}
	require.NoError(t, reporter.Close())
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, map[string]int{"document_added": 6, "document_opened": 6, "search_run": 8}, counts)
	for _, properties := range events {
		assert.Contains(t, []string{"web", "tui", "cli"}, properties["surface"])
		assert.NotContains(t, properties, "query")
	}
}
