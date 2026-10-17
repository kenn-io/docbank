package daemonconn

import (
	"context"
	"net/http"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/telemetry"
)

type actionSurfaceKey struct{}

// WithActionSurface attributes imports started with ctx to an interactive surface.
func WithActionSurface(ctx context.Context, surface string) context.Context {
	return context.WithValue(ctx, actionSurfaceKey{}, surface)
}

// SendActionSurface is a request editor that forwards WithActionSurface attribution.
func SendActionSurface(ctx context.Context, request *http.Request) error {
	if surface, _ := ctx.Value(actionSurfaceKey{}).(string); surface != "" {
		request.Header.Set(api.ActionSurfaceHeader, surface)
	}
	return nil
}

// ReportAction sends a core action with the search mode normalized for telemetry.
func (c *Connection) ReportAction(ctx context.Context, event, surface, value string) (*apiclient.ReportTelemetryEventResponse, error) {
	props := &apiclient.TelemetryEventProperties{Surface: surface}
	if event == telemetry.EventSearchRun {
		props.SearchType = telemetry.SearchType(value)
	}
	return c.API().ReportTelemetryEvent(ctx, &apiclient.ReportTelemetryEventRequestOptions{Body: &apiclient.ReportTelemetryEventBody{Event: event, Properties: props}})
}
