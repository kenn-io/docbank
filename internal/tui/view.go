package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

type styles struct {
	titleBar   lipgloss.Style
	stats      lipgloss.Style
	heading    lipgloss.Style
	separator  lipgloss.Style
	cursor     lipgloss.Style
	alternate  lipgloss.Style
	muted      lipgloss.Style
	error      lipgloss.Style
	footer     lipgloss.Style
	modal      lipgloss.Style
	modalTitle lipgloss.Style
	spinner    lipgloss.Style
}

func newStyles(dark bool) styles {
	lightDark := lipgloss.LightDark(dark)
	muted := lightDark(lipgloss.Color("#5C6773"), lipgloss.Color("#9AA5B1"))
	selection := lightDark(lipgloss.Color("#DCEEF3"), lipgloss.Color("#24454E"))
	alternate := lightDark(lipgloss.Color("#F4F7F8"), lipgloss.Color("#182124"))
	danger := lightDark(lipgloss.Color("#A40000"), lipgloss.Color("#FF8A80"))
	return styles{
		titleBar: lipgloss.NewStyle().Bold(true).
			Background(lightDark(lipgloss.Color("#DCE6E9"), lipgloss.Color("#26373C"))).
			Foreground(lightDark(lipgloss.Color("#142126"), lipgloss.Color("#F4FBFD"))).
			Padding(0, 1),
		stats:     lipgloss.NewStyle().Foreground(muted),
		heading:   lipgloss.NewStyle().Bold(true),
		separator: lipgloss.NewStyle().Foreground(muted).Faint(true),
		cursor:    lipgloss.NewStyle().Background(selection).Bold(true),
		alternate: lipgloss.NewStyle().Background(alternate),
		muted:     lipgloss.NewStyle().Foreground(muted),
		error:     lipgloss.NewStyle().Foreground(danger).Bold(true),
		footer:    lipgloss.NewStyle().Foreground(muted),
		modal: lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(1, 2).
			Background(lightDark(lipgloss.Color("#FFFFFF"), lipgloss.Color("#101416"))),
		modalTitle: lipgloss.NewStyle().Bold(true),
		spinner:    lipgloss.NewStyle().Bold(true),
	}
}

// View implements tea.Model.
func (m Model) View() tea.View {
	content := ""
	if !m.quitting {
		content = m.render()
	}
	view := tea.NewView(content)
	view.AltScreen = true
	return view
}

func (m Model) render() string {
	if m.width <= 0 || m.height <= 0 {
		return "Loading Docbank..."
	}
	lines := []string{
		m.renderTitleBar(),
		m.renderLocation(),
	}
	if m.searching {
		lines = append(lines, fit(m.searchInput.View(), m.width))
	}

	bodyHeight := max(m.height-len(lines)-1, 1)
	body := m.renderBody(bodyHeight)
	if m.detailOpen {
		body = m.renderExpandedDetail(bodyHeight)
	}
	lines = append(lines, body, m.renderFooter())
	content := strings.Join(lines, "\n")
	if m.helpOpen {
		return m.renderHelp(content)
	}
	return content
}

func (m Model) renderTitleBar() string {
	if m.width < 3 {
		return fit("docbank", m.width)
	}
	contentWidth := max(m.width-2, 1)
	left := "docbank  documents for you and your agents"
	right := "READ ONLY"
	if lipgloss.Width(left)+lipgloss.Width(right)+2 > contentWidth {
		left = "docbank"
	}
	content := joinSides(left, right, contentWidth)
	return m.styles.titleBar.Render(pad(content, contentWidth))
}

func (m Model) renderLocation() string {
	var left, right string
	if m.mode == modeSearch {
		left = " Search " + quoted(m.searchQuery)
		right = fmt.Sprintf("%d result(s)", len(m.rows))
		if m.truncated {
			right = "first 1,000 result(s)"
		}
	} else {
		left = " " + quoted(m.directory.Path)
		right = fmt.Sprintf("%d item(s)", m.total)
		if m.total > len(m.rows) {
			right = fmt.Sprintf("first %d of %d", len(m.rows), m.total)
		}
	}
	if m.width >= 48 {
		right += " · " + m.sortSummary()
	}
	if m.loading {
		right = m.styles.spinner.Render(m.spinnerIndicator()) + " loading"
	}
	if m.err != nil {
		return m.styles.error.Render(fit(left+" — "+quoted(m.err.Error()), m.width))
	}
	return m.styles.stats.Render(joinSides(left, right, m.width))
}

