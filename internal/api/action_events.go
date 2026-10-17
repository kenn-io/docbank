package api

import (
	"context"
	"net/http"
)

func browserActionSurface(r *http.Request) string {
	if browserSessionRequest(r.Context()) {
		return "web"
	}
	return ""
}

// ActionSurfaceHeader lets an interactive CLI attribute the import it starts.
const ActionSurfaceHeader = "X-Docbank-Surface"

// importActionSurface prefers the browser session over a client's CLI claim.
func importActionSurface(r *http.Request) string {
	if surface := browserActionSurface(r); surface != "" {
		return surface
	}
	return cliImportSurface(r.Context(), r.Header.Get(ActionSurfaceHeader))
}

func cliImportSurface(ctx context.Context, surface string) string {
	if surface == "cli" && !browserSessionRequest(ctx) {
		return "cli"
	}
	return ""
}
