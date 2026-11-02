package helprender

import (
	"image/color"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"

	"go.kenn.io/kit/tui/helplayout"
)

// ColumnGap is the complete visible width inserted before every column
// after the first: the ▕ border cell plus one padding cell. Pass it to
// helplayout.ReflowRows so layout and rendering agree.
const ColumnGap = 2

// Styles selects the presentation RenderHelpTable applies. Key and
// Description style item text; BorderColor colors the ▕ separator, and a
// nil BorderColor leaves it uncolored.
type Styles struct {
	Key         lipgloss.Style
	Description lipgloss.Style
	BorderColor color.Color
}

// RenderHelpTable renders already-reflowed help rows as an aligned table.
// A key with a non-empty description renders as key, one space, then
// description. Columns after the first are separated by a ▕ border that is
// hidden for cells padding a short row to the table's column count;
// a present item with empty text keeps its border. Rows share one column
// grid sized by helplayout.ColumnWidths.
func RenderHelpTable(rows [][]helplayout.HelpItem, st Styles) string {
	if len(rows) == 0 {
		return ""
	}

	cellStyle := lipgloss.NewStyle()
	cellWithBorder := lipgloss.NewStyle().
		PaddingLeft(1).
		Border(lipgloss.Border{Left: "▕"}, false, false, false, true)
	if st.BorderColor != nil {
		cellWithBorder = cellWithBorder.BorderForeground(st.BorderColor)
	}

	colMinW := helplayout.ColumnWidths(rows)
	maxCols := len(colMinW)

	t := table.New().
		BorderTop(false).
		BorderBottom(false).
		BorderLeft(false).
		BorderRight(false).
		BorderColumn(false).
		BorderRow(false).
		StyleFunc(func(row, col int) lipgloss.Style {
			minW := 0
			if col < len(colMinW) {
				minW = colMinW[col]
			}
			// Cells padding a short row to the grid drop the border.
			if col == 0 || (row < len(rows) && col >= len(rows[row])) {
				return cellStyle.Width(minW)
			}
			return cellWithBorder.Width(minW + ColumnGap)
		}).
		Wrap(false)

	for _, row := range rows {
		styled := make([]string, maxCols)
		for i, item := range row {
			if item.Description != "" {
				styled[i] = st.Key.Render(item.Key) + " " + st.Description.Render(item.Description)
			} else {
				styled[i] = st.Key.Render(item.Key)
			}
		}
		t = t.Row(styled...)
	}

	return t.Render()
}
