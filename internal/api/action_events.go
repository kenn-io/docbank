package api

import (
	"net/http"
)

func actionSurface(r *http.Request) string {
	if browserSessionRequest(r.Context()) {
		return "web"
	}
	if r.Header.Get("X-Docbank-Surface") == "cli" {
		return "cli"
	}
	return ""
}
