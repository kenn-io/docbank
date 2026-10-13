package media

import (
	"mime"
	"path/filepath"
	"strings"
)

// MaxMediaSourceBytes is the default and largest configurable streaming audio
// source ceiling. It does not change document, visual, or provider limits.
const MaxMediaSourceBytes int64 = 4 << 30

// InspectionSourceByteCeiling selects the structural source bound. Audio
// inspection streams; other inspectors retain their existing memory bound.
// M4A identities share the audio ceiling when the M4A inspector is installed.
func InspectionSourceByteCeiling(filename, mediaType string) int64 {
	baseType, _, _ := mime.ParseMediaType(mediaType)
	switch baseType {
	case "audio/wav", "audio/x-wav", "audio/mpeg", "audio/mp4", "audio/x-m4a":
		return MaxMediaSourceBytes
	}
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".wav", ".mp3", ".m4a":
		return MaxMediaSourceBytes
	}
	return MaxInspectionSourceBytes
}
