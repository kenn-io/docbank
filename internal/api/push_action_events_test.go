package api_test

import (
	"net/http"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestPushRunsReportFirstAdditionOnce(t *testing.T) {
	t.Setenv("DOCBANK_TELEMETRY_ENABLED", "1")
	t.Setenv("TELEMETRY_ENABLED", "1")
	capture := newActionCollector(t)
	server, catalog := newTestServer(t, func(d *api.Deps) { d.TelemetryCapture = capture.capture })
	upload := func(runID, name, surface string) {
		data := []byte("synthetic content")
		response, body := sendUpload(t, server.URL, server.Client(), catalog.RootID(), uploadRequest{
			name: name, sourceRef: name, pushName: "synthetic", runID: runID,
			content: data, expectedHash: uploadIdentity(data), expectedSize: int64(len(data)), surface: surface,
		})
		require.Equal(t, http.StatusCreated, response.StatusCode, body)
	}
	active := uuid.NewV4().String()
	upload(active, "first.txt", "cli")
	upload(active, "late.txt", "cli")
	upload(uuid.NewV4().String(), "scripted.txt", "")
	require.NoError(t, capture.reporter.Close())
	require.Len(t, capture.events, 1)
	for len(capture.events) > 0 {
		event := <-capture.events
		require.Equal(t, "document_added", event["event"])
		require.Subset(t, event["properties"], map[string]any{"surface": "cli"})
	}
}
