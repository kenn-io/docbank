package loadfile

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLFPBoundaryFlagIsTheSecondIMParameter(t *testing.T) {
	p, err := ReadProfile("lfp-ipro-v1")
	require.NoError(t, err)
	raw, err := os.ReadFile("testdata/ab-package.lfp")
	require.NoError(t, err)
	var images []ImageRef
	require.NoError(t, ScanLFP(t.Context(), strings.NewReader(string(raw)), p, func(image ImageRef) error {
		images = append(images, image)
		return nil
	}))
	require.Len(t, images, 3)
	assert.Equal(t, "document", images[0].Boundary)
	assert.True(t, images[0].DocumentBreak)
	assert.Empty(t, images[1].Boundary)
	assert.Equal(t, "child", images[2].Boundary)
	assert.True(t, images[2].DocumentBreak)
	assert.Equal(t, "IMAGES/001/EXT000003.tif", images[2].RelPath)
	assert.Equal(t, 90, images[2].Rotation)
	assert.Equal(t, 1, images[2].PageOrdinal)
}

func TestLFPRejectsUnsupportedCommandsOffsetsAndOversizeFields(t *testing.T) {
	p, err := ReadProfile("lfp-ipro-v1")
	require.NoError(t, err)
	for _, command := range []string{"OF", "VF", "VN", "BF", "BR", "ZZ"} {
		err := ScanLFP(t.Context(), strings.NewReader(command+",VOL001\n"), p, func(ImageRef) error { t.Fatal("unexpected image"); return nil })
		require.ErrorIs(t, err, ErrUnrepresentable, command)
	}
	err = ScanLFP(t.Context(), strings.NewReader("IM,EXT000001,D,1,@VOL001;IMAGES;EXT000001.tif;2,0\n"), p, nil)
	require.ErrorIs(t, err, ErrUnrepresentable)
	tooLong := "IM," + strings.Repeat("x", MaxFieldValueBytes+1) + ",D,0,@VOL001;IMAGES;a.tif;2,0\n"
	require.ErrorIs(t, ScanLFP(t.Context(), strings.NewReader(tooLong), p, nil), ErrMalformedInput)
}

func TestLFPCancelsCommentOnlyInput(t *testing.T) {
	profile, err := ReadProfile("lfp-ipro-v1")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, ScanLFP(ctx, strings.NewReader("## synthetic comment\n"), profile, nil), context.Canceled)
}
