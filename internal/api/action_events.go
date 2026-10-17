package api

import (
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
	if surface := browserActionSurface(r); surface != "" || r.Header.Get(ActionSurfaceHeader) != "cli" {
		return surface
	}
	return "cli"
}
