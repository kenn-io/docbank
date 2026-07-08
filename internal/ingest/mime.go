package ingest

import (
	"mime"
	"net/http"
	"path/filepath"
)

// detectMime resolves a MIME type from the file extension, falling back to
// content sniffing over the first 512 bytes.
func detectMime(path string, head []byte) string {
	if byExt := mime.TypeByExtension(filepath.Ext(path)); byExt != "" {
		return byExt
	}
	return http.DetectContentType(head)
}
