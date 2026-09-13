package ingest

import (
	"mime"
	"net/http"
	"path/filepath"
)

// detectMime resolves a MIME type from the file extension, falling back to
// content sniffing over the first 512 bytes.
func detectMime(path string, head []byte) string {
	return detectMimeWithExtension(path, head, mime.TypeByExtension)
}

func detectMimeWithExtension(path string, head []byte, byExtension func(string) string) string {
	extension := filepath.Ext(path)
	if len(extension) == 4 && extension[0] == '.' &&
		(extension[1] == 'e' || extension[1] == 'E') &&
		(extension[2] == 'm' || extension[2] == 'M') &&
		(extension[3] == 'l' || extension[3] == 'L') {
		return "message/rfc822"
	}
	if byExt := byExtension(extension); byExt != "" {
		return byExt
	}
	return http.DetectContentType(head)
}
