// Package tui provides Docbank's daemon-backed terminal interface.
package tui

import (
	"context"
	"errors"
	"path"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"go.kenn.io/docbank/internal/api"
)

const (
	maxBrowserItems = 1000
	maxSearchItems  = 1000
)

// Backend is the bounded read surface needed by the first TUI slice.
// *client.Client satisfies it without exposing a direct vault path.
type Backend interface {
	Stat(ctx context.Context, path string) (api.Node, error)
	Node(ctx context.Context, nodeID int64) (api.Node, error)
	ChildrenPage(ctx context.Context, nodeID int64, limit, offset int) (api.NodePage, error)
	Search(ctx context.Context, query string, limit int) (api.SearchReport, error)
}

type viewMode uint8

const (
	modeBrowse viewMode = iota
	modeSearch
)

type row struct {
	node  api.Node
	path  string
	match string
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

type spinnerTickMsg struct{}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const spinnerInterval = 80 * time.Millisecond

// Model is a read-only virtual-tree and search browser. Update uses a value
// receiver because Bubble Tea treats models as immutable values; small helper
// methods mutate only the copied value before it is returned.
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

	searchInput  textinput.Model
	searching    bool
	searchQuery  string
	searchReturn *location

	requestID     uint64
	loading       bool
	err           error
	quitting      bool
	helpOpen      bool
	spinnerFrame  int
	spinnerActive bool

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
		spinnerActive: true,
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
		return m, nil
	case tea.BackgroundColorMsg:
		m.styles = newStyles(msg.IsDark())
		return m, nil
	case directoryLoadedMsg:
		return m.applyDirectory(msg)
	case searchLoadedMsg:
		return m.applySearch(msg)
	case spinnerTickMsg:
		if !m.loading {
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
		if ok && selected.node.Kind == "dir" {
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
		if directory.Kind != "dir" || directory.TrashedAt != "" || directory.Path == "" {
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
	previousCursor, previousOffset := m.cursor, m.offset
	switch msg.kind {
	case navigationForward:
		if current.directory.Path != "" {
			m.stack = append(m.stack, current)
		}
	case navigationInitial, navigationRefresh:
	}
	m.mode = modeBrowse
	m.directory = msg.directory
	m.rows = rowsForDirectory(msg.directory, msg.page.Items)
	m.total = msg.page.Total
	m.truncated = len(msg.page.Items) < msg.page.Total
	m.cursor, m.offset = 0, 0
	if msg.kind == navigationRefresh {
		m.cursor, m.offset = previousCursor, previousOffset
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
	m.mode = modeSearch
	m.searchQuery = msg.query
	m.rows = make([]row, 0, len(msg.report.Hits))
	for _, hit := range msg.report.Hits {
		node := hit.Node
		node.Path = hit.Path
		m.rows = append(m.rows, row{node: node, path: hit.Path, match: hit.Match})
	}
	m.total = len(msg.report.Hits)
	m.truncated = msg.report.Truncated
	m.cursor, m.offset = 0, 0
	m.err = nil
	return m, nil
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
	for _, node := range nodes {
		node.Path = path.Join(directory.Path, node.Name)
		rows = append(rows, row{node: node, path: node.Path})
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
	bodyHeight := max(m.height-headerLines-1, 1)
	listHeight := bodyHeight
	if m.width < 72 {
		listHeight = max((bodyHeight*2)/3, 1)
	}
	return max(listHeight-2, 1)
}
