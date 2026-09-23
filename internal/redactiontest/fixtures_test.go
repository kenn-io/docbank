package redactiontest

import (
	"bytes"
	"testing"

	pdfapi "github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
)

func TestPDFProducesIndependentlyValidSyntheticDocument(t *testing.T) {
	encoded := PDF(t, []string{"Synthetic first line", "Synthetic second line"}, "Synthetic fixture subject")
	require.True(t, bytes.HasPrefix(encoded, []byte("%PDF-")))
	require.NoError(t, pdfapi.Validate(bytes.NewReader(encoded), nil))
}

func TestMapProducesOneASCIIAtomPerByte(t *testing.T) {
	value := Map("ABC")
	require.NoError(t, redaction.ValidateMap(value))
	require.Equal(t, int64(10_000), value.Pages[0].Height, "physical coordinates use 1/10,000 inch units")
	require.Equal(t, int64(3000), value.Pages[0].Width)
	require.Len(t, value.Atoms, 3)
	for index, atom := range value.Atoms {
		require.Equal(t, redaction.Span{Start: int64(index), End: int64(index + 1)}, atom.Span)
		require.Equal(t, int64(index*1000), atom.Boxes[0].X0)
		require.Equal(t, int64(index*1000+100), atom.Boxes[0].X1)
	}
}

func TestMapRejectsNonASCIIInput(t *testing.T) {
	require.Panics(t, func() { Map("é") })
}
