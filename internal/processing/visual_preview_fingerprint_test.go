package processing

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image/color"
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
)

const (
	pinnedVisualPreviewProcessorFingerprint = "13e099735c888d186defdbd153acac419b2cf6598da349825adcc09598256993"
	pinnedVisualPreviewRecipeFingerprint    = "826b726be8a64e19501e7d01a0f32302c36aa873f63dddb844d96af3e586bf54"
)

func TestVisualPreviewProcessorFingerprintIsPinned(t *testing.T) {
	t.Parallel()
	assert.Equal(t, pinnedVisualPreviewProcessorFingerprint,
		CurrentVisualPreviewRecipe().ProcessorFingerprint)
}

func TestVisualPreviewRecipeFingerprintIsPinned(t *testing.T) {
	t.Parallel()
	_, fingerprint, err := document.MarshalVisualPreviewRecipeV1(CurrentVisualPreviewRecipe())
	require.NoError(t, err)
	assert.Equal(t, pinnedVisualPreviewRecipeFingerprint, fingerprint)
}

func TestProducedVisualPreviewCarriesCurrentRecipe(t *testing.T) {
	t.Parallel()
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
}

func TestVisualPreviewDescriptorTracksLinkedDependenciesAndPolicy(t *testing.T) {
	t.Parallel()
	info, ok := debug.ReadBuildInfo()
	require.True(t, ok)

	var xImageVersion string
	for _, dependency := range info.Deps {
		if dependency.Path == "golang.org/x/image" {
			xImageVersion = dependency.Version
			break
		}
	}
	require.NotEmpty(t, xImageVersion)
	descriptor := ":" + fmt.Sprintf(visualPreviewProcessorDescriptor,
		visualPreviewMaxEdgePixels, visualPreviewJPEGQuality) + ":"
	assert.Contains(t, descriptor, "+x-image-draw-"+xImageVersion+":")
	assert.Contains(t, descriptor, ":"+fmt.Sprintf("max-edge=%d", visualPreviewMaxEdgePixels)+":")
	assert.Contains(t, descriptor, ":"+fmt.Sprintf("quality=%d", visualPreviewJPEGQuality)+":")
}
