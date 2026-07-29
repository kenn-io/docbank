// Package tui provides Docbank's daemon-backed terminal interface.
package tui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

const (
	maxBrowserItems = 1000
	maxSearchItems  = 1000
	maxHistoryItems = 100
	maxJobItems     = 1000
	maxTrashItems   = 1000
	nodeKindDir     = "dir"
	nodeKindFile    = "file"
)

// Backend is the bounded daemon surface needed by the TUI.
// *client.Client satisfies it without exposing a direct vault path.
type Backend interface {
	Stat(ctx context.Context, path string) (api.Node, error)
	Node(ctx context.Context, nodeID int64) (api.Node, error)
	ChildrenPage(ctx context.Context, nodeID int64, limit, offset int) (api.NodePage, error)
	Search(ctx context.Context, query string, limit int) (api.SearchReport, error)
	NodeTags(ctx context.Context, nodeID int64, limit, offset int) (api.TagPage, error)
	Jobs(ctx context.Context) ([]api.Job, error)
	TrashPage(ctx context.Context, limit, offset int) (api.TrashPage, error)
	Trash(ctx context.Context, nodeID, revision int64) (api.Node, error)
	Restore(ctx context.Context, nodeID, revision int64) (api.Node, error)
	AuditHistory(
		ctx context.Context, path string, nodeID int64, limit int, cursor string,
	) (api.AuditEventPage, error)
}

type viewMode uint8

const (
	modeBrowse viewMode = iota
	modeSearch
)

type sortField uint8

const (
	sortByRelevance sortField = iota
	sortByName
	sortBySize
	sortByModified
)

type row struct {
	node  api.Node
	path  string
	match string
	rank  int
}

type location struct {
	mode         viewMode
	directory    api.Node
	rows         []row
	total        int
	truncated    bool
	cursor       int
	offset       int
	searchQuery  string
	searchReturn *location
	sortField    sortField
	sortDesc     bool
}

type navigationKind uint8

const (
	navigationInitial navigationKind = iota
	navigationForward
	navigationRefresh
)

type directoryLoadedMsg struct {
	requestID uint64
	kind      navigationKind
	directory api.Node
	page      api.NodePage
	err       error
}

type searchLoadedMsg struct {
	requestID uint64
	query     string
	report    api.SearchReport
	err       error
}

type historyLoadedMsg struct {
	requestID uint64
	pageIndex int
	page      api.AuditEventPage
	err       error
}

type detailTagsLoadedMsg struct {
	requestID uint64
	page      api.TagPage
	err       error
}

type jobsLoadedMsg struct {
	requestID uint64
	items     []api.Job
	err       error
}

type trashLoadedMsg struct {
	requestID uint64
	page      api.TrashPage
	err       error
}

type mutationAction uint8

const (
	mutationTrash mutationAction = iota
	mutationRestore
)

type mutationConfirmation struct {
	action mutationAction
	target row
}

type mutationCompletedMsg struct {
	requestID uint64
	action    mutationAction
	target    row
	node      api.Node
	err       error
}

type spinnerTickMsg struct{}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

var errDetailNodeChanged = errors.New(
	"document changed while loading tags; close and inspect it again",
)

const spinnerInterval = 80 * time.Millisecond

// Model is a virtual-tree, search, audited-history, and recoverable-trash
// browser. Update uses a value receiver because Bubble Tea treats models as
// immutable values; small helper methods mutate only the copied value before
// it is returned.
//
//nolint:recvcheck // intentional Bubble Tea value-model pattern
type Model struct {
	ctx     context.Context
	backend Backend

	mode      viewMode
	directory api.Node
	rows      []row
	total     int
	truncated bool
	cursor    int
	offset    int
	stack     []location
	sortField sortField
	sortDesc  bool

	searchInput  textinput.Model
	searching    bool
	searchQuery  string
	searchReturn *location

	requestID           uint64
	loading             bool
	err                 error
	quitting            bool
	helpOpen            bool
	detailOpen          bool
	detailOffset        int
	detailNode          row
	detailTags          []api.Tag
	detailTagsTotal     int
	detailTagsLoading   bool
	detailTagsErr       error
	detailRequestID     uint64
	jobsOpen            bool
	jobs                []api.Job
	jobsTotal           int
	jobsRunning         int
	jobsCursor          int
	jobsOffset          int
	jobsLoading         bool
	jobsErr             error
	jobsRequestID       uint64
	jobDetail           bool
	jobDetailOffset     int
	trashOpen           bool
	trashItems          []api.Node
	trashTotal          int
	trashCursor         int
	trashOffset         int
	trashChanged        bool
	confirmation        *mutationConfirmation
	mutationRunning     bool
	notice              string
	historyOpen         bool
	historyNode         row
	historyPages        []api.AuditEventPage
	historyPage         int
	historyTotal        int
	historyCursor       int
	historyOffset       int
	historyDetail       bool
	historyDetailOffset int
	spinnerFrame        int
	spinnerActive       bool

	width  int
	height int
	styles styles
}

// New returns a model that will load the vault root on Init.
func New(ctx context.Context, backend Backend) (Model, error) {
	if ctx == nil || backend == nil {
		return Model{}, errors.New("tui requires a context and daemon client")
	}
	input := textinput.New()
	input.Prompt = "/ "
	input.Placeholder = "search names and extracted text"
	input.CharLimit = 512
	input.SetWidth(48)
	return Model{
		ctx: ctx, backend: backend, loading: true,
		searchInput: input, styles: newStyles(true), requestID: 1,
		spinnerActive: true, sortField: sortByName,
	}, nil
}

