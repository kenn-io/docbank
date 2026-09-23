package tui

import (
	"fmt"
	"strconv"
	"strings"

	"charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"go.kenn.io/docbank/internal/api"
)

type packagesLoadedMsg struct {
	requestID uint64
	page      api.PackagePage
	err       error
}

type packageMembersLoadedMsg struct {
	requestID uint64
	packageID string
	page      api.PackageMemberPage
	err       error
}

type packageLabelLoadedMsg struct {
	requestID uint64
	label     string
	page      api.PackageLabelCandidatePage
	err       error
}

func packageResponseCurrent(requestID, responseID uint64) bool { return requestID == responseID }

func (m Model) loadPackages(after string, requestID uint64) tea.Cmd {
	ctx, backend := m.ctx, m.backend
	return func() tea.Msg {
		page, err := backend.Packages(ctx, "", after, 250)
		return packagesLoadedMsg{requestID: requestID, page: page, err: err}
	}
}

func (m Model) loadPackageMembers(packageID string, afterOrdinal int, requestID uint64) tea.Cmd {
	ctx, backend := m.ctx, m.backend
	return func() tea.Msg {
		page, err := backend.PackageMembers(ctx, packageID, afterOrdinal, 250)
		return packageMembersLoadedMsg{requestID: requestID, packageID: packageID, page: page, err: err}
	}
}

func (m Model) loadPackageLabel(label, packageID, cursor string, requestID uint64) tea.Cmd {
	ctx, backend := m.ctx, m.backend
	return func() tea.Msg {
		page, err := backend.LookupLabel(ctx, label, packageID, cursor, 100)
		return packageLabelLoadedMsg{requestID: requestID, label: label, page: page, err: err}
	}
}

func (m Model) updatePackageLabelInput(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case keyCtrlC:
		m.quitting = true
		return m, tea.Quit
	case keyEscape:
		m.restorePackageLabelInput()
		return m, nil
	case keyEnter:
		label := strings.TrimSpace(m.searchInput.Value())
		if label == "" {
			m.restorePackageLabelInput()
			return m, nil
		}
		selected, ok := m.selectedPackage()
		if !ok {
			return m, nil
		}
		m.restorePackageLabelInput()
		m.packagesLoading = true
		m.packageLabelErr = nil
		m.packageLabelMatches = nil
		m.packageLabel = label
		m.packageLabelNextCursor = ""
		m.packageLabelCursor, m.packageLabelOffset = 0, 0
		m.packagesErr = nil
		m.packagesRequestID++
		return m, tea.Batch(m.startSpinner(), m.loadPackageLabel(label, selected.PackageID, "", m.packagesRequestID))
	default:
		var cmd tea.Cmd
		m.searchInput, cmd = m.searchInput.Update(msg)
		return m, cmd
	}
}

func (m *Model) restorePackageLabelInput() {
	m.packageLabelSearching = false
	m.searchInput.Blur()
	m.searchInput.Prompt = "/ "
	m.searchInput.Placeholder = searchPlaceholder
	m.searchInput.SetValue("")
}

func (m Model) updatePackagesKeys(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", keyCtrlC:
		m.quitting = true
		return m, tea.Quit
	case "?":
		m.helpOpen = true
	case keyEscape, keyBackspace, keyLeft, "h":
		m.packagesRequestID++
		m.packagesLoading = false
		if m.packageLabel != "" || m.packageLabelErr != nil {
			m.packageLabel = ""
			m.packageLabelMatches = nil
			m.packageLabelErr = nil
			m.packageLabelNextCursor = ""
			m.packageLabelCursor, m.packageLabelOffset = 0, 0
			return m, nil
		}
		if m.packageMembersOpen {
			m.packageMembersOpen = false
			m.packageMembers = nil
			m.packageMembersCursor, m.packageMembersOffset = 0, 0
			m.packageMembersNextAfter = 0
			m.packagesErr = nil
			return m, nil
		}
		m.packagesOpen = false
	case "r", "n":
		next := msg.String() == "n"
		if next && (m.packagesLoading || !m.packageHasNextPage()) {
			return m, nil
		}
		m.packagesLoading = true
		m.packagesErr, m.packageLabelErr = nil, nil
		m.packagesRequestID++
		if m.packageLabel != "" {
			cursor := ""
			if next {
				cursor = m.packageLabelNextCursor
			}
			selected, _ := m.selectedPackage()
			return m, tea.Batch(m.startSpinner(), m.loadPackageLabel(m.packageLabel, selected.PackageID, cursor, m.packagesRequestID))
		}
		if m.packageMembersOpen {
			afterOrdinal := 0
			if next {
				afterOrdinal = m.packageMembersNextAfter
			}
			return m, tea.Batch(m.startSpinner(), m.loadPackageMembers(m.packageMembersPackage.PackageID, afterOrdinal, m.packagesRequestID))
		}
		after := ""
		if next {
			after = m.packagesNextAfter
		}
		return m, tea.Batch(m.startSpinner(), m.loadPackages(after, m.packagesRequestID))
	case keyEnter:
		if m.packageMembersOpen || m.packageLabel != "" {
			return m, nil
		}
		selected, ok := m.selectedPackage()
		if !ok {
			return m, nil
		}
		if selected.SnapshotID == "" {
			m.notice = "Members are available once the package import completes"
			return m, nil
		}
		m.packageMembersOpen = true
		m.packageMembersPackage = selected
		m.packageMembers = nil
		m.packageMembersCursor, m.packageMembersOffset = 0, 0
		m.packageMembersNextAfter = 0
		m.packagesLoading = true
		m.packagesErr = nil
		m.packagesRequestID++
		return m, tea.Batch(m.startSpinner(), m.loadPackageMembers(selected.PackageID, 0, m.packagesRequestID))
	case "l":
		if _, ok := m.selectedPackage(); !ok {
			return m, nil
		}
		m.searchInput.SetValue("")
		m.searchInput.Prompt = "Label / "
		m.searchInput.Placeholder = "exact Bates label"
		m.packageLabelSearching = true
		m.notice = ""
		return m, m.searchInput.Focus()
	case "up", "k":
		m.movePackageCursor(-1)
	case keyDown, "j":
		m.movePackageCursor(1)
	case keyPageUp:
		m.movePackageCursor(-m.visiblePackageRows())
	case keyPageDown:
		m.movePackageCursor(m.visiblePackageRows())
	case keyHome, "g":
		m.movePackageCursor(-len(m.packages) - len(m.packageMembers) - len(m.packageLabelMatches))
	case keyEnd, "G":
		m.movePackageCursor(len(m.packages) + len(m.packageMembers) + len(m.packageLabelMatches))
	}
	return m, nil
}

