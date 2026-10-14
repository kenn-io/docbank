package media

import (
	"mime"
	"path/filepath"
	"strings"
)

// MaxMediaSourceBytes is the default and largest configurable streaming audio
// source ceiling. It does not change document, visual, or provider limits.
const MaxMediaSourceBytes int64 = 4 << 30

// InspectionSourceByteCeiling selects the structural source bound. WAV and MP3
// inspection streams; every other inspector materializes the source and keeps
// MaxInspectionSourceBytes.
func InspectionSourceByteCeiling(filename, mediaType string) int64 {
	baseType, _, _ := mime.ParseMediaType(mediaType)
	if streamingAudio(baseType, strings.ToLower(filepath.Ext(filename))) {
		return MaxMediaSourceBytes
	}
	return MaxInspectionSourceBytes
}

func streamingAudio(baseType, ext string) bool {
	return baseType == "audio/wav" || baseType == "audio/x-wav" || baseType == "audio/mpeg" ||
		ext == ".wav" || ext == ".mp3"
}
