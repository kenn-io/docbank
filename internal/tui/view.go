package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type styles struct {
	title    lipgloss.Style
	location lipgloss.Style
	heading  lipgloss.Style
	selected lipgloss.Style
	muted    lipgloss.Style
	error    lipgloss.Style
	footer   lipgloss.Style
}

func newStyles(dark bool) styles {
	lightDark := lipgloss.LightDark(dark)
	primary := lightDark(lipgloss.Color("#005A74"), lipgloss.Color("#74D7EC"))
	muted := lightDark(lipgloss.Color("#5C6773"), lipgloss.Color("#9AA5B1"))
	selection := lightDark(lipgloss.Color("#D8EEF4"), lipgloss.Color("#174B57"))
	danger := lightDark(lipgloss.Color("#A40000"), lipgloss.Color("#FF8A80"))
	return styles{
		title:    lipgloss.NewStyle().Bold(true).Foreground(primary),
		location: lipgloss.NewStyle().Foreground(primary),
		heading:  lipgloss.NewStyle().Bold(true),
		selected: lipgloss.NewStyle().Background(selection).Bold(true),
		muted:    lipgloss.NewStyle().Foreground(muted),
		error:    lipgloss.NewStyle().Foreground(danger),
		footer:   lipgloss.NewStyle().Foreground(muted),
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
		m.styles.title.Render(fit("docbank — documents for you and your agents", m.width)),
		m.renderLocation(),
	}
	if m.searching {
		lines = append(lines, fit(m.searchInput.View(), m.width))
	}

	bodyHeight := max(m.height-len(lines)-2, 1)
	lines = append(lines, m.renderBody(bodyHeight), m.renderFooter())
	return strings.Join(lines, "\n")
}

func (m Model) renderLocation() string {
	var value string
	if m.mode == modeSearch {
		value = fmt.Sprintf("Search %s — %d result(s)", quoted(m.searchQuery), len(m.rows))
		if m.truncated {
			value += " (first 1000)"
		}
	} else {
		value = quoted(m.directory.Path)
		if m.total > len(m.rows) {
			value += fmt.Sprintf(" — first %d of %d", len(m.rows), m.total)
		}
	}
	if m.loading {
		value += " — loading"
	}
	if m.err != nil {
		value += " — " + quoted(m.err.Error())
		return m.styles.error.Render(fit(value, m.width))
	}
	return m.styles.location.Render(fit(value, m.width))
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
			m.renderDetail(m.width, max(detailHeight-1, 1))
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
	heading := " Documents"
	if m.mode == modeSearch {
		heading = " Search results"
	}
	lines = append(lines, m.styles.heading.Render(pad(fit(heading, width), width)))
	visible := max(height-1, 0)
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
		kind := "f"
		if item.node.Kind == "dir" {
			kind = "d"
		}
		label := item.path
		if m.mode == modeBrowse {
			label = item.node.Name
		}
		line := fmt.Sprintf(" %s  %s", kind, quoted(label))
		if item.match != "" {
			line += "  [" + item.match + "]"
		}
		line = pad(fit(line, width), width)
		if index == m.cursor {
			lines = append(lines, m.styles.selected.Render(line))
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
	lines := []string{m.styles.heading.Render(" Document")}
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
				fmt.Sprintf(" Size: %d bytes", node.Size),
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
			plain = m.styles.heading.Render(pad(fit(" Document", width), width))
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
	text := "↑/↓ move  enter open  ← back  / search  r refresh  q quit"
	if m.searching {
		text = "enter search  esc cancel  ctrl+c quit"
	} else if m.mode == modeSearch {
		text = "↑/↓ move  enter open directory  esc results  / search  r refresh  q quit"
	}
	return m.styles.footer.Render(fit(text, m.width))
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
	if width == 1 {
		return "…"
	}
	runes := []rune(value)
	for len(runes) > 0 && lipgloss.Width(string(runes)+"…") > width {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…"
}

func pad(value string, width int) string {
	return value + strings.Repeat(" ", max(width-lipgloss.Width(value), 0))
}
