package media

import (
	"errors"
	"math"
)

// Format identifies a detected media container.
type Format string

// Detected media formats.
const (
	FormatJPEG Format = "jpeg"
	FormatPNG  Format = "png"
	FormatWebP Format = "webp"
	FormatGIF  Format = "gif"
	FormatMP4  Format = "mp4"
)

// Kind classifies detected media as still or moving picture input.
type Kind string

// Detected media kinds.
const (
	KindImage Kind = "image"
	KindVideo Kind = "video"
)

// Metadata describes detected media without exposing its bytes.
type Metadata struct {
	// Format is the sniffed container format.
	Format Format `json:"format"`
	// Kind is image or video.
	Kind Kind `json:"kind"`
	// MediaType is the canonical media type for Format.
	MediaType string `json:"media_type"`
	// DeclaredMediaType is the caller-declared media type, recorded verbatim.
	DeclaredMediaType string `json:"declared_media_type,omitempty"`
	// Size is the input length in bytes.
	Size int64 `json:"size"`
	// Width and Height are pixel dimensions.
	Width  int64 `json:"width"`
	Height int64 `json:"height"`
	// FrameCount is the number of image frames; zero for video.
	FrameCount int `json:"frame_count,omitempty"`
	// DurationMS is the video duration in milliseconds when DurationKnown.
	DurationMS int64 `json:"duration_ms,omitempty"`
	// DurationKnown reports that the container declared a measurable
	// duration. Fragmented or header-only video leaves it false.
	DurationKnown bool `json:"duration_known,omitempty"`
	// Animated reports a multi-frame image.
	Animated bool `json:"animated,omitempty"`
}

// Pixels returns the total pixel count, zero when dimensions are unknown, or
// math.MaxInt64 when the product would overflow.
func (m Metadata) Pixels() int64 {
	if m.Width <= 0 || m.Height <= 0 {
		return 0
	}
	if m.Width > math.MaxInt64/m.Height {
		return math.MaxInt64
	}
	return m.Width * m.Height
}

// Sentinel detection errors.
var (
	// ErrUnsupportedMedia reports bytes that are not a supported image or
	// video container.
	ErrUnsupportedMedia = errors.New("media: unsupported media type")
	// ErrMalformedMedia reports a recognized container that cannot yield
	// bounded metadata.
	ErrMalformedMedia = errors.New("media: malformed media")
)
