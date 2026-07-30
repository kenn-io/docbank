package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"go.kenn.io/docbank/internal/api"
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
	lines := []string{m.renderTitleBar()}
	if m.jobsOpen {
		lines = append(lines, m.renderJobsLocation())
	} else if m.operationsOpen {
		lines = append(lines, m.renderOperationsLocation())
	} else if m.trashOpen {
		lines = append(lines, m.renderTrashLocation())
	} else if m.historyOpen {
		lines = append(lines, m.renderHistoryLocation())
	} else {
		lines = append(lines, m.renderLocation())
	}
	if m.searching {
		lines = append(lines, fit(m.searchInput.View(), m.width))
	}
	if m.notice != "" {
		lines = append(lines, m.styles.stats.Render(fit(" "+m.notice, m.width)))
	}

	bodyHeight := max(m.height-len(lines)-1, 1)
	body := m.renderBody(bodyHeight)
	if m.jobsOpen {
		body = m.renderJobsList(bodyHeight)
	} else if m.operationsOpen {
		body = m.renderOperations(bodyHeight)
	} else if m.trashOpen {
		body = m.renderTrashList(bodyHeight)
	} else if m.historyOpen {
		body = m.renderHistoryList(bodyHeight)
	}
	if m.detailOpen {
		body = m.renderExpandedDetail(bodyHeight)
	}
	if m.historyDetail {
		body = m.renderHistoryDetail(bodyHeight)
	}
	if m.jobDetail {
		body = m.renderJobDetail(bodyHeight)
	}
	lines = append(lines, body, m.renderFooter())
	content := strings.Join(lines, "\n")
	if m.helpOpen {
		return m.renderHelp(content)
	}
	if m.confirmation != nil {
		return m.renderConfirmation(content)
	}
	return content
}

func (m Model) renderJobsLocation() string {
	left := " Daemon activity · background jobs"
	right := fmt.Sprintf("%d running · %d total", m.jobsRunning, m.jobsTotal)
	if m.jobsTotal > len(m.jobs) {
		right = fmt.Sprintf(
			"%d running · first %d of %d", m.jobsRunning, len(m.jobs), m.jobsTotal,
		)
	}
	if m.jobsLoading {
		right = m.styles.spinner.Render(m.spinnerIndicator()) + " loading"
	}
	if m.jobsErr != nil {
		right = "jobs unavailable"
	}
	return m.styles.stats.Render(joinSides(left, right, m.width))
}

func (m Model) renderOperationsLocation() string {
	left := " Vault operations · read-only"
	right := fmt.Sprintf("%d recovery point(s)", m.operationsTotal)
	if m.operationsTotal > len(m.operationsSnapshots) {
		right = fmt.Sprintf(
			"first %d of %d recovery points",
			len(m.operationsSnapshots), m.operationsTotal,
		)
	}
	if m.operationsInfoBusy || m.operationsBackupBusy {
		switch {
		case m.operationsInfoBusy && m.operationsBackupBusy:
			right = m.styles.spinner.Render(m.spinnerIndicator()) + " loading"
		case m.operationsInfoBusy:
			right = m.styles.spinner.Render(m.spinnerIndicator()) + " loading storage"
		default:
			right = m.styles.spinner.Render(m.spinnerIndicator()) + " loading backups"
		}
	} else if m.operationsStorageErr != nil && m.operationsBackupErr != nil {
		right = "status unavailable"
	} else if m.operationsStorageErr != nil || m.operationsBackupErr != nil {
		right = "partial status"
	}
	return m.styles.stats.Render(joinSides(left, right, m.width))
}

func (m Model) renderHistoryLocation() string {
	left := " Audit history · " + quoted(m.historyNode.path)
	right := ""
	if page, ok := m.currentHistoryPage(); ok {
		start := 1
		for index := range m.historyPage {
			start += len(m.historyPages[index].Items)
		}
		end := start + len(page.Items) - 1
		if len(page.Items) == 0 {
			start, end = 0, 0
		}
		right = fmt.Sprintf("events %d-%d of %d", start, end, page.Total)
	}
	if m.loading {
		right = m.styles.spinner.Render(m.spinnerIndicator()) + " loading"
	}
	if m.err != nil {
		right = "history unavailable"
	}
	return m.styles.stats.Render(joinSides(left, right, m.width))
}

