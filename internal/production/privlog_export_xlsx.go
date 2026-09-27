package production

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"time"

	documentproduction "go.kenn.io/docbank/document/production"
)

// ExportPrivilegeLogXLSX renders the exact frozen public projection as a
// deterministic workbook. All values use inline string cells, so source text
// beginning with a spreadsheet formula prefix remains text.
func ExportPrivilegeLogXLSX(receipt documentproduction.PrivilegeLogReceipt, withheld documentproduction.WithheldSelection, rows []documentproduction.PrivilegeRow) (PrivilegeLogExport, error) {
	ordered, err := verifyFrozenPrivilegeRows(receipt, withheld, rows)
	if err != nil {
		return PrivilegeLogExport{}, err
	}
	content, err := encodePrivilegeXLSX(publicPrivilegeRowsInOrder(ordered))
	if err != nil {
		return PrivilegeLogExport{}, err
	}
	return newPrivilegeLogExport(PrivilegeLogXLSXMediaType, receipt, content), nil
}

func encodePrivilegeXLSX(rows []documentproduction.PrivilegePublicRow) ([]byte, error) {
	worksheet, err := privilegeXLSXWorksheetXML(rows)
	if err != nil {
		return nil, err
	}
	parts := []struct {
		name    string
		content string
	}{
		{"[Content_Types].xml", privilegeXLSXContentTypes},
		{"_rels/.rels", privilegeXLSXRootRelationships},
		{"xl/workbook.xml", privilegeXLSXWorkbook},
		{"xl/_rels/workbook.xml.rels", privilegeXLSXWorkbookRelationships},
		{"xl/styles.xml", privilegeXLSXStyles},
		{"xl/worksheets/sheet1.xml", worksheet},
	}
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for _, part := range parts {
		header := &zip.FileHeader{
			Name: part.name, Method: zip.Deflate,
			Modified: time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC),
		}
		entry, createErr := writer.CreateHeader(header)
		if createErr != nil {
			return nil, fmt.Errorf("creating privilege XLSX part %s: %w", part.name, createErr)
		}
		if _, writeErr := io.WriteString(entry, part.content); writeErr != nil {
			return nil, fmt.Errorf("writing privilege XLSX part %s: %w", part.name, writeErr)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("closing privilege XLSX: %w", err)
	}
	return output.Bytes(), nil
}

func privilegeXLSXWorksheetXML(rows []documentproduction.PrivilegePublicRow) (string, error) {
	var output bytes.Buffer
	output.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	output.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`)
	output.WriteString(`<dimension ref="A1:F` + strconv.Itoa(len(rows)+1) + `"/>`)
	output.WriteString(`<sheetViews><sheetView workbookViewId="0"><pane ySplit="1" topLeftCell="A2" activePane="bottomLeft" state="frozen"/></sheetView></sheetViews>`)
	output.WriteString(`<sheetFormatPr defaultRowHeight="15"/>`)
	output.WriteString(`<cols><col min="1" max="2" width="39" customWidth="1"/><col min="3" max="3" width="14" customWidth="1"/><col min="4" max="4" width="39" customWidth="1"/><col min="5" max="5" width="22" customWidth="1"/><col min="6" max="6" width="72" customWidth="1"/></cols>`)
	output.WriteString(`<sheetData>`)
	if err := writePrivilegeXLSXRow(&output, 1, privilegeLogColumns, 2, 30); err != nil {
		return "", err
	}
	for index, row := range rows {
		values := privilegePublicRowValues(row)
		height := 30.0
		if len(row.PublicDescription) > 100 {
			height = 60
		}
		if err := writePrivilegeXLSXRow(&output, index+2, values, 1, height); err != nil {
			return "", err
		}
	}
	output.WriteString(`</sheetData><autoFilter ref="A1:F` + strconv.Itoa(len(rows)+1) + `"/>`)
	output.WriteString(`<pageMargins left="0.25" right="0.25" top="0.5" bottom="0.5" header="0.2" footer="0.2"/><pageSetup orientation="landscape" fitToWidth="1" fitToHeight="0"/>`)
	output.WriteString(`</worksheet>`)
	return output.String(), nil
}

func writePrivilegeXLSXRow(output *bytes.Buffer, rowNumber int, values []string, style int, height float64) error {
	output.WriteString(`<row r="` + strconv.Itoa(rowNumber) + `" ht="` + strconv.FormatFloat(height, 'f', -1, 64) + `" customHeight="1">`)
	for index, value := range values {
		column := string(rune('A' + index))
		output.WriteString(`<c r="` + column + strconv.Itoa(rowNumber) + `" s="` + strconv.Itoa(style) + `" t="inlineStr"><is><t xml:space="preserve">`)
		if err := xml.EscapeText(output, []byte(neutralizePrivilegeXLSXEscapes(value))); err != nil {
			return fmt.Errorf("encoding privilege XLSX cell %s%d: %w", column, rowNumber, err)
		}
		output.WriteString(`</t></is></c>`)
	}
	output.WriteString(`</row>`)
	return nil
}

func neutralizePrivilegeXLSXEscapes(value string) string {
	var output bytes.Buffer
	for index := 0; index < len(value); {
		if index+7 <= len(value) && value[index] == '_' && (value[index+1] == 'x' || value[index+1] == 'X') &&
			value[index+6] == '_' && privilegeXLSXHex(value[index+2:index+6]) {
			output.WriteString("_x005F_")
			index++
			continue
		}
		output.WriteByte(value[index])
		index++
	}
	return output.String()
}

func privilegeXLSXHex(value string) bool {
	for index := range len(value) {
		character := value[index]
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') && (character < 'A' || character > 'F') {
			return false
		}
	}
	return true
}

func privilegePublicRowValues(row documentproduction.PrivilegePublicRow) []string {
	return []string{row.ID, row.WithheldMemberID, strconv.FormatInt(row.FamilyOrder, 10), row.SourceVersionID, row.Basis, row.PublicDescription}
}

const privilegeXLSXContentTypes = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/><Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/><Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/></Types>`

const privilegeXLSXRootRelationships = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`

const privilegeXLSXWorkbook = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Privilege Log" sheetId="1" r:id="rId1"/></sheets><definedNames><definedName name="_xlnm.Print_Titles" localSheetId="0">'Privilege Log'!$1:$1</definedName></definedNames><calcPr calcId="0" fullCalcOnLoad="0"/></workbook>`

const privilegeXLSXWorkbookRelationships = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/><Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/></Relationships>`

const privilegeXLSXStyles = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><fonts count="2"><font><sz val="10"/><name val="Arial"/></font><font><b/><sz val="10"/><name val="Arial"/></font></fonts><fills count="3"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill><fill><patternFill patternType="solid"><fgColor rgb="FFD9EAF7"/><bgColor indexed="64"/></patternFill></fill></fills><borders count="1"><border><left/><right/><top/><bottom/><diagonal/></border></borders><cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs><cellXfs count="3"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0" applyAlignment="1"><alignment vertical="top" wrapText="1"/></xf><xf numFmtId="0" fontId="1" fillId="2" borderId="0" xfId="0" applyFill="1" applyFont="1" applyAlignment="1"><alignment vertical="center" wrapText="1"/></xf></cellXfs><cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles></styleSheet>`
