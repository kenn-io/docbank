package production

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"

	pdfapi "github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/stretchr/testify/require"

	documentproduction "go.kenn.io/docbank/document/production"
)

func TestPrivilegeXLSXRoundTripUsesFrozenPublicRows(t *testing.T) {
	receipt, withheld, rows := privilegeExportFormatFixture(t, 5)

	exported, err := ExportPrivilegeLogXLSX(receipt, withheld, rows)
	require.NoError(t, err)
	require.Equal(t, PrivilegeLogXLSXMediaType, exported.MediaType)
	require.Equal(t, receipt.SHA256, exported.ReceiptSHA256)
	require.Equal(t, receipt.RowsSHA256, exported.RowsSHA256)
	requireExportContentDigest(t, exported)

	worksheet := readPrivilegeXLSXWorksheet(t, exported.Content)
	require.Equal(t, privilegeLogColumns, worksheet.values[0])
	ordered, err := documentproduction.OrderPrivilegeRows(withheld, rows)
	require.NoError(t, err)
	expected := publicPrivilegeRowsInOrder(ordered)
	require.Len(t, worksheet.values, len(expected)+1)
	for index, row := range expected {
		require.Equal(t, []string{row.ID, row.WithheldMemberID, strconv.FormatInt(row.FamilyOrder, 10), row.SourceVersionID, row.Basis, row.PublicDescription}, worksheet.values[index+1])
	}
	require.True(t, strings.HasPrefix(worksheet.values[1][5], "=2+2 is literal synthetic text"))
	require.Contains(t, worksheet.values[1][5], "literal _x0041_x0042_ token")
	require.Contains(t, worksheet.sourceXML, "_x005F_x0041_x005F_x0042_", "overlapping OOXML escape patterns must be neutralized")
	require.Empty(t, worksheet.formulas, "formula-like source text must not become an XLSX formula")
	require.NotContains(t, fmt.Sprint(worksheet.values), "PRIVATE-")

	reversed := slices.Clone(rows)
	slices.Reverse(reversed)
	reordered, err := ExportPrivilegeLogXLSX(receipt, withheld, reversed)
	require.NoError(t, err)
	require.Equal(t, exported.Content, reordered.Content)
	require.Equal(t, exported.ContentSHA256, reordered.ContentSHA256)

	repeated, err := ExportPrivilegeLogXLSX(receipt, withheld, rows)
	require.NoError(t, err)
	require.Equal(t, exported.Content, repeated.Content)

	if output := os.Getenv("DOCBANK_PRIVLOG_XLSX_OUTPUT"); output != "" {
		require.NoError(t, os.WriteFile(output, exported.Content, 0o600))
		t.Logf("wrote synthetic privilege-log XLSX to %s", output)
	}
}

func TestPrivilegeXLSXEncoderPreservesEmptyCells(t *testing.T) {
	content, err := encodePrivilegeXLSX([]documentproduction.PrivilegePublicRow{{
		ID: "80000000-0000-4000-8000-000000000001", WithheldMemberID: "40000000-0000-4000-8000-000000000001",
		FamilyOrder: 1, SourceVersionID: "50000000-0000-4000-8000-000000000001", Basis: "synthetic_basis",
	}})
	require.NoError(t, err)
	worksheet := readPrivilegeXLSXWorksheet(t, content)
	require.Len(t, worksheet.values, 2)
	require.Len(t, worksheet.values[1], len(privilegeLogColumns))
	require.Empty(t, worksheet.values[1][5])
	require.Empty(t, worksheet.formulas)
}

