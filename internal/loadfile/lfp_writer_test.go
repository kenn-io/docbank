package loadfile

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLFPWriterMatchesIndependentlyAuthoredImageMap(t *testing.T) {
	profile, err := ReadProfile("lfp-ipro-v1")
	require.NoError(t, err)
	fixture, err := os.ReadFile("testdata/ab-package.lfp")
	require.NoError(t, err)
	var images []ImageRef
	require.NoError(t, ScanLFP(t.Context(), bytes.NewReader(fixture), profile, func(image ImageRef) error {
		images = append(images, image)
		return nil
	}))
	var output bytes.Buffer
	require.NoError(t, WriteLFP(&output, images, profile))
	assert.Equal(t, "IM,EXT000001,D,0,@VOL001;IMAGES\\001;EXT000001.tif;2,0\r\n"+
		"IM,EXT000002,,0,@VOL001;IMAGES\\001;EXT000002.tif;2,0\r\n"+
		"IM,EXT000003,C,0,@VOL001;IMAGES\\001;EXT000003.tif;2,90\r\n", output.String())
	var round []ImageRef
	require.NoError(t, ScanLFP(t.Context(), &output, profile, func(image ImageRef) error {
		round = append(round, image)
		return nil
	}))
	assert.Equal(t, images, round)
}

func TestLFPWriterRefusesAmbiguousAndInconsistentValues(t *testing.T) {
	profile, err := ReadProfile("lfp-ipro-v1")
	require.NoError(t, err)
	valid := ImageRef{ImageKey: "A1", Volume: "VOL001", RelPath: "IMAGES/A1.tif", DocumentBreak: true, Boundary: "document", PageOrdinal: 1}
	for _, change := range []func(*ImageRef){
		func(image *ImageRef) { image.ImageKey = "A,1" },
		func(image *ImageRef) { image.Volume = "VOL;001" },
		func(image *ImageRef) { image.RelPath = "IMAGES/A;1.tif" },
		func(image *ImageRef) { image.RelPath = "IMAGES\\A1.tif" },
		func(image *ImageRef) { image.RelPath = "../A1.tif" },
		func(image *ImageRef) { image.Rotation = 45 },
		func(image *ImageRef) { image.Boundary = "" },
		func(image *ImageRef) { image.PageOrdinal = 2 },
	} {
		image := valid
		change(&image)
		var output bytes.Buffer
		require.ErrorIs(t, WriteLFP(&output, []ImageRef{image}, profile), ErrUnrepresentable, "%+v", image)
		assert.Empty(t, output.String(), "invalid model cannot leave partial output")
	}
	valid.ImageKey = strings.Repeat("A", MaxFieldValueBytes+1)
	require.ErrorIs(t, WriteLFP(&bytes.Buffer{}, []ImageRef{valid}, profile), ErrUnrepresentable)
}
