package api

import (
	"net/http"
	"strings"
)

// Browser sessions may request only the verified, one-use package handoff.
func productionPackageBrowserRequestAllowed(r *http.Request) bool {
	if r.Method != http.MethodPost || r.URL.RawQuery != "" {
		return false
	}
	after, ok := strings.CutPrefix(r.URL.Path, "/api/v1/productions/jobs/")
	if !ok {
		return false
	}
	parts := strings.Split(after, "/")
	return len(parts) == 4 && validBatesBrowserID(parts[0]) &&
		parts[1] == "packages" && validBatesBrowserID(parts[2]) && parts[3] == "download"
}
