package api

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/telemetry"
)

const telemetryEventsPath = "/api/daemon/telemetry/events"

// TelemetryEventRequest documents what interfaces post; the daemon's capture handler decodes it.
type TelemetryEventRequest struct {
	Properties *TelemetryEventProperties `json:"properties,omitempty"`
	Event      string                    `json:"event" minLength:"1"`
}

// TelemetryEventProperties lists the properties the telemetry allowlist sends.
type TelemetryEventProperties struct {
	Kind           string `json:"kind,omitempty"`
	SearchType     string `json:"search_type,omitempty" enum:"text,semantic,hybrid"`
	Screen         string `json:"screen,omitempty"`
	Surface        string `json:"surface,omitempty" enum:"web,tui,cli"`
	DurationBucket string `json:"duration_bucket,omitempty"`
}

// TransformSchema publishes telemetry's allowed screen and duration values.
func (TelemetryEventProperties) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	screen := s.Properties["screen"]
	for _, name := range telemetry.ScreenNames() {
		screen.Enum = append(screen.Enum, name)
	}
	s.Properties["duration_bucket"].Enum = durationBucketEnum()
	return s
}

func durationBucketEnum() []any {
	buckets := telemetry.DurationBuckets()
	values := make([]any, len(buckets))
	for i, bucket := range buckets {
		values[i] = bucket
	}
	return values
}

// TelemetryEventReceipt documents the accepted response.
type TelemetryEventReceipt struct {
	Status string `json:"status" enum:"queued,disabled"`
}

// registerTelemetryEvents mounts the interface usage-event route; nil leaves it unregistered (offline OpenAPI, tests).
func registerTelemetryEvents(mux *http.ServeMux, capture http.Handler) {
	if capture == nil {
		return
	}
	mux.Handle("POST "+telemetryEventsPath, capture)
}
