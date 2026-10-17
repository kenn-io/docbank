package api_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/telemetry"
	"go.kenn.io/kit/telemetry/posthog"
)

type actionCollector struct {
	reporter *telemetry.Reporter
	imports  *telemetry.Imports
	mu       sync.Mutex
	events   []map[string]any
}

func newActionCollector(t *testing.T) *actionCollector {
	t.Helper()
	if posthog.ProcessDisabled() {
		t.Skip("built with kit_posthog_disabled")
	}
	c := new(actionCollector)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch struct {
			Batch []map[string]any `json:"batch"`
		}
		assert.NoError(t, json.UnmarshalRead(r.Body, &batch))
		c.mu.Lock()
		c.events = append(c.events, batch.Batch...)
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	var err error
	c.reporter, err = posthog.NewReporter(posthog.Options{APIKey: "synthetic-key", Endpoint: server.URL, Application: "docbank", EnvPrefix: "DOCBANK", DistinctID: "synthetic-install"}, posthog.WithAllowedEvent("document_added", posthog.AllowProperty("surface", posthog.AllowStringValues("web", "cli"))), posthog.WithAllowedEvent("screen_viewed", posthog.AllowProperty("screen", posthog.AllowStringValues("browse")), posthog.AllowProperty("surface", posthog.AllowStringValues("web"))))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, c.reporter.Close()) })
	c.imports = telemetry.NewImports(c.reporter)
	return c
}

func (c *actionCollector) requireEvent(t *testing.T, event string, properties map[string]any) {
	t.Helper()
	require.NoError(t, c.reporter.Close())
	c.mu.Lock()
	defer c.mu.Unlock()
	require.Len(t, c.events, 1)
	require.Equal(t, event, c.events[0]["event"])
	require.Subset(t, c.events[0]["properties"], properties)
}