func (m Model) selectedPackage() (api.PackageSummary, bool) {
	if m.packagesCursor < 0 || m.packagesCursor >= len(m.packages) {
		return api.PackageSummary{}, false
	}
	return m.packages[m.packagesCursor], true
}

func (m *Model) movePackageCursor(delta int) {
	if m.packageLabel != "" {
		m.packageLabelCursor += delta
	} else if m.packageMembersOpen {
		m.packageMembersCursor += delta
	} else {
		m.packagesCursor += delta
	}
	m.clampPackageSelection()
}

// clampPackageSelection keeps the cursor inside the list and the list
// scrolled so the cursor row is visible.
func (m *Model) clampPackageSelection() {
	visible := m.visiblePackageRows()
	clamp := func(cursor, offset *int, length int) {
		*cursor = min(max(*cursor, 0), max(length-1, 0))
		if *cursor < *offset {
			*offset = *cursor
		}
		if *cursor >= *offset+visible {
			*offset = *cursor - visible + 1
		}
		*offset = min(max(*offset, 0), max(length-1, 0))
	}
	clamp(&m.packageLabelCursor, &m.packageLabelOffset, len(m.packageLabelMatches))
	clamp(&m.packageMembersCursor, &m.packageMembersOffset, len(m.packageMembers))
	clamp(&m.packagesCursor, &m.packagesOffset, len(m.packages))
}

func (m Model) visiblePackageRows() int { return max(m.bodyViewportHeight()-2, 1) }

func (m Model) renderPackagesLocation() string {
	left := " Load-file packages · received and produced"
	right := fmt.Sprintf("%d package(s)", len(m.packages))
	if m.packagesNextAfter != "" {
		right = fmt.Sprintf("%d+ package(s)", len(m.packages))
	}
	if m.packageMembersOpen {
		left = " Load-file package · " + quoted(m.packageMembersPackage.PackageName)
		right = fmt.Sprintf("%d document(s)", len(m.packageMembers))
		if m.packageMembersNextAfter != 0 {
			right = fmt.Sprintf("%d+ document(s)", len(m.packageMembers))
		}
	}
	if m.packageLabel != "" {
		left = " Load-file package · label " + quoted(m.packageLabel)
		right = fmt.Sprintf("%d match(es)", len(m.packageLabelMatches))
		if m.packageLabelNextCursor != "" {
			right = fmt.Sprintf("%d+ match(es)", len(m.packageLabelMatches))
		}
	}
	if m.packagesLoading {
		right = m.styles.spinner.Render(m.spinnerIndicator()) + " loading"
	}
	if m.packagesErr != nil {
		right = "packages unavailable"
	}
	return m.styles.stats.Render(joinSides(left, right, m.width))
}