// Init starts the initial bounded root listing and asks the terminal for its
// background color so the palette stays legible in light and dark themes.
func (m Model) Init() tea.Cmd {
	return tea.Batch(tea.RequestBackgroundColor,
		m.loadDirectory(0, navigationInitial, m.requestID), spinnerTick())
}

// Update implements tea.Model.
func (m Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.searchInput.SetWidth(max(msg.Width-4, 1))
		m.clampSelection()
		m.clampDetailOffset()
		m.clampJobsSelection()
		m.clampJobDetailOffset()
		m.clampTrashSelection()
		m.clampHistorySelection()
		m.clampHistoryDetailOffset()
		return m, nil
	case tea.BackgroundColorMsg:
		m.styles = newStyles(msg.IsDark())
		return m, nil
	case directoryLoadedMsg:
		return m.applyDirectory(msg)
	case searchLoadedMsg:
		return m.applySearch(msg)
	case historyLoadedMsg:
		return m.applyHistory(msg)
	case detailTagsLoadedMsg:
		if !m.detailOpen || msg.requestID != m.detailRequestID {
			return m, nil
		}
		m.detailTagsLoading = false
		m.detailTagsErr = msg.err
		if msg.err == nil {
			m.detailTags = msg.page.Items
			m.detailTagsTotal = msg.page.Total
		}
		m.clampDetailOffset()
		return m, nil
	case jobsLoadedMsg:
		if !m.jobsOpen || msg.requestID != m.jobsRequestID {
			return m, nil
		}
		m.jobsLoading = false
		m.jobsErr = msg.err
		if msg.err == nil {
			m.jobsTotal = len(msg.items)
			m.jobsRunning = 0
			for _, job := range msg.items {
				if job.Status == "running" {
					m.jobsRunning++
				}
			}
			m.jobs = msg.items[:min(len(msg.items), maxJobItems)]
			m.clampJobsSelection()
		}
		return m, nil
	case trashLoadedMsg:
		if !m.trashOpen || msg.requestID != m.requestID {
			return m, nil
		}
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		previousID := int64(0)
		if selected, ok := m.selectedTrash(); ok {
			previousID = selected.ID
		}
		m.trashItems = msg.page.Items
		m.trashTotal = msg.page.Total
		m.trashCursor, m.trashOffset = 0, 0
		for index := range m.trashItems {
			if m.trashItems[index].ID == previousID {
				m.trashCursor = index
				break
			}
		}
		m.err = nil
		m.clampTrashSelection()
		return m, nil
	case mutationCompletedMsg:
		if msg.requestID != m.requestID || m.confirmation == nil ||
			msg.action != m.confirmation.action {
			return m, nil
		}
		m.mutationRunning = false
		m.confirmation = nil
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		if msg.action == mutationRestore {
			m.trashChanged = true
			m.notice = fmt.Sprintf("Restored %q to %q", msg.target.node.Name, msg.node.Path)
			m.removeTrashItem(msg.target.node.ID)
			m.loading = true
			m.requestID++
			return m, tea.Batch(m.startSpinner(), m.loadTrash(m.requestID))
		}
		m.notice = fmt.Sprintf("Moved %q to recoverable trash", msg.target.path)
		m.removeTrashedRows(msg.target)
		m.stack = nil
		m.searchReturn = nil
		return m.reloadCurrent()
	case spinnerTickMsg:
		if !m.loading && !m.jobsLoading && !m.mutationRunning {
			m.spinnerActive = false
			return m, nil
		}
		m.spinnerFrame = (m.spinnerFrame + 1) % len(spinnerFrames)
		return m, spinnerTick()
	case tea.KeyPressMsg:
		if m.helpOpen {
			m.helpOpen = false
			return m, nil
		}
		if m.confirmation != nil {
			return m.updateConfirmationKeys(msg)
		}
		if m.trashOpen {
			return m.updateTrashKeys(msg)
		}
		if m.jobsOpen {
			return m.updateJobsKeys(msg)
		}
		if m.historyDetail {
			return m.updateHistoryDetailKeys(msg)
		}
		if m.historyOpen {
			return m.updateHistoryKeys(msg)
		}
		if m.detailOpen {
			return m.updateDetailKeys(msg)
		}
		if m.searching {
			return m.updateSearchInput(msg)
		}
		return m.updateKeys(msg)
	default:
		return m, nil
	}
}

func (m Model) updateSearchInput(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "esc":
		m.searching = false
		m.searchInput.Blur()
		return m, nil
	case "enter":
		query := strings.TrimSpace(m.searchInput.Value())
		if query == "" {
			m.searching = false
			m.searchInput.Blur()
			return m, nil
		}
		m.searching = false
		m.searchInput.Blur()
		m.loading = true
		m.err = nil
		m.requestID++
		return m, tea.Batch(m.startSpinner(), m.loadSearch(query, m.requestID))
	default:
		var cmd tea.Cmd
		m.searchInput, cmd = m.searchInput.Update(msg)
		return m, cmd
	}
}

