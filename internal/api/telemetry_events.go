package api

import "net/http"

const (
	telemetryEventsPath = "/api/daemon/telemetry/events"
	// Room for an event name; the reporter drops every property its allowlist omits.
	telemetryEventMaxBodyBytes = 4096
)

// TelemetryEventRequest documents what the web application posts; the daemon's reporter decodes it.
type TelemetryEventRequest struct {
	Event string `json:"event" minLength:"1" doc:"An event the daemon's telemetry allowlist names. Other events return 400."`
}

// TelemetryEventReceipt documents the accepted response.
type TelemetryEventReceipt struct {
	Status string `json:"status" enum:"queued,disabled" doc:"queued when the event will be sent; disabled when telemetry is off and nothing is sent."`
}

// registerTelemetryEvents mounts the web app's usage-event route; nil leaves it unregistered (offline OpenAPI, tests).
func registerTelemetryEvents(mux *http.ServeMux, capture http.Handler) {
	if capture == nil {
		return
	}
	mux.HandleFunc("POST "+telemetryEventsPath, func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, telemetryEventMaxBodyBytes)
		capture.ServeHTTP(w, r)
	})
}
