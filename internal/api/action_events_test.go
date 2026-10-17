package api_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/telemetry"
	"go.kenn.io/kit/telemetry/posthog"
)

type actionCollector struct {
	reporter *telemetry.Reporter
	capture  *telemetry.Capture
	events   chan map[string]any
}

func newActionCollector(t *testing.T) *actionCollector {
	t.Helper()
	if posthog.ProcessDisabled() {
		t.Skip("built with kit_posthog_disabled")
	}
	c := &actionCollector{events: make(chan map[string]any, 256)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch struct {
			Batch []map[string]any `json:"batch"`
		}
		assert.NoError(t, json.UnmarshalRead(r.Body, &batch))
		for _, event := range batch.Batch {
			c.events <- event
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	c.reporter = telemetry.New(telemetry.Options{Dir: t.TempDir(), Endpoint: server.URL})
	t.Cleanup(func() { require.NoError(t, c.reporter.Close()) })
	c.capture = telemetry.CaptureHandler(c.reporter, t.TempDir())
	return c
}

func (c *actionCollector) requireEvent(t *testing.T, event string, properties map[string]any) {
	t.Helper()
	require.NoError(t, c.reporter.Close())
	require.Len(t, c.events, 1)
	delivered := <-c.events
	require.Equal(t, event, delivered["event"])
	require.Subset(t, delivered["properties"], properties)
}
