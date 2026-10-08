package api

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/telemetry"
)

const telemetryEventsPath = "/api/daemon/telemetry/events"

// TelemetryEventRequest documents what interfaces post; the daemon's capture handler decodes it.
type TelemetryEventRequest struct {
	Properties *TelemetryEventProperties `json:"properties,omitempty" doc:"Event properties. screen_viewed requires screen and surface; session_ended takes surface and duration_bucket."`
	Event      string                    `json:"event" minLength:"1" doc:"An event the daemon's telemetry allowlist names. Other events return 400."`
}

// TelemetryEventProperties lists the properties the telemetry allowlist sends.
type TelemetryEventProperties struct {
	Screen         string `json:"screen,omitempty" doc:"The screen shown. Other values return 400 for screen_viewed."`
	Surface        string `json:"surface,omitempty" enum:"web,tui" doc:"The interface that sent the event."`
	DurationBucket string `json:"duration_bucket,omitempty" enum:"under_1m,1_to_5m,5_to_30m,over_30m" doc:"How long a session_ended session lasted."`
}

// TransformSchema publishes the telemetry allowlist's screen names as the screen enum.
func (TelemetryEventProperties) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	screen := s.Properties["screen"]
	for _, name := range telemetry.ScreenNames() {
		screen.Enum = append(screen.Enum, name)
	}
	return s
}

// TelemetryEventReceipt documents the accepted response.
type TelemetryEventReceipt struct {
	Status string `json:"status" enum:"queued,disabled" doc:"queued when the event is queued or its screen was already sent today; disabled when telemetry is off and nothing is sent."`
}

// registerTelemetryEvents mounts the interface usage-event route; nil leaves it unregistered (offline OpenAPI, tests).
func registerTelemetryEvents(mux *http.ServeMux, capture http.Handler) {
	if capture == nil {
		return
	}
	mux.Handle("POST "+telemetryEventsPath, capture)
}
