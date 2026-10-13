package main

import (
	"testing"

	"github.com/google/uuid"
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

func TestPhotoFaceCreateRequest(t *testing.T) {
	t.Parallel()
	box := &document.FaceBox{Width: 0.2, Height: 0.2}
	request, err := photoFaceCreateRequest(box, "", "")
	require.NoError(t, err)
	id, err := uuid.Parse(request.FaceID)
	require.NoError(t, err)
	require.Equal(t, uuid.Version(4), id.Version())
	retry, err := photoFaceCreateRequest(box, "", request.FaceID)
	require.NoError(t, err)
	require.Equal(t, request, retry)
	_, err = photoFaceCreateRequest(nil, "", request.FaceID)
	require.Error(t, err)
	_, err = photoFaceCreateRequest(box, "", "invalid")
	require.Error(t, err)
}
