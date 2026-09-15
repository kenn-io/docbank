package loadfile

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLFPDoesNotSilentlyDropUnimplementedCommands(t *testing.T) {
	for _, command := range []string{"OF", "VF", "VN", "BF", "BR"} {
		require.False(t, lfpCommandSupported(command), command)
	}
	require.True(t, lfpCommandSupported("IM"))
	require.True(t, lfpCommandSupported("##"))
}

func TestLFPBoundaryFlagIsTheSecondIMParameter(t *testing.T) {
	p, err := ReadProfile("lfp-ipro-v1")
	require.NoError(t, err)
	raw, err := os.ReadFile("testdata/ab-package.lfp")
	require.NoError(t, err)
	images, diagnostics, err := ParseLFP(strings.NewReader(string(raw)), p)
	require.NoError(t, err)
	assert.Empty(t, diagnostics)
	require.Len(t, images, 3)
	assert.Equal(t, "document", images[0].Boundary)
	assert.True(t, images[0].DocumentBreak)
	assert.Empty(t, images[1].Boundary)
	assert.Equal(t, "child", images[2].Boundary)
	assert.True(t, images[2].DocumentBreak)
	assert.Equal(t, "IMAGES/001/EXT000003.tif", images[2].RelPath)
	assert.Equal(t, 90, images[2].Rotation)
	assert.Equal(t, 1, images[2].PageOrdinal)
	var out strings.Builder
	require.NoError(t, WriteLFP(&out, images, p))
	round, _, err := ParseLFP(strings.NewReader(out.String()), p)
	require.NoError(t, err)
	assert.Equal(t, images, round)
}

func TestLFPRefusesUnknownCommandAndUnrepresentableValue(t *testing.T) {
	p, err := ReadProfile("lfp-ipro-v1")
	require.NoError(t, err)
	for _, raw := range []string{
		"ZZ,EXT000001\n",
		"VN,VOL001\n",
		"IM,EXT000001,D,1,@VOL001;IMAGES;EXT000001.tif;2,0\n",
	} {
		_, _, err = ParseLFP(strings.NewReader(raw), p)
		require.ErrorIs(t, err, ErrUnrepresentable, raw)
	}
	err = WriteLFP(io.Discard, []ImageRef{{ImageKey: "EXT,0001", Volume: "VOL001", RelPath: "a.tif"}}, p)
	require.ErrorIs(t, err, ErrUnrepresentable)
	var rootFile strings.Builder
	require.NoError(t, WriteLFP(&rootFile, []ImageRef{{ImageKey: "EXT0001", Volume: "VOL001", RelPath: "a.tif", DocumentBreak: true, Boundary: "document", PageOrdinal: 1}}, p))
	parsed, _, err := ParseLFP(strings.NewReader(rootFile.String()), p)
	require.NoError(t, err)
	require.Len(t, parsed, 1)
	assert.Equal(t, "a.tif", parsed[0].RelPath)
}

func TestLFPBoundsFieldsAndRefusesModelStateItCannotEncode(t *testing.T) {
	p, err := ReadProfile("lfp-ipro-v1")
	require.NoError(t, err)
	tooLong := "IM," + strings.Repeat("x", MaxFieldValueBytes+1) + ",D,0,@VOL001;IMAGES;a.tif;2,0\n"
	_, _, err = ParseLFP(strings.NewReader(tooLong), p)
	require.ErrorIs(t, err, ErrMalformedInput)
	for _, image := range []ImageRef{
		{ImageKey: "A", Volume: "VOL001", RelPath: "a.tif", DocumentBreak: true, Boundary: "document", PageOrdinal: 1, FolderBreak: true},
		{ImageKey: "A", Volume: "VOL001", RelPath: "a.tif", DocumentBreak: true, Boundary: "document", PageOrdinal: 2},
		{ImageKey: "A", Volume: "VOL001", RelPath: "a.tif", Boundary: "child", PageOrdinal: 1},
		{ImageKey: "A", Volume: "VOL001", RelPath: "a.tif", Boundary: "document", PageOrdinal: 1},
		{ImageKey: strings.Repeat("x", MaxFieldValueBytes+1), Volume: "VOL001", RelPath: "a.tif", PageOrdinal: 1},
	} {
		require.ErrorIs(t, WriteLFP(io.Discard, []ImageRef{image}, p), ErrUnrepresentable)
	}
}