func TestPrivilegePDFIsSearchableMultipageAndDeterministic(t *testing.T) {
	receipt, withheld, rows := privilegeExportFormatFixture(t, 28)

	exported, err := ExportPrivilegeLogPDF(receipt, withheld, rows)
	require.NoError(t, err)
	require.Equal(t, PrivilegeLogPDFMediaType, exported.MediaType)
	require.Equal(t, receipt.SHA256, exported.ReceiptSHA256)
	require.Equal(t, receipt.RowsSHA256, exported.RowsSHA256)
	requireExportContentDigest(t, exported)
	require.NoError(t, pdfapi.Validate(bytes.NewReader(exported.Content), nil))
	require.NotContains(t, string(exported.Content), "PRIVATE-")

	text := extractPrivilegePDFText(t, exported.Content)
	require.Contains(t, text, "Résumé café Δ")
	require.Contains(t, text, "=2+2 is literal synthetic text")
	require.Contains(t, text, "ROW-28-END")
	require.GreaterOrEqual(t, strings.Count(text, "public_description"), 2, "each PDF page must repeat the column header")
	require.NotContains(t, text, "PRIVATE-")

	reversed := slices.Clone(rows)
	slices.Reverse(reversed)
	reordered, err := ExportPrivilegeLogPDF(receipt, withheld, reversed)
	require.NoError(t, err)
	require.Equal(t, exported.Content, reordered.Content)
	require.Equal(t, exported.ContentSHA256, reordered.ContentSHA256)

	repeated, err := ExportPrivilegeLogPDF(receipt, withheld, rows)
	require.NoError(t, err)
	require.Equal(t, exported.Content, repeated.Content)

	if output := os.Getenv("DOCBANK_PRIVLOG_PDF_OUTPUT"); output != "" {
		require.NoError(t, os.WriteFile(output, exported.Content, 0o600))
		t.Logf("wrote synthetic privilege-log PDF to %s", output)
	}
}

func TestPrivilegePDFEncoderAcceptsEmptyCells(t *testing.T) {
	content, err := encodePrivilegePDF([]documentproduction.PrivilegePublicRow{{
		ID: "80000000-0000-4000-8000-000000000001", WithheldMemberID: "40000000-0000-4000-8000-000000000001",
		FamilyOrder: 1, SourceVersionID: "50000000-0000-4000-8000-000000000001", Basis: "synthetic_basis",
	}})
	require.NoError(t, err)
	require.NoError(t, pdfapi.Validate(bytes.NewReader(content), nil))
	require.Contains(t, extractPrivilegePDFText(t, content), "synthetic_basis")
}

func TestPrivilegePDFEncoderRendersPermittedControlSeparators(t *testing.T) {
	content, err := encodePrivilegePDF([]documentproduction.PrivilegePublicRow{{
		ID: "80000000-0000-4000-8000-000000000001", WithheldMemberID: "40000000-0000-4000-8000-000000000001",
		FamilyOrder: 1, SourceVersionID: "50000000-0000-4000-8000-000000000001", Basis: "synthetic_basis",
		PublicDescription: "alpha\tbeta\rgamma",
	}})
	require.NoError(t, err)
	text := strings.Join(strings.Fields(extractPrivilegePDFText(t, content)), " ")
	require.Contains(t, text, "alpha beta gamma")
}

