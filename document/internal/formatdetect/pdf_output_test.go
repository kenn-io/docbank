package formatdetect

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPDFOutputRejectsOverflowBeforeRetainingIt(t *testing.T) {
	output := pdfOutput{limit: 5}
	n, err := output.Write([]byte("four"))
	require.NoError(t, err)
	require.Equal(t, 4, n)
	n, err = output.Write([]byte("more"))
	require.ErrorIs(t, err, ErrPDFExpandedBytes)
	require.Zero(t, n)
	require.Equal(t, "four", output.buffer.String())
}
