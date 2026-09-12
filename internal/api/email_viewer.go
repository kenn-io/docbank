package api

import (
	"net/http"
	"strings"

	"go.kenn.io/docbank/document"
)

// Browser presentation consumes retained authority only; decoding stays on
// the existing processing surface, outside the browser read allowlist.
func emailViewerBrowserReadAllowed(r *http.Request) bool {
	if r.Method != http.MethodGet || r.URL.RawQuery != "" {
		return false
	}
	after, ok := strings.CutPrefix(r.URL.Path, "/api/v1/versions/")
	if !ok {
		return false
	}
	parts := strings.Split(after, "/")
	if len(parts) != 2 && len(parts) != 4 && len(parts) != 7 {
		return false
	}
	_, err := document.NormalizeEmailDocumentRelationQuery(document.EmailDocumentRelationQuery{ParentVersionID: parts[0]})
	if err != nil || parts[1] != "email" {
		return false
	}
	if len(parts) == 2 {
		return true
	}
	if parts[2] != "generations" || len(parts[3]) != 64 || strings.Trim(parts[3], "0123456789abcdef") != "" {
		return false
	}
	return len(parts) == 4 || parts[4] == "parts" && document.ValidateEmailPartPath(parts[5]) == nil && emailArtifactRole(parts[6])
}