func (m Model) updateKeys(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	switch key {
	case "q", "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "/":
		if m.mode == modeBrowse {
			state := m.snapshot()
			m.searchReturn = &state
		}
		m.searching = true
		return m, m.searchInput.Focus()
	case "?":
		m.helpOpen = true
		return m, nil
	case "T":
		m.trashOpen = true
		m.trashItems = nil
		m.trashTotal = 0
		m.trashCursor = 0
		m.trashOffset = 0
		m.trashChanged = false
		m.loading = true
		m.err = nil
		m.notice = ""
		m.requestID++
		return m, tea.Batch(m.startSpinner(), m.loadTrash(m.requestID))
	case "x":
		selected, ok := m.selected()
		if !ok {
			return m, nil
		}
		m.err = nil
		m.notice = ""
		m.confirmation = &mutationConfirmation{action: mutationTrash, target: selected}
		return m, nil
	case "J":
		m.jobsOpen = true
		m.jobs = nil
		m.jobsTotal = 0
		m.jobsRunning = 0
		m.jobsCursor = 0
		m.jobsOffset = 0
		m.jobsLoading = true
		m.jobsErr = nil
		m.jobDetail = false
		m.jobDetailOffset = 0
		m.jobsRequestID++
		return m, tea.Batch(m.startSpinner(), m.loadJobs(m.jobsRequestID))
	case "s":
		m.cycleSortField()
		m.sortRowsPreservingSelection()
		return m, nil
	case "v":
		m.sortDesc = !m.sortDesc
		m.sortRowsPreservingSelection()
		return m, nil
	case "i":
		return m.openDetail()
	case "a":
		selected, ok := m.selected()
		if !ok {
			return m, nil
		}
		m.historyOpen = true
		m.historyNode = selected
		m.historyPages = nil
		m.historyPage = 0
		m.historyTotal = 0
		m.historyCursor = 0
		m.historyOffset = 0
		m.historyDetail = false
		m.historyDetailOffset = 0
		m.loading = true
		m.err = nil
		m.requestID++
		return m, tea.Batch(
			m.startSpinner(), m.loadHistory(selected.node.ID, "", 0, m.requestID),
		)
	case "r":
		if m.mode == modeSearch && m.searchQuery != "" {
			m.loading = true
			m.err = nil
			m.requestID++
			return m, tea.Batch(m.startSpinner(), m.loadSearch(m.searchQuery, m.requestID))
		}
		if m.directory.Path != "" {
			m.loading = true
			m.err = nil
			m.requestID++
			return m, tea.Batch(
				m.startSpinner(),
				m.loadDirectory(m.directory.ID, navigationRefresh, m.requestID),
			)
		}
	case "up", "k":
		m.moveCursor(-1)
	case "down", "j":
		m.moveCursor(1)
	case "pgup":
		m.moveCursor(-m.visibleRows())
	case "pgdown":
		m.moveCursor(m.visibleRows())
	case "home", "g":
		m.cursor = 0
		m.offset = 0
	case "end", "G":
		if len(m.rows) > 0 {
			m.cursor = len(m.rows) - 1
			m.clampSelection()
		}
	case "enter", "right", "l":
		selected, ok := m.selected()
		if ok && selected.node.Kind != nodeKindDir {
			return m.openDetail()
		}
		if ok {
			m.loading = true
			m.err = nil
			m.requestID++
			return m, tea.Batch(
				m.startSpinner(),
				m.loadDirectory(selected.node.ID, navigationForward, m.requestID),
			)
		}
	case "esc", "left", "h", "backspace":
		if m.mode == modeSearch && m.searchReturn != nil {
			m.requestID++
			m.restore(*m.searchReturn)
			return m, nil
		}
		if len(m.stack) > 0 {
			m.requestID++
			m.restore(m.stack[len(m.stack)-1])
			m.stack = m.stack[:len(m.stack)-1]
			return m, nil
		}
	}
	return m, nil
}

func (m Model) updateConfirmationKeys(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		if m.mutationRunning {
			return m, nil
		}
		m.quitting = true
		return m, tea.Quit
	case "esc":
		if !m.mutationRunning {
			m.confirmation = nil
		}
		return m, nil
	case "enter":
		if m.mutationRunning || m.confirmation == nil {
			return m, nil
		}
		m.mutationRunning = true
		m.err = nil
		m.notice = ""
		m.requestID++
		confirmation := *m.confirmation
		return m, tea.Batch(
			m.startSpinner(),
			m.runMutation(confirmation, m.requestID),
		)
	}
	return m, nil
}

func (m Model) updateTrashKeys(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "esc", "left", "h", "backspace":
		m.trashOpen = false
		m.requestID++
		m.err = nil
		if m.trashChanged {
			m.stack = nil
			m.searchReturn = nil
			m.notice = "Trash changes applied"
			m.loading = true
			return m, tea.Batch(
				m.startSpinner(),
				m.loadDirectory(0, navigationInitial, m.requestID),
			)
		}
		return m, nil
	case "r":
		m.loading = true
		m.err = nil
		m.notice = ""
		m.requestID++
		return m, tea.Batch(m.startSpinner(), m.loadTrash(m.requestID))
	case "up", "k":
		m.moveTrashCursor(-1)
	case "down", "j":
		m.moveTrashCursor(1)
	case "pgup":
		m.moveTrashCursor(-m.visibleTrashRows())
	case "pgdown":
		m.moveTrashCursor(m.visibleTrashRows())
	case "home", "g":
		m.trashCursor, m.trashOffset = 0, 0
	case "end", "G":
		if len(m.trashItems) > 0 {
			m.trashCursor = len(m.trashItems) - 1
			m.clampTrashSelection()
		}
	case "enter":
		selected, ok := m.selectedTrash()
		if !ok {
			return m, nil
		}
		m.err = nil
		m.notice = ""
		m.confirmation = &mutationConfirmation{
			action: mutationRestore,
			target: row{node: selected},
		}
	}
	return m, nil
}