func (m Model) renderPackages(height int) string {
	lines := make([]string, 0, height)
	if m.packageLabel != "" || m.packageLabelErr != nil {
		lines = append(lines, m.styles.heading.Render(pad("  Label match                              Set          Provenance  Page", m.width)))
	} else if m.packageMembersOpen {
		lines = append(lines, m.styles.heading.Render(pad("  #   Document                                      Kind        Pages", m.width)))
	} else {
		lines = append(lines, m.styles.heading.Render(pad("  Package                              Direction   State and contents", m.width)))
	}
	if height > 1 {
		lines = append(lines, m.styles.separator.Render(strings.Repeat("─", m.width)))
	}
	visible := max(height-2, 0)
	if m.packagesErr != nil && visible > 0 {
		wrapped := strings.Split(ansi.Hardwrap(" "+quoted(m.packagesErr.Error()), max(m.width, 1), false), "\n")
		for _, line := range wrapped[:min(len(wrapped), visible)] {
			lines = append(lines, m.styles.error.Render(pad(fit(line, m.width), m.width)))
		}
	} else if m.packageLabelErr != nil {
		wrapped := strings.Split(ansi.Hardwrap(" "+quoted(m.packageLabelErr.Error()), max(m.width, 1), false), "\n")
		for _, line := range wrapped[:min(len(wrapped), visible)] {
			lines = append(lines, m.styles.error.Render(pad(fit(line, m.width), m.width)))
		}
	} else if m.packageLabel != "" {
		end := min(m.packageLabelOffset+visible, len(m.packageLabelMatches))
		for index := m.packageLabelOffset; index < end; index++ {
			item := m.packageLabelMatches[index]
			page := "unknown"
			if item.PageState == "verified" && item.PageNumber > 0 {
				page = strconv.Itoa(item.PageNumber)
			}
			line := fmt.Sprintf("  %-40s %-12s %-11s %s", quoted(item.Label), quoted(item.LabelSet), item.Provenance, page)
			lines = append(lines, m.packageRow(line, index, m.packageLabelCursor))
		}
		if len(m.packageLabelMatches) == 0 && visible > 0 {
			lines = append(lines, m.styles.muted.Render(pad(" No scoped match for "+quoted(m.packageLabel), m.width)))
		}
	} else if m.packageMembersOpen {
		end := min(m.packageMembersOffset+visible, len(m.packageMembers))
		for index := m.packageMembersOffset; index < end; index++ {
			member := m.packageMembers[index]
			pages := member.SourcePageCount
			if len(member.SelectedSourcePages) > 0 {
				pages = len(member.SelectedSourcePages)
			}
			line := fmt.Sprintf("  %-3d %-45s %-11s %d", member.Ordinal, quoted(member.DisplayName), member.DocumentKind, pages)
			lines = append(lines, m.packageRow(line, index, m.packageMembersCursor))
		}
	} else {
		end := min(m.packagesOffset+visible, len(m.packages))
		for index := m.packagesOffset; index < end; index++ {
			item := m.packages[index]
			left := fmt.Sprintf("  %s  %s", quoted(item.PackageName), item.Direction)
			status := fmt.Sprintf("%s · %d documents · %d pages ", item.State, item.MemberCount, item.PageCount)
			line := joinSides(left, status, m.width)
			lines = append(lines, m.packageRow(line, index, m.packagesCursor))
		}
	}
	if len(lines) == 2 && visible > 0 && m.packagesErr == nil {
		message := " No load-file packages"
		if m.packagesLoading {
			message = " Loading load-file packages..."
		}
		lines = append(lines, m.styles.muted.Render(pad(message, m.width)))
	}
	for len(lines) < height {
		lines = append(lines, strings.Repeat(" ", m.width))
	}
	return strings.Join(lines, "\n")
}

func (m Model) packageRow(line string, index, cursor int) string {
	line = pad(fit(line, m.width), m.width)
	if index == cursor {
		return m.styles.cursor.Render("▶" + line[1:])
	}
	if index%2 == 1 {
		return m.styles.alternate.Render(line)
	}
	return line
}

func (m Model) packageHasNextPage() bool {
	if m.packageLabel != "" {
		return m.packageLabelNextCursor != ""
	}
	if m.packageMembersOpen {
		return m.packageMembersNextAfter != 0
	}
	return m.packagesNextAfter != ""
}

func (m Model) renderPackagesFooter() string {
	hints := []hint{{text: "↑/↓ move", priority: 100}, {text: "r first page", priority: 75},
		{text: "l label lookup", priority: 55}, {text: "esc back", priority: 90},
		{text: hintHelp, priority: 65}, {text: hintQuit, priority: 50}}
	if !m.packageMembersOpen && m.packageLabel == "" {
		hints = append(hints, hint{text: "enter members", priority: 95})
	}
	if m.packageHasNextPage() {
		hints = append(hints, hint{text: "n next page", priority: 95})
	}
	position := ""
	if m.packageLabel != "" {
		if len(m.packageLabelMatches) > 0 {
			position = fmt.Sprintf(" %d/%d ", m.packageLabelCursor+1, len(m.packageLabelMatches))
		}
	} else if m.packageMembersOpen && len(m.packageMembers) > 0 {
		position = fmt.Sprintf(" %d/%d ", m.packageMembersCursor+1, len(m.packageMembers))
	} else if len(m.packages) > 0 {
		position = fmt.Sprintf(" %d/%d ", m.packagesCursor+1, len(m.packages))
	}
	return m.styles.footer.Render(joinSides(fitHints(hints, max(m.width-len(position)-1, 0)), position, m.width))
}