func (m Model) renderTrashLocation() string {
	left := " Recoverable trash · newest first"
	right := fmt.Sprintf("%d restorable root(s)", m.trashTotal)
	if m.trashTotal > len(m.trashItems) {
		right = fmt.Sprintf("first %d of %d", len(m.trashItems), m.trashTotal)
	}
	if m.trashLoading {
		right = m.styles.spinner.Render(m.spinnerIndicator()) + " loading"
	}
	if m.trashErr != nil {
		return m.styles.error.Render(fit(left+" — "+quoted(m.trashErr.Error()), m.width))
	}
	return m.styles.stats.Render(joinSides(left, right, m.width))
}

func (m Model) renderTitleBar() string {
	if m.width < 3 {
		return fit("docbank", m.width)
	}
	contentWidth := max(m.width-2, 1)
	left := "docbank  documents for you and your agents"
	right := "RECOVERABLE CHANGES"
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
	} else if !m.sortIndicatorVisible() {
		right = m.sortSummary()
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

func (m Model) renderJobsList(height int) string {
	lines := make([]string, 0, height)
	lines = append(lines, m.renderJobsHeading())
	if height > 1 {
		lines = append(lines, m.styles.separator.Render(strings.Repeat("─", m.width)))
	}
	visible := max(height-2, 0)
	if m.jobsErr != nil && visible > 0 {
		wrapped := strings.Split(ansi.Hardwrap(
			" "+quoted(m.jobsErr.Error()), max(m.width, 1), false,
		), "\n")
		for _, line := range wrapped[:min(len(wrapped), visible)] {
			lines = append(lines, m.styles.error.Render(pad(fit(line, m.width), m.width)))
		}
		visible -= min(len(wrapped), visible)
	} else if len(m.jobs) == 0 && visible > 0 {
		message := " No background jobs"
		if m.jobsLoading {
			message = " Loading background jobs..."
		}
		lines = append(lines, m.styles.muted.Render(pad(fit(message, m.width), m.width)))
		visible--
	}
	end := min(m.jobsOffset+visible, len(m.jobs))
	for index := m.jobsOffset; index < end; index++ {
		line := m.renderJobRow(m.jobs[index])
		if index == m.jobsCursor {
			line = "▶" + line[1:]
		}
		line = pad(fit(line, m.width), m.width)
		if index == m.jobsCursor {
			lines = append(lines, m.styles.cursor.Render(line))
		} else if index%2 == 1 {
			lines = append(lines, m.styles.alternate.Render(line))
		} else {
			lines = append(lines, line)
		}
	}
	for len(lines) < height {
		lines = append(lines, strings.Repeat(" ", m.width))
	}
	return strings.Join(lines, "\n")
}

func (m Model) renderOperations(height int) string {
	lines := m.operationsLines(m.width)
	maxOffset := max(len(lines)-height, 0)
	offset := min(m.operationsOffset, maxOffset)
	end := min(offset+height, len(lines))
	visible := append([]string(nil), lines[offset:end]...)
	for len(visible) < height {
		visible = append(visible, strings.Repeat(" ", m.width))
	}
	return strings.Join(visible, "\n")
}

func (m Model) operationsLines(width int) []string {
	separator := m.styles.separator.Render(strings.Repeat("─", max(width, 0)))
	lines := []string{
		m.styles.heading.Render(pad(fit(" Storage inventory", width), width)),
		separator,
	}
	if m.operationsInfoBusy && m.operationsInfo.VaultID == "" {
		lines = append(lines, m.styles.muted.Render(pad(
			" Loading storage inventory...", width,
		)))
	} else if m.operationsStorageErr != nil {
		lines = appendWrapped(lines, " Storage unavailable: "+
			quoted(m.operationsStorageErr.Error()), width, m.styles.error)
	} else {
		info, storage := m.operationsInfo, m.operationsInfo.Storage
		lines = appendWrapped(lines, " Vault: "+info.VaultID, width, lipgloss.NewStyle())
		lines = appendWrapped(lines, " Logical authority: "+
			countLabel(info.LiveFiles, "live file", "live files")+" · "+
			countLabel(info.LiveDirectories, "directory", "directories")+" · "+
			countLabel(info.ContentVersions, "version", "versions"),
			width, lipgloss.NewStyle())
		lines = appendWrapped(lines, fmt.Sprintf(
			" Tracked content: %s · %s",
			countLabel(info.TrackedBlobs, "blob", "blobs"),
			formatBytes(info.TrackedBlobBytes),
		), width, lipgloss.NewStyle())
		lines = appendWrapped(lines, fmt.Sprintf(
			" Loose inventory: %s · %s",
			countLabel(int64(storage.LooseBlobs), "file", "files"),
			formatBytes(storage.LooseBytes),
		), width, lipgloss.NewStyle())
		lines = appendWrapped(lines, fmt.Sprintf(
			" Pack inventory: %s · %s stored",
			countLabel(int64(storage.Packs), "pack", "packs"),
			formatBytes(storage.PackStoredBytes),
		), width, lipgloss.NewStyle())
		lines = appendWrapped(lines, fmt.Sprintf(
			" Live packed content: %s · %s raw · %s stored",
			countLabel(storage.PackedBlobs, "blob", "blobs"),
			formatBytes(storage.PackedRawBytes),
			formatBytes(storage.PackedStoredBytes),
		), width, lipgloss.NewStyle())
		lines = appendWrapped(lines, fmt.Sprintf(
			" Dead packed payload: %s awaiting explicit repack",
			formatBytes(storage.DeadPackedBytes),
		), width, lipgloss.NewStyle())
	}

	lines = append(lines, separator,
		m.styles.heading.Render(pad(fit(" Backup recovery points", width), width)),
		separator,
	)
	switch {
	case m.operationsBackupBusy && len(m.operationsSnapshots) == 0:
		lines = append(lines, m.styles.muted.Render(pad(
			" Loading backup recovery points...", width,
		)))
	case m.operationsBackupErr != nil:
		lines = appendWrapped(lines, " Backup repository unavailable: "+
			quoted(m.operationsBackupErr.Error()), width, m.styles.error)
	case len(m.operationsSnapshots) == 0:
		lines = append(lines, m.styles.muted.Render(pad(
			" No snapshots in the configured backup repository", width,
		)))
	default:
		for _, snapshot := range m.operationsSnapshots {
			tag := snapshot.Tag
			if tag == "" {
				tag = "untagged"
			}
			lines = appendWrapped(lines, fmt.Sprintf(
				" %s · %s · %s · +%s",
				formatModified(snapshot.CreatedAt), quoted(tag),
				countLabel(snapshot.Files, "file", "files"),
				formatBytes(snapshot.BytesAdded),
			), width, lipgloss.NewStyle())
			lines = appendWrapped(lines, "   Snapshot: "+snapshot.ID,
				width, m.styles.muted)
		}
		if m.operationsTotal > len(m.operationsSnapshots) {
			lines = append(lines, m.styles.muted.Render(pad(fmt.Sprintf(
				" Showing the first %d of %d recovery points",
				len(m.operationsSnapshots), m.operationsTotal,
			), width)))
		}
	}
	return lines
}

func appendWrapped(
	lines []string, value string, width int, style lipgloss.Style,
) []string {
	wrapped := ansi.Hardwrap(value, max(width, 1), false)
	for line := range strings.SplitSeq(wrapped, "\n") {
		lines = append(lines, style.Render(pad(line, width)))
	}
	return lines
}

func countLabel(value int64, singular, plural string) string {
	label := plural
	if value == 1 {
		label = singular
	}
	return fmt.Sprintf("%d %s", value, label)
}

func (m Model) renderJobsHeading() string {
	switch {
	case m.width >= 88:
		return m.styles.heading.Render(pad(
			"   STATUS       JOB                              STARTED            FINISHED", m.width,
		))
	case m.width >= 72:
		return m.styles.heading.Render(pad(
			"   STATUS       JOB                              STARTED", m.width,
		))
	default:
		return m.styles.heading.Render(pad("   STATUS       JOB", m.width))
	}
}

func (m Model) renderJobRow(job api.Job) string {
	status := pad(strings.ToUpper(job.Status), 11)
	nameWidth := max(m.width-16, 1)
	if m.width >= 72 {
		nameWidth = 32
	}
	line := "   " + status + "  " + pad(quoted(job.Name), nameWidth)
	if m.width >= 72 {
		line += "  " + pad(formatJobTime(job.StartedAt), 17)
	}
	if m.width >= 88 {
		finished := "running"
		if job.FinishedAt != "" {
			finished = formatJobTime(job.FinishedAt)
		}
		line += "  " + finished
	}
	return line
}

func (m Model) renderJobDetail(height int) string {
	lines := m.jobDetailLines(m.width)
	maximum := max(len(lines)-height, 0)
	offset := min(m.jobDetailOffset, maximum)
	end := min(offset+height, len(lines))
	visible := append([]string(nil), lines[offset:end]...)
	for len(visible) < height {
		visible = append(visible, strings.Repeat(" ", m.width))
	}
	return strings.Join(visible, "\n")
}

func (m Model) jobDetailLines(width int) []string {
	heading := m.styles.heading.Render(pad(fit(" Complete background job", width), width))
	separator := m.styles.separator.Render(strings.Repeat("─", max(width, 0)))
	lines := []string{heading, separator}
	job, ok := m.selectedJob()
	if !ok {
		return append(lines, m.styles.muted.Render(" Nothing selected"))
	}
	fields := []string{
		" Name: " + quoted(job.Name),
		" Status: " + job.Status,
		" Started: " + job.StartedAt,
	}
	if job.FinishedAt == "" {
		fields = append(fields, " Finished: still running")
	} else {
		fields = append(fields, " Finished: "+job.FinishedAt)
	}
	if job.Error != "" {
		fields = append(fields, " Failure: "+quoted(job.Error))
	}
	for _, field := range fields {
		wrapped := ansi.Hardwrap(field, max(width, 1), false)
		for line := range strings.SplitSeq(wrapped, "\n") {
			lines = append(lines, pad(line, width))
		}
	}
	return lines
}

func (m Model) renderList(width, height int) string {
	lines := make([]string, 0, height)
	layout := newTableLayout(width, m.mode)
	heading := layout.render(
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
			quoted(label), kind, match, size, formatModified(item.node.ModifiedAt),
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

func (m Model) renderTrashList(height int) string {
	width := max(m.width, 1)
	lines := make([]string, 0, height)
	layout := newTableLayout(width, modeBrowse)
	heading := layout.render("DOCUMENT", "TYPE", "", "SIZE", "TRASHED")
	lines = append(lines, m.styles.heading.Render(pad(fit(heading, width), width)))
	if height > 1 {
		lines = append(lines, m.styles.separator.Render(strings.Repeat("─", width)))
	}
	visible := max(height-2, 0)
	if len(m.trashItems) == 0 && visible > 0 {
		message := " Trash is empty"
		if m.trashLoading {
			message = " Loading..."
		}
		lines = append(lines, m.styles.muted.Render(pad(fit(message, width), width)))
	}
	end := min(m.trashOffset+visible, len(m.trashItems))
	for index := m.trashOffset; index < end; index++ {
		node := m.trashItems[index]
		kind, size := "FILE", formatBytes(node.Size)
		if node.Kind == nodeKindDir {
			kind, size = "DIR ", "-"
		}
		line := layout.render(
			quoted(node.Name), kind, "", size, formatModified(node.TrashedAt),
		)
		if index == m.trashCursor {
			line = "▶" + line[1:]
		}
		line = pad(fit(line, width), width)
		if index == m.trashCursor {
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

func (m Model) renderHistoryList(height int) string {
	lines := make([]string, 0, height)
	lines = append(lines, m.renderHistoryHeading())
	if height > 1 {
		lines = append(lines, m.styles.separator.Render(strings.Repeat("─", m.width)))
	}
	visible := max(height-2, 0)
	page, ok := m.currentHistoryPage()
	if !m.loading && m.err != nil && visible > 0 {
		messages := strings.Split(ansi.Hardwrap(
			" "+quoted(m.err.Error()), max(m.width, 1), false,
		), "\n")
		for _, line := range messages[:min(len(messages), visible)] {
			lines = append(lines, m.styles.muted.Render(pad(fit(line, m.width), m.width)))
		}
		visible -= min(len(messages), visible)
	} else if (!ok || len(page.Items) == 0) && visible > 0 {
		message := " No recorded events"
		if m.loading {
			message = " Loading permanent history..."
		}
		for _, line := range []string{message} {
			lines = append(lines, m.styles.muted.Render(pad(fit(line, m.width), m.width)))
		}
		visible--
	}
	if ok {
		end := min(m.historyOffset+visible, len(page.Items))
		for index := m.historyOffset; index < end; index++ {
			line := m.renderHistoryRow(page.Items[index])
			if index == m.historyCursor {
				line = "▶" + line[1:]
			}
			line = pad(fit(line, m.width), m.width)
			if index == m.historyCursor {
				lines = append(lines, m.styles.cursor.Render(line))
			} else if index%2 == 1 {
				lines = append(lines, m.styles.alternate.Render(line))
			} else {
				lines = append(lines, line)
			}
		}
	}
	for len(lines) < height {
		lines = append(lines, strings.Repeat(" ", m.width))
	}
	return strings.Join(lines, "\n")
}

func (m Model) renderHistoryHeading() string {
	if m.width >= 72 {
		return m.styles.heading.Render(pad("   RECORDED            EVENT                   CHANGE", m.width))
	}
	return m.styles.heading.Render(pad("   EVENT                   CHANGE", m.width))
}

func (m Model) renderHistoryRow(event api.AuditEvent) string {
	prefix := "   "
	kind := pad(quoted(event.Kind), 22)
	change := historyEventSummary(event)
	if m.width >= 72 {
		return prefix + pad(formatHistoryTime(event.RecordedAt), 17) + "  " + kind + "  " + change
	}
	return prefix + kind + "  " + change
}

func historyEventSummary(event api.AuditEvent) string {
	if event.OldPath != nil && event.NewPath != nil {
		return quoted(event.OldPath.Path) + " → " + quoted(event.NewPath.Path)
	}
	if event.PriorCurrentVersionID != nil || event.ResultingCurrentVersionID != nil {
		return "version " + shortAuditID(event.PriorCurrentVersionID) + " → " +
			shortAuditID(event.ResultingCurrentVersionID)
	}
	if event.Attachment != nil {
		identity := event.Attachment.Identity.TagID
		if identity == "" {
			identity = event.Attachment.Identity.ProvenanceID
		}
		return quoted(event.Attachment.Kind) + " " + shortString(identity, 12)
	}
	return fmt.Sprintf("revision %d → %d", event.PriorNodeRevision, event.ResultingNodeRevision)
}

func (m Model) renderHistoryDetail(height int) string {
	lines := m.historyDetailLines(m.width)
	maximum := max(len(lines)-height, 0)
	offset := min(m.historyDetailOffset, maximum)
	end := min(offset+height, len(lines))
	visible := append([]string(nil), lines[offset:end]...)
	for len(visible) < height {
		visible = append(visible, strings.Repeat(" ", m.width))
	}
	return strings.Join(visible, "\n")
}

func (m Model) historyDetailLines(width int) []string {
	heading := m.styles.heading.Render(pad(fit(" Complete audited event", width), width))
	separator := m.styles.separator.Render(strings.Repeat("─", max(width, 0)))
	lines := []string{heading, separator}
	event, ok := m.selectedHistoryEvent()
	if !ok {
		return append(lines, m.styles.muted.Render(" Nothing selected"))
	}
	fields := []string{
		" Event: " + event.ID,
		" Operation: " + event.OperationID,
		fmt.Sprintf(" Sequence: %d.%d", event.OperationSequence, event.Ordinal),
		" Scope: " + event.ScopeID,
		fmt.Sprintf(" Node: id:%d", event.NodeID),
		" Kind: " + event.Kind,
		" Recorded: " + event.RecordedAt,
		" Origin: " + quoted(event.Origin),
		fmt.Sprintf(" Revision: %d -> %d", event.PriorNodeRevision, event.ResultingNodeRevision),
	}
	if event.AgentLabel != nil {
		fields = append(fields, " Agent: "+quoted(*event.AgentLabel))
	}
	if event.OldPath != nil {
		fields = append(fields, " Before path: "+auditPathDetail(*event.OldPath))
	}
	if event.NewPath != nil {
		fields = append(fields, " After path: "+auditPathDetail(*event.NewPath))
	}
	if event.PriorCurrentVersionID != nil {
		fields = append(fields, " Before version: "+*event.PriorCurrentVersionID)
	}
	if event.ResultingCurrentVersionID != nil {
		fields = append(fields, " After version: "+*event.ResultingCurrentVersionID)
	}
	if event.SourceVersionID != nil {
		fields = append(fields, " Source version: "+*event.SourceVersionID)
	}
	if event.TargetNodeID != nil {
		fields = append(fields, fmt.Sprintf(" Target node: id:%d", *event.TargetNodeID))
	}
	if event.BaselineDigest != nil {
		fields = append(fields, " Baseline: "+*event.BaselineDigest)
	}
	fields = append(fields, auditAttachmentDetail(event.Attachment)...)
	for _, field := range fields {
		wrapped := ansi.Hardwrap(field, max(width, 1), false)
		for line := range strings.SplitSeq(wrapped, "\n") {
			lines = append(lines, pad(line, width))
		}
	}
	return lines
}

func auditPathDetail(state api.AuditPathState) string {
	return quoted(state.Path) + " (" + state.State + ")"
}

func auditAttachmentDetail(change *api.AuditAttachmentChange) []string {
	if change == nil {
		return nil
	}
	lines := []string{" Attachment: " + change.Kind}
	if change.Identity.TagID != "" {
		lines = append(lines, " Attachment tag: "+change.Identity.TagID)
	}
	if change.Identity.NodeID != 0 {
		lines = append(lines, fmt.Sprintf(" Attachment node: id:%d", change.Identity.NodeID))
	}
	if change.Identity.ProvenanceID != "" {
		lines = append(lines, " Attachment provenance: "+change.Identity.ProvenanceID)
	}
	lines = append(lines, auditAttachmentStateDetail("Before attachment", change.Before)...)
	lines = append(lines, auditAttachmentStateDetail("After attachment", change.After)...)
	return lines
}

func auditAttachmentStateDetail(label string, state *api.AuditAttachmentState) []string {
	if state == nil {
		return []string{" " + label + ": absent"}
	}
	lines := []string{" " + label + ": present"}
	if state.TagName != "" {
		lines = append(lines, "   Tag name: "+quoted(state.TagName))
	}
	if state.IngestID != "" {
		lines = append(lines, "   Ingest: "+state.IngestID)
	}
	if state.OriginalPath != nil {
		lines = append(lines, "   Original reference: "+quoted(*state.OriginalPath))
	}
	if state.OriginalMTime != nil {
		lines = append(lines, "   Original modified: "+*state.OriginalMTime)
	}
	if state.Supersedes != nil {
		lines = append(lines, "   Supersedes: "+*state.Supersedes)
	}
	return lines
}

func shortAuditID(value *string) string {
	if value == nil {
		return "none"
	}
	return shortString(*value, 8)
}

func shortString(value string, length int) string {
	if len(value) <= length {
		return value
	}
	return value[:length]
}

func formatHistoryTime(value string) string {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return value
	}
	return parsed.UTC().Format("2006-01-02 15:04Z")
}

func formatJobTime(value string) string {
	if value == "" {
		return "-"
	}
	return formatHistoryTime(value)
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
		fixed += 2 + 17
	}
	layout.document = max(width-fixed, 1)
	return layout
}

func (l tableLayout) render(document, kind, match, size, modified string) string {
	var line strings.Builder
	line.WriteString("   ")
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
		line.WriteString(pad(modified, 17))
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

func (m Model) sortIndicatorVisible() bool {
	layout := newTableLayout(m.width, m.mode)
	switch m.sortField {
	case sortByName:
		return layout.document >= lipgloss.Width("DOCUMENT↑")
	case sortBySize:
		return layout.showSize
	case sortByModified:
		return layout.showModified
	case sortByRelevance:
		return false
	default:
		return false
	}
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
	if m.detailNode.node.ID == 0 {
		return append(lines, m.styles.muted.Render(" Nothing selected"))
	}

	node := m.detailNode.node
	fields := make([]string, 0, 10)
	if node.Kind == nodeKindFile {
		fields = append(fields,
			" Version: "+node.CurrentVersionID,
			" SHA-256: "+node.BlobHash,
		)
	}
	fields = append(fields,
		" Path: "+quoted(m.detailNode.path),
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
	lines = append(lines, separator)
	switch {
	case m.detailTagsLoading:
		lines = append(lines, m.styles.muted.Render(pad(" Tags: loading...", width)))
	case m.detailTagsErr != nil:
		lines = append(lines, m.styles.error.Render(pad(
			fit(" Tags unavailable: "+quoted(m.detailTagsErr.Error()), width), width,
		)))
	case len(m.detailTags) == 0:
		lines = append(lines, m.styles.muted.Render(pad(" Tags: none", width)))
	default:
		label := fmt.Sprintf(" Tags (%d)", m.detailTagsTotal)
		if m.detailTagsTotal > len(m.detailTags) {
			label = fmt.Sprintf(" Tags (first %d of %d)", len(m.detailTags), m.detailTagsTotal)
		}
		lines = append(lines, m.styles.heading.Render(pad(fit(label, width), width)))
		for _, tag := range m.detailTags {
			field := "   " + quoted(tag.Name) + "  " + tag.ID
			wrapped := ansi.Hardwrap(field, max(width, 1), false)
			for line := range strings.SplitSeq(wrapped, "\n") {
				lines = append(lines, pad(line, width))
			}
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
	if m.jobsOpen {
		return m.renderJobsFooter()
	}
	if m.operationsOpen {
		return m.renderOperationsFooter()
	}
	if m.historyOpen {
		return m.renderHistoryFooter()
	}
	if m.trashOpen {
		return m.renderTrashFooter()
	}
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
		hints = append(hints,
			hint{text: "i inspect", priority: 65},
			hint{text: "a history", priority: 68},
			hint{text: "x trash", priority: 72},
		)
	}
	if len(m.stack) > 0 || m.mode == modeSearch {
		hints = append(hints, hint{text: "← back", priority: 75})
	}
	hints = append(hints,
		hint{text: "/ search", priority: 90},
		hint{text: "T recover", priority: 74},
		hint{text: "J jobs", priority: 68},
		hint{text: "O operations", priority: 66},
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

func (m Model) renderOperationsFooter() string {
	lines := m.operationsLines(m.width)
	viewport := m.operationsViewportHeight()
	position := ""
	if len(lines) > viewport {
		last := min(m.operationsOffset+viewport, len(lines))
		position = fmt.Sprintf(" %d-%d/%d ", m.operationsOffset+1, last, len(lines))
	}
	hints := []hint{
		{text: "↑/↓ scroll", priority: 100},
		{text: "r refresh", priority: 80},
		{text: "esc back", priority: 90},
		{text: "? help", priority: 70},
		{text: "q quit", priority: 60},
	}
	available := max(m.width-lipgloss.Width(position)-1, 0)
	return m.styles.footer.Render(joinSides(fitHints(hints, available), position, m.width))
}

func (m Model) renderTrashFooter() string {
	hints := []hint{
		{text: "↑/↓ move", priority: 100},
		{text: "enter restore", priority: 95},
		{text: "r refresh", priority: 70},
		{text: "esc back", priority: 90},
		{text: "? help", priority: 75},
		{text: "q quit", priority: 60},
	}
	position := ""
	if len(m.trashItems) > 0 {
		position = fmt.Sprintf(" %d/%d ", m.trashCursor+1, m.trashTotal)
	}
	available := max(m.width-lipgloss.Width(position)-1, 0)
	return m.styles.footer.Render(joinSides(fitHints(hints, available), position, m.width))
}

func (m Model) renderJobsFooter() string {
	if m.jobDetail {
		lines := m.jobDetailLines(m.width)
		viewport := m.jobsViewportHeight()
		position := ""
		if len(lines) > viewport {
			last := min(m.jobDetailOffset+viewport, len(lines))
			position = fmt.Sprintf(" %d-%d/%d ", m.jobDetailOffset+1, last, len(lines))
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
	hints := []hint{
		{text: "↑/↓ move", priority: 100},
		{text: "enter inspect", priority: 90},
		{text: "r refresh", priority: 80},
		{text: "esc back", priority: 85},
		{text: "? help", priority: 70},
		{text: "q quit", priority: 60},
	}
	position := ""
	if len(m.jobs) > 0 {
		position = fmt.Sprintf(" %d/%d ", m.jobsCursor+1, len(m.jobs))
	}
	available := max(m.width-lipgloss.Width(position)-1, 0)
	return m.styles.footer.Render(joinSides(fitHints(hints, available), position, m.width))
}

func (m Model) renderHistoryFooter() string {
	if m.historyDetail {
		lines := m.historyDetailLines(m.width)
		viewport := m.historyViewportHeight()
		position := ""
		if len(lines) > viewport {
			last := min(m.historyDetailOffset+viewport, len(lines))
			position = fmt.Sprintf(" %d-%d/%d ", m.historyDetailOffset+1, last, len(lines))
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
	hints := []hint{
		{text: "↑/↓ move", priority: 100},
		{text: "enter inspect", priority: 90},
		{text: "p newer", priority: 55},
		{text: "n older", priority: 60},
		{text: "esc back", priority: 85},
		{text: "? help", priority: 70},
		{text: "q quit", priority: 50},
	}
	position := ""
	if page, ok := m.currentHistoryPage(); ok && len(page.Items) > 0 {
		position = fmt.Sprintf(" %d/%d ", m.historyCursor+1, len(page.Items))
	}
	available := max(m.width-lipgloss.Width(position)-1, 0)
	return m.styles.footer.Render(joinSides(fitHints(hints, available), position, m.width))
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
	lines := m.helpLines()
	contentWidth := min(max(m.width-8, 1), 54)
	maxLines := max(m.height-4, 1)
	if len(lines) > maxLines {
		if maxLines >= 2 {
			lines = append(lines[:maxLines-1], lines[len(lines)-1])
		} else {
			lines = lines[:maxLines]
		}
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

func (m Model) renderConfirmation(background string) string {
	confirmation := m.confirmation
	if confirmation == nil {
		return background
	}
	node := confirmation.target.node
	title := "Move to recoverable trash?"
	action := "Move"
	subject := confirmation.target.path
	detail := []string{"The node remains restorable. No content bytes are reclaimed."}
	if confirmation.action == mutationRestore {
		title = "Restore this node?"
		action = "Restore"
		subject = node.Name
		detail = []string{
			"Docbank reports the actual restored path after resolving",
			"name collisions or missing origin parents.",
		}
	}
	status := "Enter " + strings.ToLower(action) + " · Esc cancel"
	if m.mutationRunning {
		status = m.styles.spinner.Render(m.spinnerIndicator()) + " " +
			strings.ToLower(action) + " in progress"
	}
	contentWidth := min(max(m.width-10, 1), 68)
	lines := []string{
		title,
		"",
		" " + quoted(subject),
		fmt.Sprintf(" Stable selector: id:%d", node.ID),
		fmt.Sprintf(" Bound revision: %d", node.Revision),
		"",
	}
	lines = append(lines, detail...)
	lines = append(lines, "", status)
	for index := range lines {
		lines[index] = fit(lines[index], contentWidth)
	}
	lines[0] = m.styles.modalTitle.Render(lines[0])
	modal := m.styles.modal.Render(strings.Join(lines, "\n"))
	return m.overlayModal(background, modal)
}

func (m Model) helpLines() []string {
	if m.operationsOpen {
		return []string{
			"Vault operations shortcuts",
			"",
			"↑/k, ↓/j       Scroll one line",
			"PgUp/PgDn      Scroll one visible page",
			"Home/End       Jump to first or last line",
			"r              Refresh storage and backups",
			"Esc            Return to documents",
			"q              Quit",
			"",
			"Packing, repacking, backup creation, and restore",
			"remain deliberate CLI or API operations.",
			"Press any key to close",
		}
	}
	if m.trashOpen {
		return []string{
			"Recoverable trash shortcuts",
			"",
			"↑/k, ↓/j       Move through restorable roots",
			"PgUp/PgDn      Move one visible page",
			"Home/End       Jump to first or last root",
			"Enter          Review and restore this revision",
			"r              Refresh recoverable trash",
			"Esc            Return to documents",
			"q              Quit",
			"",
			"Permanent deletion is not available here.",
			"Press any key to close",
		}
	}
	if m.jobsOpen {
		if m.jobDetail {
			return []string{
				"Background job inspection",
				"",
				"↑/k, ↓/j       Scroll one line",
				"PgUp/PgDn      Scroll one visible page",
				"Home/End       Jump to first or last line",
				"Enter/i/Esc    Return to daemon activity",
				"q              Quit",
				"",
				"Press any key to close",
			}
		}
		return []string{
			"Daemon activity shortcuts",
			"",
			"↑/k, ↓/j       Move through background jobs",
			"PgUp/PgDn      Move one visible page",
			"Home/End       Jump to first or last job",
			"Enter/i        Inspect complete job state",
			"r              Refresh background jobs",
			"Esc            Return to documents",
			"q              Quit",
			"",
			"Press any key to close",
		}
	}
	if m.historyDetail {
		return []string{
			"Audited event inspection",
			"",
			"↑/k, ↓/j       Scroll one line",
			"PgUp/PgDn      Scroll one visible page",
			"Home/End       Jump to first or last line",
			"Enter/i/Esc    Return to the event timeline",
			"q              Quit",
			"",
			"Press any key to close",
		}
	}
	if m.historyOpen {
		return []string{
			"Audited history shortcuts",
			"",
			"↑/k, ↓/j       Move through recorded events",
			"PgUp/PgDn      Move one visible page",
			"Home/End       Jump to first or last event",
			"Enter/i        Inspect the complete event",
			"n/→            Load the next older page",
			"p/←            Return to a newer page",
			"r              Reload from the newest event",
			"Esc            Return to documents",
			"q              Quit",
			"",
			"Press any key to close",
		}
	}
	return []string{
		"Keyboard shortcuts",
		"",
		"↑/k, ↓/j       Move through documents",
		"PgUp/PgDn      Move one visible page",
		"Home/End       Jump to first or last",
		"Enter/→/l      Open a directory",
		"Enter/i        Inspect complete document authority",
		"x              Review moving the node to trash",
		"T              Browse and restore recoverable trash",
		"a              Browse permanent audited history",
		"J              Inspect daemon background jobs",
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
	return parsed.UTC().Format("2006-01-02 15:04Z")
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
