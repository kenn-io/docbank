package document

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
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
	image.Width++
	require.Error(t, ValidatePageImageBinding(image, frame, recipe))
	image.Width--
	image.Source.Size++
	require.Error(t, ValidatePageImageBinding(image, frame, recipe))
	image.Source.Size--
	b, _, err := MarshalPageImageV1(image)
	require.NoError(t, err)
	_, _, err = DecodePageImageV1(bytes.Replace(b, []byte(`"page":1`), []byte(`"page":0`), 1))
	require.Error(t, err)
}
