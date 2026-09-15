package pdfstamp

import (
	"bytes"
	"fmt"
	"io"
	"testing"

	"github.com/go-pdf/fpdf"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
	"github.com/stretchr/testify/require"
)

func syntheticPDF(t *testing.T, pages int, size string) []byte {
	t.Helper()
	sizes := map[string]fpdf.SizeType{
		"Letter": {Wd: 612, Ht: 792},
		"A4":     {Wd: 595.28, Ht: 841.89},
	}
	pageSize, ok := sizes[size]
	require.True(t, ok, "unknown synthetic page size %q", size)
	return syntheticPages(t, make([]fpdf.SizeType, pages), func(index int) fpdf.SizeType {
		return pageSize
	})
}

func syntheticPages(t *testing.T, pages []fpdf.SizeType, sizeAt func(int) fpdf.SizeType) []byte {
	t.Helper()
	pdf := fpdf.NewCustom(&fpdf.InitType{UnitStr: "pt", Size: fpdf.SizeType{Wd: 612, Ht: 792}})
	for index := range pages {
		pdf.AddPageFormat("P", sizeAt(index))
	}
	var output bytes.Buffer
	require.NoError(t, pdf.Output(&output))
	return output.Bytes()
}

func syntheticRotated(t *testing.T, degrees int) []byte {
	t.Helper()
	var output bytes.Buffer
	err := api.Rotate(bytes.NewReader(syntheticPDF(t, 1, "Letter")), &output, degrees, nil, nil)
	require.NoError(t, err)
	return output.Bytes()
}

func syntheticCropped(t *testing.T) []byte {
	t.Helper()
	pdf := fpdf.NewCustom(&fpdf.InitType{UnitStr: "pt", Size: fpdf.SizeType{Wd: 612, Ht: 792}})
	pdf.AddPage()
	pdf.SetPageBox("crop", 36, 36, 540, 720)
	var output bytes.Buffer
	require.NoError(t, pdf.Output(&output))
	return output.Bytes()
}

func syntheticNarrowCropped(t *testing.T) []byte {
	t.Helper()
	pdf := fpdf.NewCustom(&fpdf.InitType{UnitStr: "pt", Size: fpdf.SizeType{Wd: 612, Ht: 792}})
	pdf.AddPage()
	pdf.SetPageBox("crop", 0, 0, 60, 60)
	var output bytes.Buffer
	require.NoError(t, pdf.Output(&output))
	return output.Bytes()
}

func syntheticMixedSize(t *testing.T) []byte {
	t.Helper()
	sizes := []fpdf.SizeType{{Wd: 595.28, Ht: 841.89}, {Wd: 612, Ht: 792}}
	return syntheticPages(t, sizes, func(index int) fpdf.SizeType { return sizes[index] })
}

func syntheticTinyPage(t *testing.T) []byte {
	t.Helper()
	size := fpdf.SizeType{Wd: 60, Ht: 60}
	return syntheticPages(t, []fpdf.SizeType{size}, func(int) fpdf.SizeType { return size })
}

func syntheticAlreadyStamped(t *testing.T) []byte {
	t.Helper()
	output, err := stampOne(t, syntheticPDF(t, 1, "Letter"), "OLD000001", "Helvetica")
	require.NoError(t, err)
	return output
}

func syntheticEncrypted(t *testing.T) []byte {
	t.Helper()
	pdf := fpdf.NewCustom(&fpdf.InitType{UnitStr: "pt", Size: fpdf.SizeType{Wd: 612, Ht: 792}})
	pdf.SetProtection(fpdf.CnProtectPrint, "synthetic-user", "synthetic-owner")
	pdf.AddPage()
	var output bytes.Buffer
	require.NoError(t, pdf.Output(&output))
	return output.Bytes()
}

func pageCount(t *testing.T, pdf []byte) int {
	t.Helper()
	count, err := api.PageCount(bytes.NewReader(pdf), nil)
	require.NoError(t, err)
	return count
}

func fontFor(fixture string) string {
	if fixture == "unsupported_font" {
		return "SyntheticFontWithoutMetrics"
	}
	return "Helvetica"
}

func stampOne(t *testing.T, pdf []byte, label, font string) ([]byte, error) {
	t.Helper()
	count, err := api.PageCount(bytes.NewReader(pdf), nil)
	if err != nil {
		return nil, fmt.Errorf("read source page count: %w", err)
	}
	labels := make([]string, count)
	for index := range labels {
		labels[index] = label
	}
	return stampPages(pdf, labels, font)
}

func stampWithLabels(t *testing.T, documents [][]byte, labels []string) []byte {
	t.Helper()
	stamped := make([]io.ReadSeeker, 0, len(documents))
	offset := 0
	for _, document := range documents {
		count := pageCount(t, document)
		require.LessOrEqual(t, offset+count, len(labels))
		output, err := stampPages(document, labels[offset:offset+count], "Helvetica")
		require.NoError(t, err)
		stamped = append(stamped, bytes.NewReader(output))
		offset += count
	}
	require.Equal(t, len(labels), offset)
	var merged bytes.Buffer
	require.NoError(t, api.MergeRaw(stamped, &merged, false, nil))
	return merged.Bytes()
}

func stampPages(pdf []byte, labels []string, font string) ([]byte, error) {
	watermarks := make(map[int]*model.Watermark, len(labels))
	for index, label := range labels {
		description := fmt.Sprintf("font:%s, points:9, pos:br, off:-24 24, scalefactor:1 abs, op:1, rot:0", font)
		watermark, err := api.TextWatermark(label, description, true, false, types.POINTS)
		if err != nil {
			return nil, fmt.Errorf("create watermark for page %d: %w", index+1, err)
		}
		watermarks[index+1] = watermark
	}
	var output bytes.Buffer
	if err := api.AddWatermarksMap(bytes.NewReader(pdf), &output, watermarks, nil); err != nil {
		return nil, fmt.Errorf("add watermarks: %w", err)
	}
	return output.Bytes(), nil
}