func (m Model) updateJobsKeys(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.jobDetail {
		return m.updateJobDetailKeys(msg)
	}
	switch msg.String() {
	case "q", "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "?":
		m.helpOpen = true
	case "esc", "backspace", "left", "h":
		m.jobsOpen = false
		m.jobsLoading = false
		m.jobsRequestID++
	case "enter", "i":
		if _, ok := m.selectedJob(); ok {
			m.jobDetail = true
			m.jobDetailOffset = 0
		}
	case "r":
		m.jobsLoading = true
		m.jobsErr = nil
		m.jobsRequestID++
		return m, tea.Batch(m.startSpinner(), m.loadJobs(m.jobsRequestID))
	case "up", "k":
		m.moveJobsCursor(-1)
	case "down", "j":
		m.moveJobsCursor(1)
	case "pgup":
		m.moveJobsCursor(-m.visibleJobRows())
	case "pgdown":
		m.moveJobsCursor(m.visibleJobRows())
	case "home", "g":
		m.jobsCursor, m.jobsOffset = 0, 0
	case "end", "G":
		if len(m.jobs) > 0 {
			m.jobsCursor = len(m.jobs) - 1
			m.clampJobsSelection()
		}
	}
	return m, nil
}

func (m Model) updateJobDetailKeys(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "?":
		m.helpOpen = true
	case "enter", "i", "esc", "backspace", "left", "h":
		m.jobDetail = false
		m.jobDetailOffset = 0
	case "up", "k":
		m.jobDetailOffset--
		m.clampJobDetailOffset()
	case "down", "j":
		m.jobDetailOffset++
		m.clampJobDetailOffset()
	case "pgup":
		m.jobDetailOffset -= m.jobsViewportHeight()
		m.clampJobDetailOffset()
	case "pgdown":
		m.jobDetailOffset += m.jobsViewportHeight()
		m.clampJobDetailOffset()
	case "home", "g":
		m.jobDetailOffset = 0
	case "end", "G":
		m.jobDetailOffset = max(
			len(m.jobDetailLines(m.width))-m.jobsViewportHeight(), 0,
		)
	}
	return m, nil
}

func (m Model) updateHistoryKeys(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "?":
		m.helpOpen = true
		return m, nil
	case "esc", "backspace":
		m.requestID++
		m.closeHistory()
		return m, nil
	case "enter", "i":
		if _, ok := m.selectedHistoryEvent(); ok {
			m.cancelPendingHistoryLoad()
			m.historyDetail = true
			m.historyDetailOffset = 0
		}
	case "up", "k":
		m.cancelPendingHistoryLoad()
		m.moveHistoryCursor(-1)
	case "down", "j":
		m.cancelPendingHistoryLoad()
		m.moveHistoryCursor(1)
	case "pgup":
		m.cancelPendingHistoryLoad()
		m.moveHistoryCursor(-m.visibleHistoryRows())
	case "pgdown":
		m.cancelPendingHistoryLoad()
		m.moveHistoryCursor(m.visibleHistoryRows())
	case "home", "g":
		m.cancelPendingHistoryLoad()
		m.historyCursor, m.historyOffset = 0, 0
	case "end", "G":
		m.cancelPendingHistoryLoad()
		if page, ok := m.currentHistoryPage(); ok && len(page.Items) > 0 {
			m.historyCursor = len(page.Items) - 1
			m.clampHistorySelection()
		}
	case "n", "right", "l":
		return m.openOlderHistoryPage()
	case "p", "left", "h":
		m.cancelPendingHistoryLoad()
		if m.historyPage > 0 {
			m.historyPage--
			m.historyCursor, m.historyOffset = 0, 0
			m.err = nil
		}
	case "r":
		m.historyPages = nil
		m.historyPage = 0
		m.historyTotal = 0
		m.historyCursor, m.historyOffset = 0, 0
		m.loading = true
		m.err = nil
		m.requestID++
		return m, tea.Batch(
			m.startSpinner(), m.loadHistory(m.historyNode.node.ID, "", 0, m.requestID),
		)
	}
	return m, nil
}

func (m *Model) cancelPendingHistoryLoad() {
	if !m.loading {
		return
	}
	if _, ok := m.currentHistoryPage(); !ok {
		return
	}
	m.requestID++
	m.loading = false
	m.err = nil
}

func (m Model) updateHistoryDetailKeys(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "?":
		m.helpOpen = true
		return m, nil
	case "i", "enter", "esc", "left", "h", "backspace":
		m.historyDetail = false
		m.historyDetailOffset = 0
	case "up", "k":
		m.historyDetailOffset--
		m.clampHistoryDetailOffset()
	case "down", "j":
		m.historyDetailOffset++
		m.clampHistoryDetailOffset()
	case "pgup":
		m.historyDetailOffset -= m.historyViewportHeight()
		m.clampHistoryDetailOffset()
	case "pgdown":
		m.historyDetailOffset += m.historyViewportHeight()
		m.clampHistoryDetailOffset()
	case "home", "g":
		m.historyDetailOffset = 0
	case "end", "G":
		m.historyDetailOffset = max(
			len(m.historyDetailLines(m.width))-m.historyViewportHeight(), 0,
		)
	}
	return m, nil
}

