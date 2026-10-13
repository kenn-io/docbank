package main

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestParsePhotoFaceBox(t *testing.T) {
	t.Parallel()
	box, err := parsePhotoFaceBox("0.1, 0.2,0.3,0.4")
	require.NoError(t, err)
	require.Equal(t, &document.FaceBox{X: 0.1, Y: 0.2, Width: 0.3, Height: 0.4}, box)
	box, err = parsePhotoFaceBox("")
	require.NoError(t, err)
	require.Nil(t, box)
	for _, value := range []string{"1,2,3", "a,0,0.1,0.1", "NaN,0,0.1,0.1", "0,0,0,0.1", "0.9,0,0.2,0.1", "-0.1,0,0.2,0.1"} {
		_, err := parsePhotoFaceBox(value)
		require.Error(t, err, value)
	}
}
