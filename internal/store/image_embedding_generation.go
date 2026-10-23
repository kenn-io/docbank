package store

import (
	"fmt"
	"strings"
)

// ImageEmbeddingGenerationID identifies the exact profile binding and preview
// used to produce an image embedding generation.
func ImageEmbeddingGenerationID(versionID, profileFingerprint, bindingID, previewChecksum string) string {
	return imageEmbeddingFingerprint("docbank/image-input-generation/v1", versionID, profileFingerprint, bindingID, previewChecksum)
}

// ImageEmbeddingPolicyFingerprint identifies the supported image input policy.
func ImageEmbeddingPolicyFingerprint() string {
	return imageEmbeddingFingerprint("image/grid/v1")
}

func imageEmbeddingFingerprint(values ...string) string {
	var identity strings.Builder
	for _, value := range values {
		fmt.Fprintf(&identity, "%d:%s", len(value), value)
	}
	return hashCatalogText(identity.String())
}