func (m Model) updateDetailKeys(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "?":
		m.helpOpen = true
		return m, nil
	case "i", "enter", "esc", "left", "h", "backspace":
		m.detailOpen = false
		m.detailOffset = 0
		m.detailRequestID++
	case "up", "k":
		m.detailOffset--
		m.clampDetailOffset()
	case "down", "j":
		m.detailOffset++
		m.clampDetailOffset()
	case "pgup":
		m.detailOffset -= m.detailViewportHeight()
		m.clampDetailOffset()
	case "pgdown":
		m.detailOffset += m.detailViewportHeight()
		m.clampDetailOffset()
	case "home", "g":
		m.detailOffset = 0
	case "end", "G":
		m.detailOffset = max(len(m.expandedDetailLines(m.width))-m.detailViewportHeight(), 0)
	}
	return m, nil
}

func (m Model) openDetail() (tea.Model, tea.Cmd) {
	selected, ok := m.selected()
	if !ok {
		return m, nil
	}
	m.detailOpen = true
	m.detailOffset = 0
	m.detailNode = selected
	m.detailTags = nil
	m.detailTagsTotal = 0
	m.detailTagsLoading = true
	m.detailTagsErr = nil
	m.detailRequestID++
	return m, m.loadDetailTags(
		selected.node.ID, selected.node.Revision, m.detailRequestID,
	)
}

func (m Model) loadDetailTags(nodeID, revision int64, requestID uint64) tea.Cmd {
	ctx, backend := m.ctx, m.backend
	return func() tea.Msg {
		page, err := backend.NodeTags(ctx, nodeID, maxBrowserItems, 0)
		if err == nil {
			var current api.Node
			current, err = backend.Node(ctx, nodeID)
			if err == nil && current.Revision != revision {
				err = errDetailNodeChanged
			}
		}
		return detailTagsLoadedMsg{requestID: requestID, page: page, err: err}
	}
}

func (m Model) loadJobs(requestID uint64) tea.Cmd {
	ctx, backend := m.ctx, m.backend
	return func() tea.Msg {
		items, err := backend.Jobs(ctx)
		return jobsLoadedMsg{requestID: requestID, items: items, err: err}
	}
}

func (m Model) loadTrash(requestID uint64) tea.Cmd {
	ctx, backend := m.ctx, m.backend
	return func() tea.Msg {
		page, err := backend.TrashPage(ctx, maxTrashItems, 0)
		return trashLoadedMsg{requestID: requestID, page: page, err: err}
	}
}

func (m Model) runMutation(
	confirmation mutationConfirmation, requestID uint64,
) tea.Cmd {
	ctx, backend := m.ctx, m.backend
	return func() tea.Msg {
		var (
			node api.Node
			err  error
		)
		switch confirmation.action {
		case mutationTrash:
			node, err = backend.Trash(
				ctx, confirmation.target.node.ID, confirmation.target.node.Revision,
			)
		case mutationRestore:
			node, err = backend.Restore(
				ctx, confirmation.target.node.ID, confirmation.target.node.Revision,
			)
		default:
			err = errors.New("unknown TUI mutation")
		}
		return mutationCompletedMsg{
			requestID: requestID, action: confirmation.action,
			target: confirmation.target, node: node, err: err,
		}
	}
}

func (m Model) reloadCurrent() (tea.Model, tea.Cmd) {
	m.loading = true
	m.requestID++
	if m.mode == modeSearch && m.searchQuery != "" {
		return m, tea.Batch(
			m.startSpinner(),
			m.loadSearch(m.searchQuery, m.requestID),
		)
	}
	return m, tea.Batch(
		m.startSpinner(),
		m.loadDirectory(m.directory.ID, navigationRefresh, m.requestID),
	)
}

func (m Model) loadDirectory(
	nodeID int64, kind navigationKind, requestID uint64,
) tea.Cmd {
	ctx, backend := m.ctx, m.backend
	return func() tea.Msg {
		var (
			directory api.Node
			err       error
		)
		if nodeID == 0 {
			directory, err = backend.Stat(ctx, "/")
		} else {
			directory, err = backend.Node(ctx, nodeID)
		}
		if err != nil {
			return directoryLoadedMsg{requestID: requestID, kind: kind, err: err}
		}
		if directory.Kind != nodeKindDir || directory.TrashedAt != "" || directory.Path == "" {
			return directoryLoadedMsg{
				requestID: requestID, kind: kind,
				err: errors.New("selected node is not a live directory"),
			}
		}
		page, err := backend.ChildrenPage(ctx, directory.ID, maxBrowserItems, 0)
		return directoryLoadedMsg{
			requestID: requestID, kind: kind, directory: directory, page: page, err: err,
		}
	}
}

func (m Model) loadSearch(query string, requestID uint64) tea.Cmd {
	ctx, backend := m.ctx, m.backend
	return func() tea.Msg {
		report, err := backend.Search(ctx, query, maxSearchItems)
		return searchLoadedMsg{requestID: requestID, query: query, report: report, err: err}
	}
}

