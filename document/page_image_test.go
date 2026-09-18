package document

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/canonical"
)

func TestPageRecipePreservesSharedIdentityVector(t *testing.T) {
	recipe := PageRecipeV1{Contract: PageImageContractV1, DPI: 150, Format: "png", RendererIdentity: PageRendererIdentity{Executable: "synthetic-renderer", Version: "1.0.0", Options: []string{"rgb", "crop-visible"}}}
	b, digest, err := MarshalPageRecipeV1(recipe)
	require.NoError(t, err)
	require.Equal(t, "6b04bdb236e58425d111fcd6a5422910032d52f4fe4d90236df616648dbe161b", digest)
	_, _, err = DecodePageRecipeV1(append([]byte(" "), b...))
	require.Error(t, err)
	recipe.DPI = 300
	_, other, err := MarshalPageRecipeV1(recipe)
	require.NoError(t, err)
	require.NotEqual(t, digest, other)
}

func TestPageRecipeDecodesRetainedRuntimeLimits(t *testing.T) {
	identity := PageRuntimeIdentity{
		InspectorSHA256: strings.Repeat("a", 64), RendererSHA256: strings.Repeat("b", 64), LimiterSHA256: strings.Repeat("c", 64),
		InspectorVersion: "synthetic-inspector", LimiterVersion: "synthetic-limiter", DeploymentIdentity: "synthetic-deployment",
		Platform: "linux/amd64", MemoryEnforcement: "rlimit-as", InspectorMemoryBytes: 1 << 30, RendererMemoryBytes: 256 << 20,
		MaxSourceBytes: 32 << 20, MaxPages: 500, MaxOutputBytes: 16 << 20, MaxPixels: 20_000_000, MaxAxis: 8192,
		MaxGeometryBytes: 8 << 20, MaxDiagnosticBytes: 32 << 10, PhaseSeconds: 30,
	}
	recipe := PageRecipeV1{Contract: PageImageContractV1, DPI: 144, Format: "png", RendererIdentity: PageRendererIdentity{Executable: "synthetic-renderer", Version: "1", Options: []string{"rgb"}, Runtime: &identity}}
	encoded, err := canonical.Marshal(recipe)
	require.NoError(t, err)
	retained, _, err := DecodePageRecipeV1(encoded)
	require.NoError(t, err, "saved runtime limits describe that render, independently of current launch policy")
	require.Equal(t, recipe, retained)
	for _, invalid := range []int64{0, -1, MaxPageInteger + 1} {
		identity.MaxDiagnosticBytes = invalid
		require.Error(t, ValidatePageRecipeV1(recipe))
	}
}

func TestPageImageReceiptRejectsChangedSourceFrameAndDimensions(t *testing.T) {
	frame, err := NewPDFPageFrame(pageSource(), 1, [4]float64{0, 0, 72, 144}, [4]float64{0, 0, 72, 144}, 0)
	require.NoError(t, err)
	_, frameHash, err := MarshalPageFrameV1(frame)
	require.NoError(t, err)
	recipe := PageRecipeV1{Contract: PageImageContractV1, DPI: 144, Format: "png", RendererIdentity: PageRendererIdentity{Executable: "synthetic-renderer", Version: "1.0.0", Options: []string{"rgb"}}}
	_, recipeHash, err := MarshalPageRecipeV1(recipe)
	require.NoError(t, err)
	image := PageImageV1{Contract: PageImageContractV1, Source: frame.Source, Page: 1, FrameSHA256: frameHash, RecipeSHA256: recipeHash, SHA256: frame.Source.SHA256, Size: 10, Width: 144, Height: 288}
	require.NoError(t, ValidatePageImageBinding(image, frame, recipe))
	image.Width += 2
	require.Error(t, ValidatePageImageBinding(image, frame, recipe))
	image.Width -= 2
	image.Source.Size++
	require.Error(t, ValidatePageImageBinding(image, frame, recipe))
	image.Source.Size--
	b, _, err := MarshalPageImageV1(image)
	require.NoError(t, err)
	_, _, err = DecodePageImageV1(bytes.Replace(b, []byte(`"page":1`), []byte(`"page":0`), 1))
	require.Error(t, err)
}

func TestPageImageBindingAllowsPDFQuantizationRoundingOnly(t *testing.T) {
	frame, err := NewPDFPageFrame(pageSource(), 1, [4]float64{0, 0, 72, 144}, [4]float64{0, 0, 72, 144}, 0)
	require.NoError(t, err)
	_, frameHash, err := MarshalPageFrameV1(frame)
	require.NoError(t, err)
	recipe := PageRecipeV1{Contract: PageImageContractV1, DPI: 144, Format: "png", RendererIdentity: PageRendererIdentity{Executable: "synthetic-renderer", Version: "1", Options: []string{"rgb"}}}
	_, recipeHash, err := MarshalPageRecipeV1(recipe)
	require.NoError(t, err)
	receipt := PageImageV1{Contract: PageImageContractV1, Source: frame.Source, Page: 1, FrameSHA256: frameHash, RecipeSHA256: recipeHash, SHA256: frame.Source.SHA256, Size: 10}
	for _, dimensions := range [][2]int64{{143, 287}, {144, 288}, {145, 289}} {
		receipt.Width, receipt.Height = dimensions[0], dimensions[1]
		require.NoError(t, ValidatePageImageBinding(receipt, frame, recipe))
	}
	receipt.Width = 146
	require.Error(t, ValidatePageImageBinding(receipt, frame, recipe))

	frame, err = NewPNGPageFrame(pageSource(), 254, 508, 10000, 10000)
	require.NoError(t, err)
	_, receipt.FrameSHA256, err = MarshalPageFrameV1(frame)
	require.NoError(t, err)
	recipe.DPI = 254
	_, receipt.RecipeSHA256, err = MarshalPageRecipeV1(recipe)
	require.NoError(t, err)
	receipt.Width, receipt.Height = 254, 508
	require.NoError(t, ValidatePageImageBinding(receipt, frame, recipe))
	receipt.Width++
	require.Error(t, ValidatePageImageBinding(receipt, frame, recipe))
}
