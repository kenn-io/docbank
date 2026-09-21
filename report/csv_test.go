package report

import (
	"bytes"
	"context"
	"encoding/csv"
	"io"
	"strings"
	"testing"
)

func TestCSVWritesExactColumnsRowsAndCRLF(t *testing.T) {
	frame := oracleFrame()
	frame.Request.Terms[0].Number = 20
	frame.Request.Terms[0].Expression = "  =SUM(1,2)\n\"x\""
	frame.Request.Terms[1].Number = 3
	frame.Request.Terms[1].Expression = "合同, alpha"
	result := Result{Frame: frame, Counts: []Counts{{3, 6, 2, 2, 5}, {2, 4, 1, 0, 3}}}
	var output bytes.Buffer
	if err := WriteCSV(context.Background(), &output, result); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte("\r\n")) || bytes.Contains(output.Bytes(), []byte("\nTerm #")) {
		t.Fatalf("CSV did not use CRLF: %q", output.String())
	}
	reader := csv.NewReader(bytes.NewReader(output.Bytes()))
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	wantHeader := []string{"Term #", "Terms", "Date Range", "Hits", "Hits Plus Family", "Unique Hits", "Unique Families", "Unique Hits Plus Family"}
	if len(rows) != 3 || strings.Join(rows[0], "|") != strings.Join(wantHeader, "|") {
		t.Fatalf("CSV header/rows=%v", rows)
	}
	if rows[1][0] != "20" || rows[1][1] != "'  =SUM(1,2)\n\"x\"" || rows[1][2] != "2024-01-01 to 2024-12-31" || rows[1][6] != "2" {
		t.Fatalf("first row=%v", rows[1])
	}
	if rows[2][0] != "3" || rows[2][1] != "合同, alpha" || rows[2][7] != "3" {
		t.Fatalf("second row=%v", rows[2])
	}
	if result.Frame.Request.Terms[0].Expression != "  =SUM(1,2)\n\"x\"" {
		t.Fatal("CSV rendering rewrote the frozen expression")
	}
}

func TestCSVFormulaEscapingCoversWhitespaceAndControlPrefixes(t *testing.T) {
	for _, expression := range []string{"=1+1", "+1", "-1", "@SUM(1)", "\talpha", "\ralpha", "\nalpha", "  +1", " \talpha"} {
		t.Run(strings.ReplaceAll(expression, "\n", "newline"), func(t *testing.T) {
			frame := oracleFrame()
			frame.Request.Terms = frame.Request.Terms[:1]
			frame.Request.Terms[0].Expression = expression
			var output bytes.Buffer
			if err := WriteCSV(context.Background(), &output, Result{Frame: frame, Counts: []Counts{{}}}); err != nil {
				t.Fatal(err)
			}
			reader := csv.NewReader(bytes.NewReader(output.Bytes()))
			if _, err := reader.Read(); err != nil {
				t.Fatal(err)
			}
			row, err := reader.Read()
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(row[1], "'") {
				t.Fatalf("unsafe cell=%q", row[1])
			}
			// encoding/csv drops lone CR bytes when UseCRLF is enabled.
			if !strings.ContainsRune(expression, '\r') && row[1] != "'"+expression {
				t.Fatalf("cell changed beyond spreadsheet escaping: %q", row[1])
			}
		})
	}
}

func TestCSVStopsOnCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WriteCSV(ctx, io.Discard, Result{Frame: oracleFrame(), Counts: []Counts{{}, {}}}); err == nil {
		t.Fatal("wrote report after cancellation")
	}
}
