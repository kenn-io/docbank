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
