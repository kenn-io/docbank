package api_test

import (
	"fmt"
	"net/http"
	"sync"
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
	concurrent := uuid.NewV4().String()
	var uploads sync.WaitGroup
	for i := range 8 {
		uploads.Go(func() { upload(concurrent, fmt.Sprintf("concurrent-%d.txt", i), "cli") })
	}
	uploads.Wait()
	// Interrupted runs never finish; they must not stop later runs from reporting.
	for i := range 257 {
		upload(uuid.NewV4().String(), fmt.Sprintf("file-%d.txt", i), "cli")
	}
	upload(active, "late.txt", "cli")
	upload(uuid.NewV4().String(), "scripted.txt", "")
	require.NoError(t, capture.reporter.Close())
	require.Len(t, capture.events, 259)
	for len(capture.events) > 0 {
		event := <-capture.events
		require.Equal(t, "document_added", event["event"])
		require.Subset(t, event["properties"], map[string]any{"surface": "cli"})
	}
}
