package report

import (
	"context"
	"encoding/csv"
	"errors"
	"io"
	"strconv"
	"unicode"
)

var csvHeader = []string{
	"Term #", "Terms", "Date Range", "Hits", "Hits Plus Family",
	"Unique Hits", "Unique Families", "Unique Hits Plus Family",
}

// WriteCSV renders the eight-column exchangeable report in input row order.
// Spreadsheet escaping changes only the rendered cell, never the frozen term.
func WriteCSV(ctx context.Context, output io.Writer, result Result) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(result.Counts) != len(result.Frame.Request.Terms) {
		return errors.New("report counts do not match term rows")
	}
	writer := csv.NewWriter(output)
	writer.UseCRLF = true
	if err := writer.Write(csvHeader); err != nil {
		return err
	}
	for index, term := range result.Frame.Request.Terms {
		if err := ctx.Err(); err != nil {
			return err
		}
		counts := result.Counts[index]
		row := []string{
			strconv.Itoa(term.Number), spreadsheetSafe(term.Expression),
			term.Dates.Start + " to " + term.Dates.End,
			strconv.FormatInt(counts.Hits, 10),
			strconv.FormatInt(counts.HitsPlusFamily, 10),
			strconv.FormatInt(counts.UniqueHits, 10),
			strconv.FormatInt(counts.UniqueFamilies, 10),
			strconv.FormatInt(counts.UniqueHitsPlusFamily, 10),
		}
		if err := writer.Write(row); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	writer.Flush()
	return writer.Error()
}

func spreadsheetSafe(cell string) string {
	for _, char := range cell {
		if char == '\t' || char == '\r' || char == '\n' {
			return "'" + cell
		}
		if unicode.IsSpace(char) {
			continue
		}
		if char == '=' || char == '+' || char == '-' || char == '@' {
			return "'" + cell
		}
		return cell
	}
	return cell
}
