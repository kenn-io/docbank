package screen

import "strings"

// Splice replaces replaceWidth terminal cells of background, starting at
// column, with replacement. The replacement is truncated or space-padded to
// that width. Negative columns clip the replacement on the left; the line has
// no right viewport boundary, so a replacement may extend it. Background
// content before and after the replaced interval is preserved.
func Splice(background, replacement string, column, replaceWidth int) string {
	if replaceWidth <= 0 || column+replaceWidth <= 0 {
		return background
	}

	leftClip := max(0, -column)
	start := max(0, column)
	visibleWidth := replaceWidth - leftClip

	left, leftTrailingControls, leftWidth := prefixParts(background, start)
	var replacementSkipped int
	var replacementControls string
	replacementControls, replacement, replacementSkipped = suffixParts(replacement, leftClip)
	replacement = replacementControls + strings.Repeat(" ", max(0, replacementSkipped-leftClip)) + replacement
	replacement, replacementTrailingControls, replacementWidth := prefixParts(replacement, visibleWidth)
	rightStart := column + replaceWidth
	rightControls, right, backgroundSkipped := suffixParts(background, rightStart)
	rightPadding := max(0, backgroundSkipped-rightStart)

	var result strings.Builder
	result.Grow(len(left) + len(leftTrailingControls) + len(replacement) + len(replacementTrailingControls) + len(rightControls) + len(right) + start - leftWidth + visibleWidth - replacementWidth + rightPadding)
	result.WriteString(left)
	result.WriteString(strings.Repeat(" ", start-leftWidth))
	result.WriteString(leftTrailingControls)
	result.WriteString(replacement)
	result.WriteString(strings.Repeat(" ", visibleWidth-replacementWidth))
	result.WriteString(replacementTrailingControls)
	result.WriteString(rightControls)
	result.WriteString(strings.Repeat(" ", rightPadding))
	result.WriteString(right)
	return result.String()
}

// OverlayAt places panel over background with the panel's top-left cell at
// the zero-based row and column in a width-by-height viewport. The panel is
// clipped to the viewport instead of being repositioned. For a non-empty
// panel, background is padded with empty rows until it has height rows; any
// original rows beyond height are preserved unchanged. An empty panel or a
// non-positive viewport dimension returns background exactly as supplied.
func OverlayAt(background, panel string, width, height, row, column int) string {
	if panel == "" || width <= 0 || height <= 0 {
		return background
	}

	backgroundLines := strings.Split(background, "\n")
	for len(backgroundLines) < height {
		backgroundLines = append(backgroundLines, "")
	}

	panelLines := strings.Split(panel, "\n")
	panelWidth := Width(panel)
	visibleStart := max(0, column)
	visibleEnd := min(width, column+panelWidth)
	if visibleEnd <= visibleStart {
		return strings.Join(backgroundLines, "\n")
	}

	leftClip := visibleStart - column
	visibleWidth := visibleEnd - visibleStart
	for panelRow, panelLine := range panelLines {
		backgroundRow := row + panelRow
		if backgroundRow < 0 || backgroundRow >= height {
			continue
		}
		panelControls, visibleLine, panelSkipped := suffixParts(panelLine, leftClip)
		visibleLine = panelControls + strings.Repeat(" ", max(0, panelSkipped-leftClip)) + visibleLine
		backgroundLines[backgroundRow] = Splice(
			backgroundLines[backgroundRow], visibleLine, visibleStart, visibleWidth,
		)
	}
	return strings.Join(backgroundLines, "\n")
}

// OverlayCentered centers panel in a width-by-height viewport and delegates
// clipping and composition to OverlayAt. When the size difference is odd, the
// extra background cell or clipped panel cell is on the bottom or right.
func OverlayCentered(background, panel string, width, height int) string {
	if panel == "" {
		return background
	}
	panelHeight := len(strings.Split(panel, "\n"))
	row := (height - panelHeight) / 2
	column := (width - Width(panel)) / 2
	return OverlayAt(background, panel, width, height, row, column)
}