func (m Model) renderBody(height int) string {
	return m.renderList(m.width, height)
}

func (m Model) renderList(width, height int) string {
	lines := make([]string, 0, height)
	layout := newTableLayout(width, m.mode)
	heading := layout.render(
		"   ",
		m.columnHeading("DOCUMENT", sortByName),
		"TYPE",
		"MATCH",
		m.columnHeading("SIZE", sortBySize),
		m.columnHeading("MODIFIED", sortByModified),
	)
	lines = append(lines, m.styles.heading.Render(pad(fit(heading, width), width)))
	if height > 1 {
		lines = append(lines, m.styles.separator.Render(strings.Repeat("─", width)))
	}
	visible := max(height-2, 0)
	if len(m.rows) == 0 && visible > 0 {
		message := " No documents"
		if m.loading {
			message = " Loading..."
		}
		lines = append(lines, m.styles.muted.Render(pad(fit(message, width), width)))
	}
	end := min(m.offset+visible, len(m.rows))
	for index := m.offset; index < end; index++ {
		item := m.rows[index]
		kind := "FILE"
		if item.node.Kind == nodeKindDir {
			kind = "DIR "
		}
		label := item.path
		if m.mode == modeBrowse {
			label = item.node.Name
		}
		match := ""
		if item.match != "" {
			match = strings.ToUpper(item.match)
		}
		size := "-"
		if item.node.Kind == nodeKindFile {
			size = formatBytes(item.node.Size)
		}
		line := layout.render(
			"   ", quoted(label), kind, match, size, formatModified(item.node.ModifiedAt),
		)
		if index == m.cursor {
			line = "▶" + line[1:]
		}
		line = pad(fit(line, width), width)
		if index == m.cursor {
			lines = append(lines, m.styles.cursor.Render(line))
		} else if index%2 == 1 {
			lines = append(lines, m.styles.alternate.Render(line))
		} else {
			lines = append(lines, line)
		}
	}
	for len(lines) < height {
		lines = append(lines, strings.Repeat(" ", width))
	}
	return strings.Join(lines, "\n")
}

type tableLayout struct {
	width        int
	document     int
	showKind     bool
	showMatch    bool
	showSize     bool
	showModified bool
}

func newTableLayout(width int, mode viewMode) tableLayout {
	layout := tableLayout{
		width: width, showKind: width >= 30,
		showSize: width >= 48, showModified: width >= 72,
		showMatch: mode == modeSearch && width >= 90,
	}
	fixed := 3
	if layout.showKind {
		fixed += 2 + 4
	}
	if layout.showMatch {
		fixed += 2 + 7
	}
	if layout.showSize {
		fixed += 2 + 9
	}
	if layout.showModified {
		fixed += 2 + 16
	}
	layout.document = max(width-fixed, 1)
	return layout
}

func (l tableLayout) render(prefix, document, kind, match, size, modified string) string {
	var line strings.Builder
	line.WriteString(fit(prefix, 3))
	line.WriteString(pad(document, l.document))
	if l.showKind {
		line.WriteString("  ")
		line.WriteString(pad(kind, 4))
	}
	if l.showMatch {
		line.WriteString("  ")
		line.WriteString(pad(match, 7))
	}
	if l.showSize {
		line.WriteString("  ")
		line.WriteString(padLeft(size, 9))
	}
	if l.showModified {
		line.WriteString("  ")
		line.WriteString(pad(modified, 16))
	}
	return pad(line.String(), l.width)
}

func (m Model) columnHeading(label string, field sortField) string {
	if m.sortField != field {
		return label
	}
	if m.sortDesc {
		return label + "↓"
	}
	return label + "↑"
}

func (m Model) sortSummary() string {
	label := "name"
	switch m.sortField {
	case sortByRelevance:
		label = "relevance"
	case sortBySize:
		label = "size"
	case sortByModified:
		label = "modified"
	case sortByName:
	}
	if m.sortDesc {
		return label + "↓"
	}
	return label + "↑"
}

func (m Model) renderExpandedDetail(height int) string {
	lines := m.expandedDetailLines(m.width)
	maxOffset := max(len(lines)-height, 0)
	offset := min(m.detailOffset, maxOffset)
	end := min(offset+height, len(lines))
	visible := append([]string(nil), lines[offset:end]...)
	for len(visible) < height {
		visible = append(visible, strings.Repeat(" ", m.width))
	}
	return strings.Join(visible, "\n")
}

