package processing

import (
	"net/url"
	"strings"
)

// capCloudOrigin scopes Cap Cloud identities. The apex and www hosts serve the
// same share and embed routes, so both select one provider-owned video.
const capCloudOrigin = "https://cap.so"

// capCloudRecording returns the video ID of a canonical Cap Cloud share or
// embed URL. Every other URL keeps the generic URL identity.
func capCloudRecording(canonicalURL string) (string, bool) {
	parsed, err := url.Parse(canonicalURL)
	if err != nil || parsed.Scheme != "https" || parsed.Port() != "" ||
		parsed.EscapedPath() != parsed.Path ||
		(parsed.Hostname() != "cap.so" && parsed.Hostname() != "www.cap.so") {
		return "", false
	}
	route, videoID, ok := strings.Cut(strings.TrimPrefix(parsed.Path, "/"), "/")
	if !ok || (route != "s" && route != "embed" && route != "dev") || videoID == "" || strings.Contains(videoID, "/") {
		return "", false
	}
	return videoID, true
}
