package loadfile

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOPTProfilesDisagreeOnTheSameBytes(t *testing.T) {
	raw, err := os.ReadFile("testdata/ab-package.opt")
	require.NoError(t, err)

	standard, err := ReadProfile("opt-standard-v1")
	require.NoError(t, err)
	images, diagnostics, err := ParseOPT(bytes.NewReader(raw), standard)
	require.NoError(t, err)
	assert.Empty(t, diagnostics)
	require.Len(t, images, 3)
	assert.True(t, images[0].DocumentBreak)
	assert.Equal(t, 2, images[0].DeclaredPageCount)
	assert.Equal(t, 1, images[0].PageOrdinal)
	assert.Equal(t, 2, images[1].PageOrdinal)
	assert.Equal(t, 1, images[2].PageOrdinal)
	assert.Equal(t, "IMAGES/001/EXT000001.tif", images[0].RelPath)

	variant, err := ReadProfile("opt-pagecount5-v1")
	require.NoError(t, err)
	shifted, _, err := ParseOPT(bytes.NewReader(raw), variant)
	require.NoError(t, err)
	require.Len(t, shifted, 3)
	assert.Equal(t, 0, shifted[0].DeclaredPageCount)
}

func TestOPTUsesAllDeclaredColumnsAndOnlyNormalizesSeparators(t *testing.T) {
	p, err := ReadProfile("opt-standard-v1")
	require.NoError(t, err)
	raw := "IMG1,VOL9,..\\IMAGES\\.\\IMG1.tif,Y,Y,Y,004\n" +
		"IMG2,VOL9,C:\\evidence\\IMG2.tif,,,,\n"

	images, diagnostics, err := ParseOPT(strings.NewReader(raw), p)
	require.NoError(t, err)
	assert.Empty(t, diagnostics)
	require.Len(t, images, 2)
	assert.Equal(t, ImageRef{
		ImageKey: "IMG1", Volume: "VOL9", RelPath: "../IMAGES/./IMG1.tif",
		DocumentBreak: true, FolderBreak: true, BoxBreak: true,
		PageOrdinal: 1, DeclaredPageCount: 4,
	}, images[0])
	assert.Equal(t, "C:/evidence/IMG2.tif", images[1].RelPath)
	assert.Equal(t, 2, images[1].PageOrdinal)
}

func TestOPTDiagnosesMalformedRowsWithoutLosingFollowingPages(t *testing.T) {
	p, err := ReadProfile("opt-standard-v1")
	require.NoError(t, err)
	raw := "TOO,FEW,FIELDS\n" +
		"IMG1,VOL1,IMAGES\\IMG1.tif,Y,,,1\n"

	images, diagnostics, err := ParseOPT(strings.NewReader(raw), p)
	require.NoError(t, err)
	require.Len(t, diagnostics, 1)
	assert.Equal(t, "opt_field_count", diagnostics[0].Code)
	assert.Equal(t, "blocking", diagnostics[0].Severity)
	assert.Equal(t, 1, diagnostics[0].RowOrdinal)
	require.Len(t, images, 1)
	assert.Equal(t, 1, images[0].PageOrdinal)
}

func TestOPTPageCountRequiresUnsignedDecimalDigits(t *testing.T) {
	for _, count := range []string{"+5", "-0", "-5"} {
		t.Run(count, func(t *testing.T) {
			images, diagnostics, err := ParseOPT(strings.NewReader("I,V,P,Y,,,"+count+"\n"), mustProfile(t, "opt-standard-v1"))
			require.NoError(t, err)
			require.Len(t, images, 1)
			require.Len(t, diagnostics, 1)
			assert.Equal(t, "opt_page_count_invalid", diagnostics[0].Code)
		})
	}
}

func TestOPTCollectorStopsAtOnePage(t *testing.T) {
	images, _, err := ParseOPT(strings.NewReader(strings.Repeat("I,V,P,Y,,,1\n", MaxRowsPerPage+1)), mustProfile(t, "opt-standard-v1"))
	require.ErrorIs(t, err, ErrLoadfileLimit)
	assert.Len(t, images, MaxRowsPerPage)
}

func TestOPTCollectorCountsMalformedRowsTowardItsLimit(t *testing.T) {
	images, diagnostics, err := ParseOPT(strings.NewReader(strings.Repeat("malformed\n", MaxRowsPerPage+1)), mustProfile(t, "opt-standard-v1"))
	require.ErrorIs(t, err, ErrLoadfileLimit)
	assert.Empty(t, images)
	assert.Len(t, diagnostics, MaxRowsPerPage)
}

func TestOPTUsesTheStrictDeclaredEncoding(t *testing.T) {
	p, err := ReadProfile("opt-standard-v1")
	require.NoError(t, err)
	_, _, err = ParseOPT(bytes.NewReader([]byte{0xef, 0xbb, 0xbf, 'A', '\n'}), p)
	require.ErrorIs(t, err, ErrMalformedInput)
}

func TestOPTScannerBoundsFieldsAndStreamsBeyondOnePage(t *testing.T) {
	p := mustProfile(t, "opt-standard-v1")
	tooLong := "IMG1,VOL1," + strings.Repeat("x", MaxFieldValueBytes+1) + ",Y,,,1\n"
	_, _, err := ParseOPT(strings.NewReader(tooLong), p)
	require.ErrorIs(t, err, ErrMalformedInput)

	count := 0
	diagnostics, err := ScanOPT(t.Context(), strings.NewReader(strings.Repeat("I,V,P,,,,1\n", MaxRowsPerPage+1)), p, func(image ImageRef) error {
		count++
		assert.Equal(t, count, image.PageOrdinal)
		return nil
	})
	require.NoError(t, err)
	assert.Empty(t, diagnostics)
	assert.Equal(t, MaxRowsPerPage+1, count)

	stop := errors.New("stop scanning")
	count = 0
	_, err = ScanOPT(t.Context(), strings.NewReader("I,V,P,Y,,,1\nI,V,P,,,,1\n"), p, func(ImageRef) error {
		count++
		return stop
	})
	require.ErrorIs(t, err, stop)
	assert.Equal(t, 1, count)
}

func TestOPTScannerCancelsBeforeMalformedRows(t *testing.T) {
	profile := mustProfile(t, "opt-standard-v1")
	for _, afterValidRow := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		input := "malformed\nmalformed\n"
		if afterValidRow {
			input = "I,V,P,Y,,,1\n" + input
		} else {
			cancel()
		}
		diagnostics, err := ScanOPT(ctx, strings.NewReader(input), profile, func(ImageRef) error {
			cancel()
			return nil
		})
		cancel()
		require.ErrorIs(t, err, context.Canceled)
		assert.Empty(t, diagnostics, "malformed rows after cancellation must not be processed")
	}
}