func privilegeExportFormatFixture(t *testing.T, count int) (documentproduction.PrivilegeLogReceipt, documentproduction.WithheldSelection, []documentproduction.PrivilegeRow) {
	t.Helper()
	policy, withheld, players, _ := internalPrivilegeFixture(t)
	withheld.Members = make([]documentproduction.WithheldMember, 0, count)
	rows := make([]documentproduction.PrivilegeRow, 0, count)
	for index := 1; index <= count; index++ {
		memberID := fmt.Sprintf("40000000-0000-4000-8000-%012x", index)
		versionID := fmt.Sprintf("50000000-0000-4000-8000-%012x", index)
		withheld.Members = append(withheld.Members, documentproduction.WithheldMember{
			ID: memberID, Ordinal: int64(index), SourceVersionID: versionID,
			SourceSHA256: fmt.Sprintf("%064x", index), SourceSize: int64(100 + index),
			FamilyOrder: 1, Family: standalonePrivilegeFamily(versionID),
		})
		description := fmt.Sprintf("Résumé café Δ synthetic discussion row %02d. %s ROW-%02d-END",
			index, strings.Repeat("Long wrapped public description with synthetic words only. ", 6), index)
		if index == 1 {
			description = "=2+2 is literal synthetic text; literal _x0041_x0042_ token; " + description
		}
		rows = append(rows, documentproduction.PrivilegeRow{
			ID: fmt.Sprintf("80000000-0000-4000-8000-%012x", index), WithheldMemberID: memberID,
			FamilyOrder: 1, SourceVersionID: versionID, Basis: "synthetic_basis", PublicDescription: description,
			PrivateRationale: "PRIVATE-RATIONALE-CANARY", EvidenceSHA256: fmt.Sprintf("%064x", count+index),
			PersonIDs: []string{players.Players[0].ID},
			Fields:    []documentproduction.PrivilegeField{{Name: "date", Value: "2026-09-22"}, {Name: "document_type", Value: "Synthetic message"}},
		})
	}
	withheld.SHA256 = canonicalWithheldDigest(t, withheld)
	receipt, err := documentproduction.FreezePrivilegeLog(documentproduction.PrivilegeLogFreezeInput{
		LogID: withheld.ID, Revision: withheld.Revision, WithheldSelectionSHA256: withheld.SHA256,
		PolicySHA256: policy.SHA256, PlayersSHA256: players.SHA256,
		ValidatedAt: "2026-09-22T03:00:00Z", Rows: rows,
		FrozenAt: "2026-09-22T03:00:01Z",
	})
	require.NoError(t, err)
	return receipt, withheld, rows
}

func requireExportContentDigest(t *testing.T, exported PrivilegeLogExport) {
	t.Helper()
	digest := sha256.Sum256(exported.Content)
	require.Equal(t, hex.EncodeToString(digest[:]), exported.ContentSHA256)
}

type privilegeXLSXWorksheet struct {
	values    [][]string
	formulas  []string
	sourceXML string
}

func readPrivilegeXLSXWorksheet(t *testing.T, content []byte) privilegeXLSXWorksheet {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	require.NoError(t, err)
	var sheet []byte
	for _, file := range reader.File {
		if file.Name != "xl/worksheets/sheet1.xml" {
			continue
		}
		stream, openErr := file.Open()
		require.NoError(t, openErr)
		sheet, err = io.ReadAll(stream)
		require.NoError(t, err)
		require.NoError(t, stream.Close())
	}
	require.NotEmpty(t, sheet)
	var parsed struct {
		Rows []struct {
			Cells []struct {
				Reference string `xml:"r,attr"`
				Type      string `xml:"t,attr"`
				Inline    struct {
					Text string `xml:"t"`
				} `xml:"is"`
				Formula string `xml:"f"`
			} `xml:"c"`
		} `xml:"sheetData>row"`
	}
	require.NoError(t, xml.Unmarshal(sheet, &parsed))
	result := privilegeXLSXWorksheet{values: make([][]string, 0, len(parsed.Rows)), sourceXML: string(sheet)}
	for _, row := range parsed.Rows {
		values := make([]string, 0, len(row.Cells))
		for _, cell := range row.Cells {
			require.Equal(t, "inlineStr", cell.Type, cell.Reference)
			values = append(values, decodeOOXMLString(cell.Inline.Text))
			if cell.Formula != "" {
				result.formulas = append(result.formulas, cell.Formula)
			}
		}
		result.values = append(result.values, values)
	}
	return result
}

func decodeOOXMLString(value string) string {
	var result strings.Builder
	for index := 0; index < len(value); {
		if index+7 <= len(value) && value[index] == '_' && value[index+1] == 'x' && value[index+6] == '_' {
			decoded, err := strconv.ParseUint(value[index+2:index+6], 16, 16)
			if err == nil {
				result.WriteRune(rune(decoded))
				index += 7
				continue
			}
		}
		result.WriteByte(value[index])
		index++
	}
	return result.String()
}

func extractPrivilegePDFText(t *testing.T, content []byte) string {
	t.Helper()
	executable, err := exec.LookPath("pdftotext")
	if err != nil {
		t.Skip("pdftotext is required for independent searchable-PDF verification")
	}
	command := exec.Command(executable, "-layout", "-", "-")
	command.Stdin = bytes.NewReader(content)
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	return string(output)
}
