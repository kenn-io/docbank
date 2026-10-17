package daemonconn

import (
	"context"
	"net/http"

	"go.kenn.io/docbank/internal/api"
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