func (m Model) loadHistory(
	nodeID int64, cursor string, pageIndex int, requestID uint64,
) tea.Cmd {
	ctx, backend := m.ctx, m.backend
	return func() tea.Msg {
		page, err := backend.AuditHistory(ctx, "", nodeID, maxHistoryItems, cursor)
		return historyLoadedMsg{
			requestID: requestID, pageIndex: pageIndex, page: page, err: err,
		}
	}
}

func (m Model) applyDirectory(msg directoryLoadedMsg) (tea.Model, tea.Cmd) {
	if msg.requestID != m.requestID {
		return m, nil
	}
	m.loading = false
	if msg.err != nil {
		m.err = msg.err
		return m, nil
	}
	current := m.snapshot()
	previousSelectedID, previousOffset := int64(0), m.offset
	if selected, ok := m.selected(); ok {
		previousSelectedID = selected.node.ID
	}
	switch msg.kind {
	case navigationForward:
		if current.directory.Path != "" {
			m.stack = append(m.stack, current)
		}
	case navigationInitial, navigationRefresh:
	}
	m.mode = modeBrowse
	if m.sortField == sortByRelevance {
		m.sortField = sortByName
		m.sortDesc = false
	}
	m.directory = msg.directory
	m.rows = rowsForDirectory(msg.directory, msg.page.Items)
	m.total = msg.page.Total
	m.truncated = len(msg.page.Items) < msg.page.Total
	m.cursor, m.offset = 0, 0
	m.sortRows()
	if msg.kind == navigationRefresh {
		m.selectNode(previousSelectedID)
		m.offset = previousOffset
	}
	m.err = nil
	m.clampSelection()
	return m, nil
}

func (m Model) applySearch(msg searchLoadedMsg) (tea.Model, tea.Cmd) {
	if msg.requestID != m.requestID {
		return m, nil
	}
	m.loading = false
	if msg.err != nil {
		m.err = msg.err
		return m, nil
	}
	refreshing := m.mode == modeSearch && m.searchQuery == msg.query
	previousSelectedID, previousOffset := int64(0), m.offset
	if selected, ok := m.selected(); ok {
		previousSelectedID = selected.node.ID
	}
	m.mode = modeSearch
	m.searchQuery = msg.query
	if !refreshing {
		m.sortField = sortByRelevance
		m.sortDesc = false
	}
	m.rows = make([]row, 0, len(msg.report.Hits))
	for rank, hit := range msg.report.Hits {
		node := hit.Node
		node.Path = hit.Path
		m.rows = append(m.rows, row{node: node, path: hit.Path, match: hit.Match, rank: rank})
	}
	m.total = len(msg.report.Hits)
	m.truncated = msg.report.Truncated
	m.cursor, m.offset = 0, 0
	m.sortRows()
	if refreshing {
		m.selectNode(previousSelectedID)
		m.offset = previousOffset
	}
	m.err = nil
	m.clampSelection()
	return m, nil
}

func (m Model) applyHistory(msg historyLoadedMsg) (tea.Model, tea.Cmd) {
	if msg.requestID != m.requestID || !m.historyOpen {
		return m, nil
	}
	m.loading = false
	if msg.err != nil {
		if errors.Is(msg.err, store.ErrAuditNotEnrolled) {
			m.err = errors.New("this node is not protected by permanent audit history")
		} else {
			m.err = msg.err
		}
		return m, nil
	}
	if msg.pageIndex < len(m.historyPages) {
		m.historyPages[msg.pageIndex] = msg.page
	} else if msg.pageIndex == len(m.historyPages) {
		if msg.pageIndex == 0 {
			m.historyTotal = msg.page.Total
		} else {
			msg.page.Total = m.historyTotal
		}
		m.historyPages = append(m.historyPages, msg.page)
	} else {
		m.err = errors.New("audit history returned an unexpected page")
		return m, nil
	}
	m.historyPage = msg.pageIndex
	m.historyNode.node = msg.page.Node
	if msg.page.Path != "" {
		m.historyNode.path = msg.page.Path
	} else {
		m.historyNode.path = fmt.Sprintf("id:%d in trash", msg.page.Node.ID)
	}
	m.historyCursor, m.historyOffset = 0, 0
	m.err = nil
	m.clampHistorySelection()
	return m, nil
}

func (m Model) openOlderHistoryPage() (tea.Model, tea.Cmd) {
	if m.loading {
		return m, nil
	}
	if m.historyPage+1 < len(m.historyPages) {
		m.historyPage++
		m.historyCursor, m.historyOffset = 0, 0
		m.err = nil
		return m, nil
	}
	page, ok := m.currentHistoryPage()
	if !ok || page.NextCursor == "" {
		return m, nil
	}
	m.loading = true
	m.err = nil
	m.requestID++
	return m, tea.Batch(
		m.startSpinner(),
		m.loadHistory(m.historyNode.node.ID, page.NextCursor, len(m.historyPages), m.requestID),
	)
}

func (m *Model) closeHistory() {
	m.historyOpen = false
	m.historyNode = row{}
	m.historyPages = nil
	m.historyPage = 0
	m.historyTotal = 0
	m.historyCursor, m.historyOffset = 0, 0
	m.historyDetail = false
	m.historyDetailOffset = 0
	m.loading = false
	m.err = nil
}

