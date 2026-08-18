package voyage

import (
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/voyage/internal/probecontract"
)

// CapabilityKind groups capabilities by how they are probed and consumed.
type CapabilityKind string

// Capability kinds.
const (
	// CapabilityKindDocument authorizes one media format as document input.
	CapabilityKindDocument CapabilityKind = "document"
	// CapabilityKindQuery authorizes one query input mode.
	CapabilityKindQuery CapabilityKind = "query"
	// CapabilityKindRequest authorizes a request shape rather than a format.
	CapabilityKindRequest CapabilityKind = "request"
)

// Capability identifiers, in manifest order.
const (
	CapabilityImageJPEG        = "image_jpeg"
	CapabilityImagePNG         = "image_png"
	CapabilityImageWebP        = "image_webp"
	CapabilityImageGIFStill    = "image_gif_still"
	CapabilityImageGIFAnimated = "image_gif_animated"
	CapabilityVideoMP4         = "video_mp4"
	CapabilityQueryText        = "query_text"
	CapabilityQueryImage       = "query_image"
	CapabilityInterleaved      = "interleaved_text_media"
	CapabilityBatchLimits      = "batch_limits"
)

// Capability describes one probe-tested provider capability.
type Capability struct {
	// ID is the stable capability identifier.
	ID string
	// Kind groups the capability.
	Kind CapabilityKind
	// Format is the media format for document and image-query capabilities.
	Format media.Format
	// Animated marks the animated-image capability.
	Animated bool
	// InputType is the provider input type used by the probe request.
	InputType string
	// Description explains what a passing probe demonstrates.
	Description string
}

var capabilities = []Capability{
	{ID: CapabilityImageJPEG, Kind: CapabilityKindDocument, Format: media.FormatJPEG, InputType: inputTypeDocument, Description: "a JPEG document embeds"},
	{ID: CapabilityImagePNG, Kind: CapabilityKindDocument, Format: media.FormatPNG, InputType: inputTypeDocument, Description: "a PNG document embeds"},
	{ID: CapabilityImageWebP, Kind: CapabilityKindDocument, Format: media.FormatWebP, InputType: inputTypeDocument, Description: "a WebP document embeds"},
	{ID: CapabilityImageGIFStill, Kind: CapabilityKindDocument, Format: media.FormatGIF, InputType: inputTypeDocument, Description: "a single-frame GIF document embeds"},
	{ID: CapabilityImageGIFAnimated, Kind: CapabilityKindDocument, Format: media.FormatGIF, Animated: true, InputType: inputTypeDocument, Description: "a multi-frame GIF embeds and differs from its first frame"},
	{ID: CapabilityVideoMP4, Kind: CapabilityKindDocument, Format: media.FormatMP4, InputType: inputTypeDocument, Description: "an MP4 document embeds"},
	{ID: CapabilityQueryText, Kind: CapabilityKindQuery, InputType: inputTypeQuery, Description: "a text query embeds and ranks its matching document first"},
	{ID: CapabilityQueryImage, Kind: CapabilityKindQuery, Format: media.FormatPNG, InputType: inputTypeQuery, Description: "an image query embeds and ranks its matching document first"},
	{ID: CapabilityInterleaved, Kind: CapabilityKindRequest, InputType: inputTypeDocument, Description: "a text-then-media document embeds and respects part order"},
	{ID: CapabilityBatchLimits, Kind: CapabilityKindRequest, InputType: inputTypeDocument, Description: "a batch at the policy limit embeds and preserves index order"},
}

// Capabilities returns every capability in manifest order.
func Capabilities() []Capability {
	out := make([]Capability, len(capabilities))
	copy(out, capabilities)
	return out
}

// CapabilityByID returns the capability with the given identifier.
func CapabilityByID(id string) (Capability, bool) {
	for _, capability := range capabilities {
		if capability.ID == id {
			return capability, true
		}
	}
	return Capability{}, false
}

// documentCapabilityFor returns the document capability that authorizes
// metadata, if any.
func documentCapabilityFor(metadata media.Metadata) (Capability, bool) {
	for _, capability := range capabilities {
		if capability.Kind != CapabilityKindDocument || capability.Format != metadata.Format {
			continue
		}
		if capability.Animated == metadata.Animated {
			return capability, true
		}
	}
	return Capability{}, false
}

// requestFingerprint identifies the exact provider request policy a
// capability was probed with.
func requestFingerprint(endpoint, model string, dimension int, capability Capability) string {
	return probecontract.Fingerprint(endpoint, model, dimension, capability.ID, capability.InputType)
}
