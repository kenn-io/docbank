package api_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestPushClaimsStayActiveUntilFinish(t *testing.T) {
	t.Setenv("DOCBANK_TELEMETRY_ENABLED", "1")
	t.Setenv("TELEMETRY_ENABLED", "1")
	capture := newActionCollector(t)
	server, catalog := newTestServer(t, func(d *api.Deps) { d.TelemetryCapture = capture.capture })
	upload := func(runID, name string) {
		data := []byte("synthetic content")
		response, body := sendUpload(t, server.URL, server.Client(), catalog.RootID(), uploadRequest{
			name: name, sourceRef: name, pushName: "synthetic", runID: runID,
			content: data, expectedHash: uploadIdentity(data), expectedSize: int64(len(data)), surface: "cli",
		})
		require.Equal(t, http.StatusCreated, response.StatusCode, body)
	}
	finish := func(id string, want int) {
		response, body := do(t, server, http.MethodPost, "/api/v1/push/runs/"+id+"/finish", nil, nil)
		require.Equal(t, want, response.StatusCode, body)
	}
	active := uuid.NewV4().String()
	upload(active, "first.txt")
	unreachable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer unreachable.Close()
	require.Error(t, daemonconn.New(unreachable.URL, "synthetic-key").FinishPushRun(t.Context(), active))
	for i := range 256 {
		id := uuid.NewV4().String()
		upload(id, fmt.Sprintf("file-%d.txt", i))
		finish(id, http.StatusNoContent)
		finish(id, http.StatusNoContent)
	}
	upload(active, "last.txt")
	finish(uuid.NewV4().String(), http.StatusNoContent)
	finish("invalid", http.StatusUnprocessableEntity)
	response, body := do(t, server, http.MethodPost, "/api/v1/push/runs/"+active+"/finish", map[string]string{"X-Api-Key": ""}, nil)
	require.Equal(t, http.StatusUnauthorized, response.StatusCode, body)
	require.NoError(t, capture.reporter.Close())
	require.Len(t, capture.events, 257)
	for len(capture.events) > 0 {
		require.Equal(t, "document_added", (<-capture.events)["event"])
	}
}

func TestPushAllActivePressureDropsNewAttribution(t *testing.T) {
	t.Setenv("DOCBANK_TELEMETRY_ENABLED", "1")
	t.Setenv("TELEMETRY_ENABLED", "1")
	capture := newActionCollector(t)
	server, catalog := newTestServer(t, func(d *api.Deps) { d.TelemetryCapture = capture.capture })
	for i := range 257 {
		data := []byte("synthetic content")
		name := fmt.Sprintf("active-%d.txt", i)
		response, body := sendUpload(t, server.URL, server.Client(), catalog.RootID(), uploadRequest{
			name: name, sourceRef: name, pushName: "synthetic", runID: uuid.NewV4().String(),
			content: data, expectedHash: uploadIdentity(data), expectedSize: int64(len(data)), surface: "cli",
		})
		require.Equal(t, http.StatusCreated, response.StatusCode, body)
	}
	require.NoError(t, capture.reporter.Close())
	require.Len(t, capture.events, 256)
}