func (m Model) currentHistoryPage() (api.AuditEventPage, bool) {
	if m.historyPage < 0 || m.historyPage >= len(m.historyPages) {
		return api.AuditEventPage{}, false
	}
	return m.historyPages[m.historyPage], true
}

func (m Model) selectedHistoryEvent() (api.AuditEvent, bool) {
	page, ok := m.currentHistoryPage()
	if !ok || m.historyCursor < 0 || m.historyCursor >= len(page.Items) {
		return api.AuditEvent{}, false
	}
	return page.Items[m.historyCursor], true
}

func (m Model) selectedJob() (api.Job, bool) {
	if m.jobsCursor < 0 || m.jobsCursor >= len(m.jobs) {
		return api.Job{}, false
	}
	return m.jobs[m.jobsCursor], true
}

func (m *Model) moveJobsCursor(delta int) {
	if len(m.jobs) == 0 {
		return
	}
	m.jobsCursor = min(max(m.jobsCursor+delta, 0), len(m.jobs)-1)
	m.clampJobsSelection()
}

func (m *Model) clampJobsSelection() {
	if len(m.jobs) == 0 {
		m.jobsCursor, m.jobsOffset = 0, 0
		return
	}
	m.jobsCursor = min(max(m.jobsCursor, 0), len(m.jobs)-1)
	visible := m.visibleJobRows()
	if m.jobsCursor < m.jobsOffset {
		m.jobsOffset = m.jobsCursor
	}
	if m.jobsCursor >= m.jobsOffset+visible {
		m.jobsOffset = m.jobsCursor - visible + 1
	}
	m.jobsOffset = min(max(m.jobsOffset, 0), max(len(m.jobs)-visible, 0))
}

func (m Model) visibleJobRows() int {
	return max(m.jobsViewportHeight()-2, 1)
}

func (m Model) jobsViewportHeight() int {
	return max(m.height-3, 1)
}

func (m *Model) clampJobDetailOffset() {
	maximum := max(len(m.jobDetailLines(m.width))-m.jobsViewportHeight(), 0)
	m.jobDetailOffset = min(max(m.jobDetailOffset, 0), maximum)
}

func (m Model) selectedTrash() (api.Node, bool) {
	if m.trashCursor < 0 || m.trashCursor >= len(m.trashItems) {
		return api.Node{}, false
	}
	return m.trashItems[m.trashCursor], true
}

func (m *Model) removeTrashItem(nodeID int64) {
	for index := range m.trashItems {
		if m.trashItems[index].ID != nodeID {
			continue
		}
		m.trashItems = append(m.trashItems[:index], m.trashItems[index+1:]...)
		m.trashTotal = max(m.trashTotal-1, 0)
		m.clampTrashSelection()
		return
	}
}

func (m *Model) removeTrashedRows(target row) {
	filtered := m.rows[:0]
	for _, item := range m.rows {
		if item.node.ID == target.node.ID ||
			(target.path != "" && strings.HasPrefix(item.path, target.path+"/")) {
			continue
		}
		filtered = append(filtered, item)
	}
	removed := len(m.rows) - len(filtered)
	m.rows = filtered
	m.total = max(m.total-removed, 0)
	m.clampSelection()
}

func (m *Model) moveTrashCursor(delta int) {
	if len(m.trashItems) == 0 {
		return
	}
	m.trashCursor = min(max(m.trashCursor+delta, 0), len(m.trashItems)-1)
	m.clampTrashSelection()
}

func (m *Model) clampTrashSelection() {
	if len(m.trashItems) == 0 {
		m.trashCursor, m.trashOffset = 0, 0
		return
	}
	m.trashCursor = min(max(m.trashCursor, 0), len(m.trashItems)-1)
	visible := m.visibleTrashRows()
	if m.trashCursor < m.trashOffset {
		m.trashOffset = m.trashCursor
	}
	if m.trashCursor >= m.trashOffset+visible {
		m.trashOffset = m.trashCursor - visible + 1
	}
	m.trashOffset = min(max(m.trashOffset, 0), max(len(m.trashItems)-visible, 0))
}

func (m Model) visibleTrashRows() int {
	noticeLines := 0
	if m.notice != "" {
		noticeLines = 1
	}
	return max(m.height-5-noticeLines, 1)
}

func (m *Model) moveHistoryCursor(delta int) {
	page, ok := m.currentHistoryPage()
	if !ok || len(page.Items) == 0 {
		return
	}
	m.historyCursor = min(max(m.historyCursor+delta, 0), len(page.Items)-1)
	m.clampHistorySelection()
}

func (m *Model) clampHistorySelection() {
	page, ok := m.currentHistoryPage()
	if !ok || len(page.Items) == 0 {
		m.historyCursor, m.historyOffset = 0, 0
		return
	}
	m.historyCursor = min(max(m.historyCursor, 0), len(page.Items)-1)
	visible := m.visibleHistoryRows()
	if m.historyCursor < m.historyOffset {
		m.historyOffset = m.historyCursor
	}
	if m.historyCursor >= m.historyOffset+visible {
		m.historyOffset = m.historyCursor - visible + 1
	}
	m.historyOffset = min(max(m.historyOffset, 0), max(len(page.Items)-visible, 0))
}

