package processing

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image/color"
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
)

const (
	pinnedVisualPreviewProcessorFingerprint = "6391e667d07b0aab1622d2b4167ffe66496fe7d336beb603401d96cd8a11a641"
	pinnedVisualPreviewRecipeFingerprint    = "03f89b744cc013004c89b1babe6d779ee043652519c586453bcde761bd7a4b04"
)

func TestVisualPreviewProcessorFingerprintIsPinned(t *testing.T) {
	assert.Equal(t, pinnedVisualPreviewProcessorFingerprint,
		CurrentVisualPreviewRecipe().ProcessorFingerprint)
}

func TestVisualPreviewRecipeFingerprintIsPinned(t *testing.T) {
	_, fingerprint, err := document.MarshalVisualPreviewRecipeV1(CurrentVisualPreviewRecipe())
	require.NoError(t, err)
	assert.Equal(t, pinnedVisualPreviewRecipeFingerprint, fingerprint)
}

func TestProducedVisualPreviewCarriesPinnedRecipe(t *testing.T) {
	source := mediatest.JPEG(3, 2, color.White)
	digest := sha256.Sum256(source)

	product, err := ProduceVisualPreview(t.Context(), bytes.NewReader(source), VisualPreviewTarget{
		SourceSHA256: hex.EncodeToString(digest[:]),
		Size:         int64(len(source)),
		MediaType:    "image/jpeg",
	})
	require.NoError(t, err)
	assert.Equal(t, document.VisualPreviewReady, product.Preview.State)
	assert.Equal(t, CurrentVisualPreviewRecipe(), product.Preview.Recipe)
	assert.Equal(t, pinnedVisualPreviewProcessorFingerprint,
		product.Preview.Recipe.ProcessorFingerprint)
	_, fingerprint, err := document.MarshalVisualPreviewRecipeV1(product.Preview.Recipe)
	require.NoError(t, err)
	assert.Equal(t, pinnedVisualPreviewRecipeFingerprint, fingerprint)
}

func TestVisualPreviewDescriptorTracksLinkedDependenciesAndPolicy(t *testing.T) {
	info, ok := debug.ReadBuildInfo()
	require.True(t, ok)

	var xImageVersion string
	for _, dependency := range info.Deps {
		if dependency.Path == "golang.org/x/image" {
			xImageVersion = dependency.Version
			break
		}
	}
	assert.Equal(t, "v0.44.0", xImageVersion)
	assert.Contains(t, visualPreviewProcessorDescriptor, "max-edge=4096")
	assert.Contains(t, visualPreviewProcessorDescriptor, "quality=90")
}