func (m Model) expandedDetailLines(width int) []string {
	heading := m.styles.heading.Render(pad(fit(" Complete document authority", width), width))
	separator := m.styles.separator.Render(strings.Repeat("─", max(width, 0)))
	lines := []string{heading, separator}
	selected, ok := m.selected()
	if !ok {
		return append(lines, m.styles.muted.Render(" Nothing selected"))
	}

	node := selected.node
	fields := make([]string, 0, 10)
	if node.Kind == nodeKindFile {
		fields = append(fields,
			" Version: "+node.CurrentVersionID,
			" SHA-256: "+node.BlobHash,
		)
	}
	fields = append(fields,
		" Path: "+quoted(selected.path),
		fmt.Sprintf(" Selector: id:%d", node.ID),
		" Kind: "+node.Kind,
		fmt.Sprintf(" Revision: %d", node.Revision),
		" Modified: "+node.ModifiedAt,
	)
	if node.Kind == nodeKindFile {
		fields = append(fields, fmt.Sprintf(" Size: %s (%d bytes)", formatBytes(node.Size), node.Size))
		if node.MimeType != "" {
			fields = append(fields, " Media type: "+quoted(node.MimeType))
		}
	}
	for _, field := range fields {
		wrapped := ansi.Hardwrap(field, max(width, 1), false)
		for line := range strings.SplitSeq(wrapped, "\n") {
			lines = append(lines, pad(line, width))
		}
	}
	return lines
}

func (m Model) detailViewportHeight() int {
	linesAboveBody := 2
	if m.searching {
		linesAboveBody++
	}
	return max(m.height-linesAboveBody-1, 1)
}

func (m *Model) clampDetailOffset() {
	maximum := max(len(m.expandedDetailLines(m.width))-m.detailViewportHeight(), 0)
	m.detailOffset = min(max(m.detailOffset, 0), maximum)
}

func (m Model) renderFooter() string {
	if m.detailOpen {
		return m.renderDetailFooter()
	}
	hints := []hint{{text: "↑/↓ move", priority: 100}}
	if selected, ok := m.selected(); ok {
		if selected.node.Kind == nodeKindDir {
			hints = append(hints, hint{text: "enter open", priority: 80})
		} else {
			hints = append(hints, hint{text: "enter inspect", priority: 85})
		}
		hints = append(hints, hint{text: "i inspect", priority: 65})
	}
	if len(m.stack) > 0 || m.mode == modeSearch {
		hints = append(hints, hint{text: "← back", priority: 75})
	}
	hints = append(hints,
		hint{text: "/ search", priority: 90},
		hint{text: "s sort", priority: 85},
		hint{text: "v reverse", priority: 25},
		hint{text: "r refresh", priority: 20},
		hint{text: "? help", priority: 70},
		hint{text: "q quit", priority: 60},
	)
	if m.searching {
		hints = []hint{
			{text: "enter search", priority: 100},
			{text: "esc cancel", priority: 90},
			{text: "ctrl+c quit", priority: 50},
		}
	}
	position := ""
	if len(m.rows) > 0 {
		total := max(m.total, len(m.rows))
		position = fmt.Sprintf(" %d/%d ", m.cursor+1, total)
	}
	available := max(m.width-lipgloss.Width(position)-1, 0)
	keys := fitHints(hints, available)
	return m.styles.footer.Render(joinSides(keys, position, m.width))
}

func (m Model) renderDetailFooter() string {
	lines := m.expandedDetailLines(m.width)
	viewport := m.detailViewportHeight()
	position := ""
	if len(lines) > viewport {
		last := min(m.detailOffset+viewport, len(lines))
		position = fmt.Sprintf(" %d-%d/%d ", m.detailOffset+1, last, len(lines))
	}
	hints := []hint{
		{text: "↑/↓ scroll", priority: 100},
		{text: "esc close", priority: 90},
		{text: "? help", priority: 70},
		{text: "q quit", priority: 60},
	}
	available := max(m.width-lipgloss.Width(position)-1, 0)
	return m.styles.footer.Render(joinSides(fitHints(hints, available), position, m.width))
}

type hint struct {
	text     string
	priority int
}

