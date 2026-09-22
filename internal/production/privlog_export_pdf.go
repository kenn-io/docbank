package production

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"

	documentproduction "go.kenn.io/docbank/document/production"
)

const (
	privilegePDFMargin     = 24.0
	privilegePDFFontSize   = 6.5
	privilegePDFLineHeight = 9.0
	privilegePDFPadding    = 2.0
	privilegePDFBottom     = 588.0
)

var privilegePDFColumnWidths = []float64{105, 105, 52, 105, 78, 299}

// ExportPrivilegeLogPDF renders the exact frozen public projection as a
// deterministic searchable PDF with repeated headers and wrapped cells.
func ExportPrivilegeLogPDF(receipt documentproduction.PrivilegeLogReceipt, withheld documentproduction.WithheldSelection, rows []documentproduction.PrivilegeRow) (PrivilegeLogExport, error) {
	ordered, err := verifyFrozenPrivilegeRows(receipt, withheld, rows)
	if err != nil {
		return PrivilegeLogExport{}, err
	}
	content, err := encodePrivilegePDF(publicPrivilegeRowsInOrder(ordered))
	if err != nil {
		return PrivilegeLogExport{}, err
	}
	return newPrivilegeLogExport(PrivilegeLogPDFMediaType, receipt, content), nil
}

func encodePrivilegePDF(rows []documentproduction.PrivilegePublicRow) ([]byte, error) {
	pdf := fpdf.New("L", "pt", "Letter", "")
	pdf.SetCatalogSort(true)
	pdf.SetCompression(true)
	pdf.SetCreationDate(time.Unix(0, 0).UTC())
	pdf.SetModificationDate(time.Unix(0, 0).UTC())
	pdf.SetMargins(privilegePDFMargin, privilegePDFMargin, privilegePDFMargin)
	pdf.SetAutoPageBreak(false, 0)
	pdf.AddUTF8FontFromBytes("Go", "", goregular.TTF)
	pdf.AddUTF8FontFromBytes("Go", "B", gobold.TTF)
	if err := pdf.Error(); err != nil {
		return nil, fmt.Errorf("initializing privilege PDF: %w", err)
	}

	y := addPrivilegePDFPage(pdf)
	for _, row := range rows {
		lineSets := splitPrivilegePDFCells(pdf, privilegePublicRowValues(row), privilegePDFFontSize, false)
		lineOffset := 0
		lineCount := maximumPrivilegePDFLines(lineSets)
		for lineOffset < lineCount {
			availableLines := int(math.Floor((privilegePDFBottom - y - 2*privilegePDFPadding) / privilegePDFLineHeight))
			if availableLines < 1 {
				y = addPrivilegePDFPage(pdf)
				continue
			}
			segmentLines := min(availableLines, lineCount-lineOffset)
			height := float64(segmentLines)*privilegePDFLineHeight + 2*privilegePDFPadding
			drawPrivilegePDFRow(pdf, lineSets, lineOffset, segmentLines, y, height, false)
			y += height
			lineOffset += segmentLines
			if lineOffset < lineCount {
				y = addPrivilegePDFPage(pdf)
			}
		}
	}

	var output bytes.Buffer
	if err := pdf.Output(&output); err != nil {
		return nil, fmt.Errorf("writing privilege PDF: %w", err)
	}
	return output.Bytes(), nil
}

func addPrivilegePDFPage(pdf *fpdf.Fpdf) float64 {
	pdf.AddPage()
	lineSets := splitPrivilegePDFCells(pdf, privilegeLogColumns, 7, true)
	height := float64(maximumPrivilegePDFLines(lineSets))*privilegePDFLineHeight + 2*privilegePDFPadding
	drawPrivilegePDFRow(pdf, lineSets, 0, maximumPrivilegePDFLines(lineSets), privilegePDFMargin, height, true)
	return privilegePDFMargin + height
}

func splitPrivilegePDFCells(pdf *fpdf.Fpdf, values []string, size float64, bold bool) [][]string {
	style := ""
	if bold {
		style = "B"
	}
	pdf.SetFont("Go", style, size)
	result := make([][]string, len(values))
	for index, value := range values {
		value = strings.NewReplacer("\r\n", "\n", "\r", " ", "\t", " ").Replace(value)
		result[index] = pdf.SplitText(value, privilegePDFColumnWidths[index]-2*privilegePDFPadding)
		if len(result[index]) == 0 {
			result[index] = []string{""}
		}
	}
	return result
}

func maximumPrivilegePDFLines(lineSets [][]string) int {
	maximum := 1
	for _, lines := range lineSets {
		maximum = max(maximum, len(lines))
	}
	return maximum
}

func drawPrivilegePDFRow(pdf *fpdf.Fpdf, lineSets [][]string, lineOffset, lineCount int, y, height float64, header bool) {
	style := ""
	fontSize := privilegePDFFontSize
	fillStyle := "D"
	if header {
		style = "B"
		fontSize = 7
		fillStyle = "DF"
		pdf.SetFillColor(217, 234, 247)
	}
	pdf.SetFont("Go", style, fontSize)
	pdf.SetDrawColor(110, 120, 130)
	pdf.SetTextColor(20, 25, 30)
	x := privilegePDFMargin
	for column, width := range privilegePDFColumnWidths {
		pdf.Rect(x, y, width, height, fillStyle)
		for index := range lineCount {
			lineIndex := lineOffset + index
			if lineIndex >= len(lineSets[column]) {
				continue
			}
			baseline := y + privilegePDFPadding + fontSize + float64(index)*privilegePDFLineHeight
			pdf.Text(x+privilegePDFPadding, baseline, lineSets[column][lineIndex])
		}
		x += width
	}
}
