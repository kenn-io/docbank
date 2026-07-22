package tui

import (
	"fmt"
	"strconv"
	"strings"

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
	lines = append(lines, m.renderBody(bodyHeight), m.renderFooter())
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
	if m.loading {
		right = m.styles.spinner.Render(m.spinnerIndicator()) + " loading"
	}
	if m.err != nil {
		return m.styles.error.Render(fit(left+" — "+quoted(m.err.Error()), m.width))
	}
	return m.styles.stats.Render(joinSides(left, right, m.width))
}

func (m Model) renderBody(height int) string {
	if m.width < 72 {
		listHeight := max((height*2)/3, 1)
		detailHeight := max(height-listHeight-1, 0)
		list := m.renderList(m.width, listHeight)
		if detailHeight == 0 {
			return list
		}
		return list + "\n" + m.styles.muted.Render(strings.Repeat("─", m.width)) + "\n" +
			m.renderDetail(m.width, detailHeight)
	}
	listWidth := max((m.width*55)/100, 34)
	detailWidth := max(m.width-listWidth-1, 1)
	return lipgloss.JoinHorizontal(
		lipgloss.Top,
		m.renderList(listWidth, height),
		m.styles.muted.Render(strings.Repeat("│\n", max(height-1, 0))+"│"),
		m.renderDetail(detailWidth, height),
	)
}

func (m Model) renderList(width, height int) string {
	lines := make([]string, 0, height)
	heading := "    TYPE  DOCUMENT"
	if m.mode == modeSearch {
		heading = "    TYPE  MATCH  DOCUMENT"
	}
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
		if item.node.Kind == "dir" {
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
		var line string
		if m.mode == modeSearch {
			line = fmt.Sprintf("  %-4s  %-7s %s", kind, match, quoted(label))
		} else {
			line = fmt.Sprintf("  %-4s  %s", kind, quoted(label))
		}
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

func (m Model) renderDetail(width, height int) string {
	lines := []string{
		m.styles.heading.Render(pad(fit(" Document authority", width), width)),
	}
	if height > 1 {
		lines = append(lines, m.styles.separator.Render(strings.Repeat("─", width)))
	}
	selected, ok := m.selected()
	if !ok {
		lines = append(lines, m.styles.muted.Render(" Nothing selected"))
	} else {
		node := selected.node
		lines = append(lines,
			" Path: "+quoted(selected.path),
			fmt.Sprintf(" Selector: id:%d", node.ID),
			" Kind: "+node.Kind,
			fmt.Sprintf(" Revision: %d", node.Revision),
			" Modified: "+node.ModifiedAt,
		)
		if node.Kind == "file" {
			lines = append(lines,
				" Version: "+node.CurrentVersionID,
				" SHA-256: "+node.BlobHash,
				fmt.Sprintf(" Size: %s (%d bytes)", formatBytes(node.Size), node.Size),
			)
			if node.MimeType != "" {
				lines = append(lines, " Media type: "+quoted(node.MimeType))
			}
		} else {
			lines = append(lines, " Enter: open directory")
		}
	}
	for index := range lines {
		plain := lines[index]
		if index == 0 {
			plain = m.styles.heading.Render(pad(fit(" Document authority", width), width))
		} else if index == 1 && height > 1 {
			plain = m.styles.separator.Render(strings.Repeat("─", width))
		} else {
			plain = pad(fit(plain, width), width)
		}
		lines[index] = plain
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	for len(lines) < height {
		lines = append(lines, strings.Repeat(" ", width))
	}
	return strings.Join(lines, "\n")
}

func (m Model) renderFooter() string {
	hints := []hint{{text: "↑/↓ move", priority: 100}}
	if selected, ok := m.selected(); ok && selected.node.Kind == "dir" {
		hints = append(hints, hint{text: "enter open", priority: 80})
	}
	if len(m.stack) > 0 || m.mode == modeSearch {
		hints = append(hints, hint{text: "← back", priority: 75})
	}
	hints = append(hints,
		hint{text: "/ search", priority: 90},
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
		"Esc/←/h        Return to the previous view",
		"/              Search names and extracted text",
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
		return "-"
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
