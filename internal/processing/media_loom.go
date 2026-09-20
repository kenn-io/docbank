package processing

import (
	"net/url"
	"strings"
)

// loomOrigin scopes Loom identities. #240 treats loom.com and www.loom.com
// as one service.
const loomOrigin = "https://www.loom.com"

// loomRecording returns the route-qualified identity of a canonical Loom
// share or embed URL. Loom documents no equality between the two routes, so
// they remain separate identities. Every other URL keeps generic identity.
func loomRecording(canonicalURL string) (string, bool) {
	parsed, err := url.Parse(canonicalURL)
	if err != nil || parsed.Scheme != "https" || parsed.Port() != "" ||
		parsed.EscapedPath() != parsed.Path ||
		(parsed.Hostname() != "loom.com" && parsed.Hostname() != "www.loom.com") {
		return "", false
	}
	route, recordingID, ok := strings.Cut(strings.TrimPrefix(parsed.Path, "/"), "/")
	if !ok || (route != "share" && route != "embed") || recordingID == "" || strings.Contains(recordingID, "/") {
		return "", false
	}
	return route + "/" + recordingID, true
}
