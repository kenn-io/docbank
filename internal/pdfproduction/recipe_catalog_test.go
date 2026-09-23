package pdfproduction

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestQualifiedRecipeForDPIBindsConcreteDependencies(t *testing.T) {
	threeHundred, err := QualifiedRecipeForDPI(300)
	require.NoError(t, err)
	sixHundred, err := QualifiedRecipeForDPI(600)
	require.NoError(t, err)
	for _, recipe := range []struct {
		dpi int
		got string
	}{{threeHundred.DPI, threeHundred.RendererSHA256}, {sixHundred.DPI, sixHundred.RendererSHA256}} {
		require.NotEmpty(t, recipe.got)
	}
	require.Equal(t, wasmSHA256, threeHundred.RendererSHA256)
	require.Equal(t, fontSHA256, threeHundred.FontSHA256)
	require.Equal(t, writerVersion, threeHundred.WriterVersion)
	require.Equal(t, 300, threeHundred.DPI)
	require.Equal(t, 600, sixHundred.DPI)
	require.NotEqual(t, threeHundred, sixHundred)
	_, err = QualifiedRecipeForDPI(150)
	require.Error(t, err)
}