func (m Model) visibleHistoryRows() int {
	return max(m.historyViewportHeight()-2, 1)
}

func (m Model) historyViewportHeight() int {
	return max(m.height-3, 1)
}

func (m *Model) clampHistoryDetailOffset() {
	maximum := max(len(m.historyDetailLines(m.width))-m.historyViewportHeight(), 0)
	m.historyDetailOffset = min(max(m.historyDetailOffset, 0), maximum)
}

func (m Model) snapshot() location {
	var searchReturn *location
	if m.searchReturn != nil {
		state := *m.searchReturn
		searchReturn = &state
	}
	return location{
		mode: m.mode, directory: m.directory, rows: append([]row(nil), m.rows...),
		total: m.total, truncated: m.truncated, cursor: m.cursor, offset: m.offset,
		searchQuery: m.searchQuery, searchReturn: searchReturn,
		sortField: m.sortField, sortDesc: m.sortDesc,
	}
}

func (m *Model) restore(state location) {
	m.mode = state.mode
	m.directory = state.directory
	m.rows = append([]row(nil), state.rows...)
	m.total = state.total
	m.truncated = state.truncated
	m.cursor = state.cursor
	m.offset = state.offset
	m.searchQuery = state.searchQuery
	m.searchReturn = state.searchReturn
	m.sortField = state.sortField
	m.sortDesc = state.sortDesc
	m.loading = false
	m.err = nil
	m.clampSelection()
}

func (m *Model) startSpinner() tea.Cmd {
	if m.spinnerActive {
		return nil
	}
	m.spinnerActive = true
	m.spinnerFrame = 0
	return spinnerTick()
}

func spinnerTick() tea.Cmd {
	return tea.Tick(spinnerInterval, func(time.Time) tea.Msg { return spinnerTickMsg{} })
}

func rowsForDirectory(directory api.Node, nodes []api.Node) []row {
	rows := make([]row, 0, len(nodes))
	for rank, node := range nodes {
		node.Path = path.Join(directory.Path, node.Name)
		rows = append(rows, row{node: node, path: node.Path, rank: rank})
	}
	return rows
}

func (m Model) selected() (row, bool) {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return row{}, false
	}
	return m.rows[m.cursor], true
}

func (m *Model) moveCursor(delta int) {
	if len(m.rows) == 0 {
		return
	}
	m.cursor = min(max(m.cursor+delta, 0), len(m.rows)-1)
	m.clampSelection()
}

func (m *Model) clampSelection() {
	if len(m.rows) == 0 {
		m.cursor, m.offset = 0, 0
		return
	}
	m.cursor = min(max(m.cursor, 0), len(m.rows)-1)
	visible := m.visibleRows()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+visible {
		m.offset = m.cursor - visible + 1
	}
	m.offset = min(max(m.offset, 0), max(len(m.rows)-visible, 0))
}

func (m Model) visibleRows() int {
	headerLines := 2
	if m.searching {
		headerLines++
	}
	if m.notice != "" {
		headerLines++
	}
	bodyHeight := max(m.height-headerLines-1, 1)
	return max(bodyHeight-2, 1)
}

func (m *Model) cycleSortField() {
	switch m.sortField {
	case sortByRelevance:
		m.sortField = sortByName
	case sortByName:
		m.sortField = sortBySize
	case sortBySize:
		m.sortField = sortByModified
	case sortByModified:
		if m.mode == modeSearch {
			m.sortField = sortByRelevance
		} else {
			m.sortField = sortByName
		}
	default:
		m.sortField = sortByName
	}
	m.sortDesc = false
}

func (m *Model) sortRowsPreservingSelection() {
	selectedID := int64(0)
	if selected, ok := m.selected(); ok {
		selectedID = selected.node.ID
	}
	m.sortRows()
	m.selectNode(selectedID)
	m.clampSelection()
}

func (m *Model) sortRows() {
	sort.SliceStable(m.rows, func(left, right int) bool {
		return m.compareRows(m.rows[left], m.rows[right]) < 0
	})
}

func (m *Model) selectNode(nodeID int64) {
	if nodeID == 0 {
		return
	}
	for index := range m.rows {
		if m.rows[index].node.ID == nodeID {
			m.cursor = index
			return
		}
	}
}

func (m Model) compareRows(left, right row) int {
	if m.sortField != sortByRelevance && left.node.Kind != right.node.Kind {
		if left.node.Kind == nodeKindDir {
			return -1
		}
		if right.node.Kind == nodeKindDir {
			return 1
		}
	}
	var comparison int
	switch m.sortField {
	case sortByRelevance:
		comparison = cmp.Compare(left.rank, right.rank)
	case sortBySize:
		comparison = cmp.Compare(left.node.Size, right.node.Size)
	case sortByModified:
		comparison = cmp.Compare(left.node.ModifiedAt, right.node.ModifiedAt)
	case sortByName:
		fallthrough
	default:
		comparison = compareNames(left, right)
	}
	if comparison != 0 {
		if m.sortDesc {
			return -comparison
		}
		return comparison
	}
	return compareNames(left, right)
}

func compareNames(left, right row) int {
	leftName, rightName := strings.ToLower(left.path), strings.ToLower(right.path)
	if comparison := cmp.Compare(leftName, rightName); comparison != 0 {
		return comparison
	}
	return cmp.Compare(left.path, right.path)
}
