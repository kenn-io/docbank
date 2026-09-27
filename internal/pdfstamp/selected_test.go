package pdfstamp

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"github.com/go-pdf/fpdf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/packagetest"
)

func syntheticNumberedPDF(t *testing.T) []byte {
	t.Helper()
	pdf := fpdf.NewCustom(&fpdf.InitType{UnitStr: "pt", Size: fpdf.SizeType{Wd: 612, Ht: 792}})
	pdf.SetFont("Helvetica", "", 16)
	for page := 1; page <= 8; page++ {
		pdf.AddPage()
		pdf.SetXY(72, 72)
		pdf.Cell(250, 25, fmt.Sprintf("SOURCE PAGE %02d", page))
	}
	var output bytes.Buffer
	require.NoError(t, pdf.Output(&output))
	return output.Bytes()
}

func TestStampSelectedKeepsExactSourcePageOrderAndVisibleContent(t *testing.T) {
	requirePopplerQualification(t)
	source := syntheticNumberedPDF(t)
	before := sha256.Sum256(source)
	var output bytes.Buffer

	result, err := StampSelected(t.Context(), bytes.NewReader(source), []PageLabel{
		{SourcePage: 3, Label: "OUR000041"},
		{SourcePage: 6, Label: "OUR000042"},
	}, validRecipe(t), &output)

	require.NoError(t, err)
	assert.Equal(t, 2, result.PageCount)
	assert.Equal(t, []SourceOutputPage{{SourcePage: 3, OutputPage: 1}, {SourcePage: 6, OutputPage: 2}}, result.PageMap)
	assert.Equal(t, int64(output.Len()), result.Size)
	assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256(output.Bytes())), result.SHA256)
	assert.Equal(t, before, sha256.Sum256(source), "source bytes must remain unchanged")
	assert.Equal(t, 2, pageCount(t, output.Bytes()))
	visible, err := packagetest.PDFText(t.Context(), output.Bytes())
	require.NoError(t, err)
	parts := strings.Split(strings.TrimSpace(visible), "\f")
	require.Len(t, parts, 2)
	assert.Contains(t, parts[0], "SOURCE PAGE 03")
	assert.Contains(t, parts[0], "OUR000041")
	assert.Contains(t, parts[1], "SOURCE PAGE 06")
	assert.Contains(t, parts[1], "OUR000042")
	for _, other := range []string{"SOURCE PAGE 01", "SOURCE PAGE 02", "SOURCE PAGE 04", "SOURCE PAGE 05", "SOURCE PAGE 07", "SOURCE PAGE 08"} {
		assert.NotContains(t, visible, other)
	}
}

func TestSelectPagesRejectsInvalidDuplicateAndOutOfRangeSelections(t *testing.T) {
	source := syntheticNumberedPDF(t)
	for name, pages := range map[string][]int{
		"empty":        nil,
		"zero":         {0},
		"negative":     {-1},
		"duplicate":    {3, 3},
		"out of range": {3, 9},
	} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			_, err := SelectPages(t.Context(), bytes.NewReader(source), pages, &output)
			require.ErrorIs(t, err, ErrStampEngineFailure)
			assert.Zero(t, output.Len())
		})
	}
}

func TestSelectPagesPreservesCallerOrderAndHashesVerifiedBytes(t *testing.T) {
	requirePopplerQualification(t)
	source := syntheticNumberedPDF(t)
	var output bytes.Buffer
	result, err := SelectPages(t.Context(), bytes.NewReader(source), []int{6, 3}, &output)
	require.NoError(t, err)
	assert.Equal(t, []SourceOutputPage{{SourcePage: 6, OutputPage: 1}, {SourcePage: 3, OutputPage: 2}}, result.PageMap)
	assert.Equal(t, 2, result.PageCount)
	assert.Equal(t, int64(output.Len()), result.Size)
	assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256(output.Bytes())), result.SHA256)
	visible, err := packagetest.PDFText(t.Context(), output.Bytes())
	require.NoError(t, err)
	parts := strings.Split(strings.TrimSpace(visible), "\f")
	require.Len(t, parts, 2)
	assert.Contains(t, parts[0], "SOURCE PAGE 06")
	assert.Contains(t, parts[1], "SOURCE PAGE 03")
}

func TestStampSelectedRejectsNonConsecutiveLabelsBeforeWriting(t *testing.T) {
	var output bytes.Buffer
	_, err := StampSelected(t.Context(), bytes.NewReader(syntheticNumberedPDF(t)), []PageLabel{
		{SourcePage: 3, Label: "OUR000041"},
		{SourcePage: 6, Label: "OUR000043"},
	}, validRecipe(t), &output)
	require.ErrorIs(t, err, ErrStampEngineFailure)
	assert.Zero(t, output.Len())
}

func TestVerifyStampedReaderChecksSeekableBytes(t *testing.T) {
	var stamped bytes.Buffer
	labels := []PageLabel{{SourcePage: 1, Label: "OUR000041"}}
	_, err := StampSelected(t.Context(), bytes.NewReader(syntheticNumberedPDF(t)), labels, validRecipe(t), &stamped)
	require.NoError(t, err)
	result, err := VerifyStampedReader(t.Context(), bytes.NewReader(stamped.Bytes()), labels)
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("%x", sha256.Sum256(stamped.Bytes())), result.SHA256)
	require.Equal(t, int64(stamped.Len()), result.Size)
}