func fitHints(hints []hint, width int) string {
	keep := make([]bool, len(hints))
	for index := range keep {
		keep[index] = true
	}
	join := func() string {
		parts := make([]string, 0, len(hints))
		for index, item := range hints {
			if keep[index] {
				parts = append(parts, item.text)
			}
		}
		return strings.Join(parts, " │ ")
	}
	for width > 0 && ansi.StringWidth(join()) > width {
		lowest, count := -1, 0
		for index, item := range hints {
			if !keep[index] {
				continue
			}
			count++
			if lowest == -1 || item.priority < hints[lowest].priority {
				lowest = index
			}
		}
		if count <= 1 {
			break
		}
		keep[lowest] = false
	}
	return fit(join(), width)
}

func (m Model) spinnerIndicator() string {
	if m.spinnerFrame >= 0 && m.spinnerFrame < len(spinnerFrames) {
		return spinnerFrames[m.spinnerFrame]
	}
	return spinnerFrames[0]
}

func (m Model) renderHelp(background string) string {
	lines := []string{
		"Keyboard shortcuts",
		"",
		"↑/k, ↓/j       Move through documents",
		"PgUp/PgDn      Move one visible page",
		"Home/End       Jump to first or last",
		"Enter/→/l      Open a directory",
		"Enter/i        Inspect complete document authority",
		"Esc/←/h        Return to the previous view",
		"/              Search names and extracted text",
		"s              Cycle the sort column",
		"v              Reverse the sort direction",
		"r              Refresh the current view",
		"?              Open this help",
		"q              Quit",
		"",
		"Press any key to close",
	}
	contentWidth := min(max(m.width-8, 1), 54)
	maxLines := max(m.height-4, 1)
	if len(lines) > maxLines {
		lines = lines[:maxLines]
	}
	for index := range lines {
		lines[index] = fit(lines[index], contentWidth)
	}
	if len(lines) > 0 {
		lines[0] = m.styles.modalTitle.Render(lines[0])
	}
	modal := m.styles.modal.Render(strings.Join(lines, "\n"))
	return m.overlayModal(background, modal)
}

func formatBytes(value int64) string {
	if value == 0 {
		return "0 B"
	}
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	divisor, exponent := int64(unit), 0
	for amount := value / unit; amount >= unit; amount /= unit {
		divisor *= unit
		exponent++
	}
	return fmt.Sprintf("%.1f %cB", float64(value)/float64(divisor), "KMGTPE"[exponent])
}

func formatModified(value string) string {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return value
	}
	return parsed.Format("2006-01-02 15:04")
}

func quoted(value string) string {
	return strconv.QuoteToGraphic(value)
}

func fit(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(value) <= width {
		return value
	}
	return ansi.Truncate(value, width, "")
}

func joinSides(left, right string, width int) string {
	if width <= 0 {
		return ""
	}
	if right == "" {
		return pad(left, width)
	}
	right = fit(right, width)
	leftWidth := max(width-ansi.StringWidth(right)-1, 0)
	left = fit(left, leftWidth)
	gap := max(width-ansi.StringWidth(left)-ansi.StringWidth(right), 0)
	return left + strings.Repeat(" ", gap) + right
}

func pad(value string, width int) string {
	value = fit(value, width)
	return value + strings.Repeat(" ", max(width-lipgloss.Width(value), 0))
}

func padLeft(value string, width int) string {
	value = fit(value, width)
	return strings.Repeat(" ", max(width-lipgloss.Width(value), 0)) + value
}

func (m Model) overlayModal(background, modal string) string {
	backgroundLines := strings.Split(background, "\n")
	modalLines := strings.Split(modal, "\n")
	modalWidth := lipgloss.Width(modal)
	startLine := max((len(backgroundLines)-len(modalLines))/2, 0)
	leftPadding := max((m.width-modalWidth)/2, 0)

	for index, modalLine := range modalLines {
		lineIndex := startLine + index
		if lineIndex >= len(backgroundLines) {
			break
		}
		backgroundLine := backgroundLines[lineIndex]
		var combined strings.Builder
		if leftPadding > 0 {
			left := ansi.Truncate(backgroundLine, leftPadding, "")
			combined.WriteString(left)
			combined.WriteString(strings.Repeat(" ", max(leftPadding-lipgloss.Width(left), 0)))
		}
		combined.WriteString(modalLine)
		rightStart := leftPadding + modalWidth
		if rightStart < lipgloss.Width(backgroundLine) {
			combined.WriteString(ansi.Cut(backgroundLine, rightStart, 10000))
		}
		backgroundLines[lineIndex] = combined.String()
	}
	return strings.Join(backgroundLines, "\n")
}
