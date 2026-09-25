package ingest

import (
	"mime"
	"path/filepath"
	"strings"

	"github.com/gabriel-vasile/mimetype"
)

// detectMime resolves a MIME type from the file bytes and file extension.
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

	detected := mimetype.Detect(head).String()
	mediaType, _, err := mime.ParseMediaType(detected)
	if err != nil {
		mediaType = detected
	}

	switch {
	case strings.HasPrefix(mediaType, "text/"), mediaType == "application/json",
		mediaType == "application/x-ndjson", mediaType == "application/octet-stream",
		mediaType == "application/zip", mediaType == "application/gzip",
		mediaType == "application/ogg", mediaType == "application/x-ole-storage",
		mediaType == "image/tiff", mediaType == "video/mp4":
		if byExt := byExtension(extension); byExt != "" {
			return byExt
		}
	case mediaType == "image/png" || mediaType == "image/vnd.mozilla.apng":
		if strings.EqualFold(extension, ".apng") {
			if byExt := byExtension(extension); byExt == "image/apng" {
				return byExt
			}
		}
	}
	return detected
}
