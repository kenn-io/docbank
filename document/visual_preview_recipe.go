package document

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

const (
	// VisualPreviewMaxEdgePixels bounds the large built-in preview.
	VisualPreviewMaxEdgePixels = 4096
	// VisualPreviewJPEGQuality is the encoder quality for every built-in preview.
	VisualPreviewJPEGQuality = 90
	// VisualPreviewProcessorDescriptor names every byte-producing choice. Bump
	// its revision when any of them changes.
	VisualPreviewProcessorDescriptor = "docbank-visual-preview:jpeg+png+gif-stdlib+webp+embedded-camera-raw+" +
		"x-image-draw-v0.44.0:max-edge=%d:quality=%d:alpha=white:v7"
)

// BuiltInVisualPreviewRecipe returns a canonical built-in size recipe.
func BuiltInVisualPreviewRecipe(size string) (VisualPreviewRecipeV1, error) {
	recipe := VisualPreviewRecipeV1{ContractVersion: VisualPreviewContractV1, MaxEdgePixels: VisualPreviewMaxEdgePixels, OutputMediaType: "image/jpeg", OrientationPolicy: "apply", ColorPolicy: "srgb", FramePolicy: "primary"}
	switch size {
	case "grid":
		recipe.MaxEdgePixels = 512
	case "fit":
		recipe.MaxEdgePixels = 2560
	case "large":
	default:
		return VisualPreviewRecipeV1{}, fmt.Errorf("unknown visual preview size %q", size)
	}
	descriptor := fmt.Sprintf(VisualPreviewProcessorDescriptor, recipe.MaxEdgePixels, VisualPreviewJPEGQuality)
	recipe.ProcessorFingerprint = previewProcessorFingerprint(descriptor)
	return recipe, nil
}

func previewProcessorFingerprint(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
