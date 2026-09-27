package api

import (
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// batesBrowserRequestAllowed exposes only the reviewed Bates workflow. Raw PDF
// content is deliberately excluded; browsers receive a one-use verified
// download ticket instead.
func batesBrowserRequestAllowed(r *http.Request) bool {
	const prefix = "/api/v1/bates/"
	path, ok := strings.CutPrefix(r.URL.Path, prefix)
	if !ok || path == "" || r.Method != http.MethodGet && r.URL.RawQuery != "" {
		return false
	}
	parts := strings.Split(path, "/")
	switch parts[0] {
	case "namespaces":
		return len(parts) == 1 && (r.Method == http.MethodGet || r.Method == http.MethodPost)
	case "preview":
		return len(parts) == 1 && r.Method == http.MethodPost
	case "allocations":
		if len(parts) == 1 {
			return r.Method == http.MethodPost
		}
		return len(parts) == 2 && r.Method == http.MethodGet && validBatesBrowserID(parts[1])
	case "exports":
		if len(parts) == 1 {
			return r.Method == http.MethodGet || r.Method == http.MethodPost
		}
		if len(parts) == 2 {
			return r.Method == http.MethodGet && validBatesBrowserID(parts[1])
		}
		return len(parts) == 3 && r.Method == http.MethodPost && validBatesBrowserID(parts[1]) && parts[2] == "download"
	}
	return false
}

func validBatesBrowserID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id.Version() == 4
}
