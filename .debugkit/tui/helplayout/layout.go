package helplayout

import "github.com/mattn/go-runewidth"

// HelpItem is one unstyled key-and-description entry in a help display.
// Description may be empty.
type HelpItem struct {
	Key         string
	Description string
}

// ColumnWidths returns the widest terminal-cell width for every column in
// rows. Empty rows do not add columns.
//
// Callers rendering an aligned table from ReflowRows output should use these
// widths with ItemWidth to calculate cell padding rather than independently
// measuring item text.
func ColumnWidths(rows [][]HelpItem) []int {
	maxColumns := 0
	for _, row := range rows {
		maxColumns = max(maxColumns, len(row))
	}
	if maxColumns == 0 {
		return nil
	}

	widths := make([]int, maxColumns)
	for _, row := range rows {
		for column, item := range row {
			widths[column] = max(widths[column], ItemWidth(item))
		}
	}
	return widths
}

// ReflowRows returns rows using the greatest aligned column count that fits
// within availableWidth.
//
// An item occupies the terminal-cell width of Key. A non-empty Description
// adds one separating cell followed by its terminal-cell width. columnGap is
// the complete visible width inserted before every column after the first; a
// negative value is treated as zero. It excludes alignment fill used to pad a
// shorter item to the width returned by ColumnWidths.
//
// Items retain their input order, and chunks from one source row are never
// combined with another source row. Empty source rows are preserved. If
// availableWidth is nonpositive, the source grouping is preserved. An item
// wider than availableWidth is not truncated or split. Because every returned
// row shares one aligned column grid, an overwide item forces a single-column
// result in which every item occupies its own row.
func ReflowRows(rows [][]HelpItem, availableWidth, columnGap int) [][]HelpItem {
	if availableWidth <= 0 {
		return cloneRows(rows)
	}
	columnGap = max(columnGap, 0)

	maxItemsPerRow := 0
	for _, row := range rows {
		maxItemsPerRow = max(maxItemsPerRow, len(row))
	}
	if maxItemsPerRow == 0 {
		return cloneRows(rows)
	}

	for columns := maxItemsPerRow; columns >= 1; columns-- {
		candidate := chunkRows(rows, columns)
		columnWidths := ColumnWidths(candidate)

		totalWidth := columnGap * (len(columnWidths) - 1)
		for _, width := range columnWidths {
			totalWidth += width
		}
		if totalWidth <= availableWidth {
			return candidate
		}
	}

	return chunkRows(rows, 1)
}

// ItemWidth returns the terminal-cell width occupied by item before column
// alignment or inter-column gap is applied.
func ItemWidth(item HelpItem) int {
	width := runewidth.StringWidth(item.Key)
	if item.Description != "" {
		width += 1 + runewidth.StringWidth(item.Description)
	}
	return width
}

func chunkRows(rows [][]HelpItem, columns int) [][]HelpItem {
	var result [][]HelpItem
	for _, row := range rows {
		if len(row) == 0 {
			result = append(result, nil)
			continue
		}
		for start := 0; start < len(row); start += columns {
			end := min(start+columns, len(row))
			result = append(result, append([]HelpItem(nil), row[start:end]...))
		}
	}
	return result
}

func cloneRows(rows [][]HelpItem) [][]HelpItem {
	if rows == nil {
		return nil
	}
	result := make([][]HelpItem, len(rows))
	for i, row := range rows {
		result[i] = append([]HelpItem(nil), row...)
	}
	return result
}
