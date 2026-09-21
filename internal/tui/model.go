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
	maxBrowserItems          = 1000
	maxSearchItems           = 1000
	maxHistoryItems          = 100
	maxJobItems              = 1000
	maxTrashItems            = 1000
	maxBackupItems           = 1000
	maxProcessingSearchItems = 20
	nodeKindDir              = "dir"
	nodeKindFile             = "file"
	keyCtrlC                 = "ctrl+c"
	keyEscape                = "esc"
	keyEnter                 = "enter"
	keyTab                   = "tab"
	keyCtrlR                 = "ctrl+r"
	searchPlaceholder        = "search names and extracted text"
	naturalNames             = "names"
	naturalAuto              = "auto"
	naturalLexical           = "lexical"
	naturalSemantic          = "semantic"
	naturalHybrid            = "hybrid"
)

// Backend is the bounded daemon surface needed by the TUI.
// The CLI adapter uses generated API operations and receipt validation.
type Backend interface {
	Stat(ctx context.Context, path string) (api.Node, error)
	Node(ctx context.Context, nodeID int64) (api.Node, error)
	ChildrenPage(ctx context.Context, nodeID int64, limit, offset int) (api.NodePage, error)
	Search(ctx context.Context, query string, limit int) (api.SearchReport, error)
	ResolveDocumentSourceFence(ctx context.Context, request api.DocumentSourceFenceResolveRequest) (api.DocumentSourceFenceResolution, error)
	NodeTags(ctx context.Context, nodeID int64, limit, offset int) (api.TagPage, error)
	Jobs(ctx context.Context) ([]api.Job, error)
	Info(ctx context.Context) (api.VaultInfo, error)
	BackupList(ctx context.Context) ([]api.BackupSnapshot, error)
	ProcessingProfiles(ctx context.Context) ([]api.ProcessingProfileSummary, error)
	PlanProcessing(ctx context.Context, request api.ProcessingPlanRequest) (api.ProcessingPlan, error)
	DocumentCoverage(ctx context.Context, profile string, fence api.DocumentSourceFence) (api.CoverageReport, error)
	SearchDocuments(ctx context.Context, request api.DocumentSearchRequest) (api.DocumentSearchReport, error)
	SimilarDocuments(ctx context.Context, request api.DocumentSimilarRequest) (api.DocumentSimilarReport, error)
	StartProcessingStream(ctx context.Context, request api.StartProcessingRequest, profileFingerprint string) (ProcessingEventStream, error)
	ProcessingStatus(ctx context.Context, jobID string) (api.ProcessingStatus, error)
	RenditionForSelector(ctx context.Context, selector api.ProcessingSelector, maxBytes int64) (Rendition, error)
	TrashPage(ctx context.Context, limit, offset int) (api.TrashPage, error)
	Trash(ctx context.Context, nodeID, revision int64) (api.Node, error)
	Restore(ctx context.Context, nodeID, revision int64) (api.Node, error)
	AuditHistory(
		ctx context.Context, path string, nodeID int64, limit int, cursor string,
	) (api.AuditEventPage, error)
}

// ProcessingEventStream is the bounded live processing sequence consumed by
// the TUI. Implementations return one durable job event and one terminal event.
type ProcessingEventStream interface {
	Next() (api.ProcessingJobEvent, error)
	Close() error
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
	node        api.Node
	path        string
	match       string
	rank        int
	excerpt     string
	evidence    []string
	naturalMode string
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
	stale        bool
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

type naturalProfilesLoadedMsg struct {
	requestID uint64
	profiles  []api.ProcessingProfileSummary
	err       error
}

type naturalSearchBaseLoadedMsg struct {
	requestID   uint64
	searchID    uint64
	query       string
	naturalMode string
	request     api.DocumentSearchRequest
	report      api.DocumentSearchReport
	rows        []row
	err         error
}

type naturalSearchRerankLoadedMsg struct {
	requestID   uint64
	searchID    uint64
	naturalMode string
	report      api.DocumentSearchReport
	rows        []row
	err         error
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

type operationsInfoLoadedMsg struct {
	requestID uint64
	info      api.VaultInfo
	err       error
}

type operationsBackupsLoadedMsg struct {
	requestID uint64
	snapshots []api.BackupSnapshot
	err       error
}

type processingProfilesLoadedMsg struct {
	requestID uint64
	profiles  []api.ProcessingProfileSummary
	err       error
}

type processingPlanLoadedMsg struct {
	requestID uint64
	plan      api.ProcessingPlan
	err       error
}

type processingSimilarLoadedMsg struct {
	requestID, similarID uint64
	report               api.DocumentSimilarReport
	err                  error
}

type processingCoverageLoadedMsg struct {
	requestID uint64
	report    api.CoverageReport
	err       error
}

type processingSearchLoadedMsg struct {
	requestID uint64
	searchID  uint64
	report    api.DocumentSearchReport
	err       error
}

// Rendition is the verified Markdown and its retained artifact metadata.
type Rendition struct {
	Markdown, AttachmentID, BuildID, ArtifactID, SHA256, Completeness string
	Size                                                              int64
	Warnings                                                          []string
}
type processingStartedMsg struct {
	requestID uint64
	streamID  uint64
	event     api.ProcessingJobEvent
	stream    ProcessingEventStream
	err       error
}
type processingTerminalMsg struct {
	requestID uint64
	streamID  uint64
	event     api.ProcessingJobEvent
	err       error
}
type processingStatusLoadedMsg struct {
	requestID uint64
	runID     uint64
	status    api.ProcessingStatus
	err       error
}
type processingRenditionLoadedMsg struct {
	requestID   uint64
	renditionID uint64
	rendition   Rendition
	err         error
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

// ErrMutationUnconfirmed marks a mutation whose request was sent but whose
// terminal receipt did not arrive intact. Callers must reacquire authority
// rather than treating the operation as failed or replaying it.
var ErrMutationUnconfirmed = errors.New("mutation outcome is unconfirmed")

type mutationUnconfirmedError struct {
	action string
	cause  error
}

func (e *mutationUnconfirmedError) Error() string {
	return fmt.Sprintf(
		"%s outcome is unconfirmed; refresh before retrying: %v", e.action, e.cause,
	)
}

func (e *mutationUnconfirmedError) Unwrap() error { return e.cause }

func (e *mutationUnconfirmedError) Is(target error) bool {
	return target == ErrMutationUnconfirmed
}

// NewMutationUnconfirmedError preserves an uncertain mutation cause for the
// model while presenting an actionable operator message.
func NewMutationUnconfirmedError(action string, cause error) error {
	return &mutationUnconfirmedError{action: action, cause: cause}
}

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

	searchInput            textinput.Model
	searching              bool
	searchQuery            string
	searchReturn           *location
	naturalProfiles        []api.ProcessingProfileSummary
	naturalProfilesRequest uint64
	naturalMode            string
	naturalResultMode      string
	naturalRerank          bool
	naturalSearchID        uint64
	naturalSearchRequest   api.DocumentSearchRequest
	naturalSearchNote      string
	naturalRerankPending   bool

	requestID                uint64
	loading                  bool
	err                      error
	quitting                 bool
	helpOpen                 bool
	detailOpen               bool
	detailOffset             int
	detailNode               row
	detailTags               []api.Tag
	detailTagsTotal          int
	detailTagsLoading        bool
	detailTagsErr            error
	detailRequestID          uint64
	jobsOpen                 bool
	jobs                     []api.Job
	jobsTotal                int
	jobsRunning              int
	jobsCursor               int
	jobsOffset               int
	jobsLoading              bool
	jobsErr                  error
	jobsRequestID            uint64
	jobDetail                bool
	jobDetailOffset          int
	operationsOpen           bool
	operationsInfo           api.VaultInfo
	operationsSnapshots      []api.BackupSnapshot
	operationsTotal          int
	operationsOffset         int
	operationsInfoBusy       bool
	operationsBackupBusy     bool
	operationsStorageErr     error
	operationsBackupErr      error
	operationsRequestID      uint64
	processingOpen           bool
	processingNode           row
	processingProfiles       []api.ProcessingProfileSummary
	processingProfile        int
	processingPlan           *api.ProcessingPlan
	processingCoverage       *api.CoverageReport
	processingLoading        bool
	processingErr            error
	processingRequestID      uint64
	processingOffset         int
	processingSearching      bool
	processingSearch         string
	processingSearchBusy     bool
	processingSearchID       uint64
	processingSearchErr      error
	processingSearchReport   *api.DocumentSearchReport
	processingSimilarReport  *api.DocumentSimilarReport
	processingSimilarScope   []string
	processingSimilarPending bool
	processingSimilarBusy    bool
	processingSimilarID      uint64
	processingSimilarErr     error
	processingJob            *api.ProcessingJob
	processingStatus         *api.ProcessingStatus
	processingStatusErr      error
	processingRendition      *Rendition
	processingRenditionID    uint64
	processingMarkdown       []string
	processingRenditionErr   error
	processingConfirmation   *api.ProcessingPlan
	processingStarting       bool
	processingRunID          uint64
	processingRunErr         error
	processingStreamID       uint64
	processingCancel         context.CancelFunc
	trashOpen                bool
	trashItems               []api.Node
	trashTotal               int
	trashCursor              int
	trashOffset              int
	trashChanged             bool
	trashLoading             bool
	trashErr                 error
	trashRequestID           uint64
	confirmation             *mutationConfirmation
	mutationRunning          bool
	mutationRequestID        uint64
	notice                   string
	historyOpen              bool
	historyNode              row
	historyPages             []api.AuditEventPage
	historyPage              int
	historyTotal             int
	historyCursor            int
	historyOffset            int
	historyDetail            bool
	historyDetailOffset      int
	spinnerFrame             int
	spinnerActive            bool

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
	input.Placeholder = searchPlaceholder
	input.CharLimit = 512
	input.SetWidth(48)
	return Model{
		ctx: ctx, backend: backend, loading: true,
		searchInput: input, styles: newStyles(true), requestID: 1,
		spinnerActive: true, sortField: sortByName, naturalMode: naturalNames,
		naturalProfilesRequest: 1,
	}, nil
}

// Init starts the initial bounded root listing and asks the terminal for its
// background color so the palette stays legible in light and dark themes.
func (m Model) Init() tea.Cmd {
	return tea.Batch(tea.RequestBackgroundColor,
		m.loadDirectory(0, navigationInitial, m.requestID), m.loadNaturalProfiles(m.naturalProfilesRequest), spinnerTick())
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
		m.clampOperationsOffset()
		m.wrapProcessingRendition()
		m.clampProcessingOffset()
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
	case naturalProfilesLoadedMsg:
		if msg.requestID != m.naturalProfilesRequest {
			return m, nil
		}
		if msg.err == nil {
			m.naturalProfiles = append([]api.ProcessingProfileSummary(nil), msg.profiles...)
			if m.naturalMode == naturalNames && naturalProfileBinding(m.naturalProfiles) != "" {
				m.naturalMode = naturalAuto
			}
		}
		return m, nil
	case naturalSearchBaseLoadedMsg:
		return m.applyNaturalSearchBase(msg)
	case naturalSearchRerankLoadedMsg:
		return m.applyNaturalSearchRerank(msg)
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
	case operationsInfoLoadedMsg:
		if !m.operationsOpen || msg.requestID != m.operationsRequestID {
			return m, nil
		}
		m.operationsInfoBusy = false
		m.operationsInfo = msg.info
		m.operationsStorageErr = msg.err
		m.clampOperationsOffset()
		return m, nil
	case operationsBackupsLoadedMsg:
		if !m.operationsOpen || msg.requestID != m.operationsRequestID {
			return m, nil
		}
		m.operationsBackupBusy = false
		m.operationsBackupErr = msg.err
		if msg.err == nil {
			snapshots := append([]api.BackupSnapshot(nil), msg.snapshots...)
			sort.SliceStable(snapshots, func(left, right int) bool {
				leftTime, _ := time.Parse(time.RFC3339Nano, snapshots[left].CreatedAt)
				rightTime, _ := time.Parse(time.RFC3339Nano, snapshots[right].CreatedAt)
				if !leftTime.Equal(rightTime) {
					return leftTime.After(rightTime)
				}
				return snapshots[left].ID > snapshots[right].ID
			})
			m.operationsTotal = len(snapshots)
			m.operationsSnapshots = snapshots[:min(len(snapshots), maxBackupItems)]
		}
		m.clampOperationsOffset()
		return m, nil
	case processingProfilesLoadedMsg:
		if !m.processingOpen || msg.requestID != m.processingRequestID {
			return m, nil
		}
		if msg.err != nil {
			m.processingLoading = false
			m.processingErr = msg.err
			return m, nil
		}
		m.processingProfiles = msg.profiles
		if len(msg.profiles) == 0 {
			m.processingLoading = false
			return m, nil
		}
		m.processingProfile = min(m.processingProfile, len(msg.profiles)-1)
		return m, m.loadProcessingPlan(msg.profiles[m.processingProfile].Name, msg.requestID)
	case processingPlanLoadedMsg:
		if !m.processingOpen || msg.requestID != m.processingRequestID {
			return m, nil
		}
		if msg.err != nil {
			m.processingLoading = false
			m.processingErr = msg.err
			return m, nil
		}
		m.processingPlan = &msg.plan
		if m.processingSimilarPending {
			m.processingSimilarPending = false
			command := m.beginSimilar()
			return m, tea.Batch(m.loadProcessingCoverage(msg.plan, msg.requestID), command)
		}
		return m, m.loadProcessingCoverage(msg.plan, msg.requestID)
	case processingSimilarLoadedMsg:
		if !m.processingOpen || msg.requestID != m.processingRequestID || msg.similarID != m.processingSimilarID {
			return m, nil
		}
		m.processingSimilarBusy, m.processingSimilarErr = false, msg.err
		if msg.err == nil {
			m.processingSimilarReport = &msg.report
		}
		return m, nil
	case processingCoverageLoadedMsg:
		if !m.processingOpen || msg.requestID != m.processingRequestID {
			return m, nil
		}
		m.processingLoading = false
		m.processingErr = msg.err
		if msg.err == nil {
			m.processingCoverage = &msg.report
		}
		return m, nil
	case processingSearchLoadedMsg:
		if !m.processingOpen || msg.requestID != m.processingRequestID || msg.searchID != m.processingSearchID {
			return m, nil
		}
		m.processingSearchBusy = false
		m.processingSearchErr = msg.err
		if msg.err == nil {
			m.processingSearchReport = &msg.report
		}
		return m, nil
	case processingStartedMsg:
		if !m.processingOpen || msg.requestID != m.processingRequestID || msg.streamID != m.processingStreamID {
			if msg.stream != nil {
				_ = msg.stream.Close()
			}
			return m, nil
		}
		if msg.err != nil {
			m.processingStarting = false
			m.finishProcessingStream(msg.streamID)
			m.processingRunErr = msg.err
			return m, nil
		}
		if msg.event.Job == nil || msg.stream == nil {
			m.processingStarting = false
			m.finishProcessingStream(msg.streamID)
			m.processingRunErr = errors.New("processing stream returned no durable job")
			return m, nil
		}
		job := *msg.event.Job
		m.processingJob = &job
		return m, m.waitProcessingTerminal(msg.stream, msg.requestID, msg.streamID)
	case processingTerminalMsg:
		if !m.processingOpen || msg.requestID != m.processingRequestID || msg.streamID != m.processingStreamID {
			return m, nil
		}
		m.finishProcessingStream(msg.streamID)
		m.processingStarting = false
		m.processingRunErr = msg.err
		if msg.event.Job != nil {
			m.processingJob = msg.event.Job
		}
		m.processingStatus, m.processingStatusErr = msg.event.Status, nil
		var commands []tea.Cmd
		if msg.event.Status != nil {
			m.processingJob.EmbeddingJobIDs = msg.event.Status.EmbeddingJobIDs
		} else if m.processingJob != nil {
			commands = append(commands, m.loadProcessingStatus(m.processingJob.ID, msg.requestID))
		}
		if m.processingPlan != nil {
			m.processingLoading = true
			commands = append(commands, m.loadProcessingCoverage(*m.processingPlan, msg.requestID))
		}
		return m, tea.Batch(commands...)
	case processingStatusLoadedMsg:
		if !m.processingOpen || msg.requestID != m.processingRequestID ||
			msg.runID != m.processingRunID || m.processingJob == nil {
			return m, nil
		}
		m.processingStatus, m.processingStatusErr = nil, msg.err
		if msg.err != nil {
			return m, nil
		}
		m.processingStatus = &msg.status
		return m, nil
	case processingRenditionLoadedMsg:
		if !m.processingOpen || msg.requestID != m.processingRequestID || msg.renditionID != m.processingRenditionID {
			return m, nil
		}
		m.processingRenditionErr = msg.err
		if msg.err == nil {
			m.processingRendition = &msg.rendition
			m.wrapProcessingRendition()
		}
		return m, nil
	case trashLoadedMsg:
		if !m.trashOpen || msg.requestID != m.trashRequestID {
			return m, nil
		}
		m.trashLoading = false
		if msg.err != nil {
			m.trashErr = msg.err
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
		m.trashErr = nil
		m.clampTrashSelection()
		return m, nil
	case mutationCompletedMsg:
		if msg.requestID != m.mutationRequestID || m.confirmation == nil ||
			msg.action != m.confirmation.action {
			return m, nil
		}
		m.mutationRunning = false
		m.confirmation = nil
		if msg.err != nil {
			if errors.Is(msg.err, ErrMutationUnconfirmed) {
				m.notice = msg.err.Error()
				if msg.action == mutationRestore {
					m.trashChanged = true
					m.trashErr = msg.err
					m.trashLoading = true
					m.trashRequestID++
					return m, tea.Batch(
						m.startSpinner(), m.loadTrash(m.trashRequestID),
					)
				}
				m.invalidateLiveView()
				m.loading = true
				m.requestID++
				return m, tea.Batch(
					m.startSpinner(),
					m.loadDirectory(0, navigationInitial, m.requestID),
				)
			}
			if msg.action == mutationRestore {
				m.trashErr = msg.err
			} else {
				m.err = msg.err
			}
			return m, nil
		}
		m.err = nil
		if msg.action == mutationRestore {
			m.trashChanged = true
			m.notice = fmt.Sprintf("Restored %q to %q", msg.target.node.Name, msg.node.Path)
			m.removeTrashItem(msg.target.node.ID)
			m.trashLoading = true
			m.trashRequestID++
			return m, tea.Batch(m.startSpinner(), m.loadTrash(m.trashRequestID))
		}
		target := row{node: msg.node, path: msg.node.Path}
		m.notice = fmt.Sprintf("Moved %q to recoverable trash", target.path)
		if target.path == "" ||
			m.directory.ID == target.node.ID ||
			(m.mode == modeSearch && target.node.Kind == nodeKindDir) ||
			pathAtOrBelow(m.directory.Path, target.path) {
			m.invalidateLiveView()
			m.loading = true
			m.requestID++
			return m, tea.Batch(
				m.startSpinner(),
				m.loadDirectory(0, navigationInitial, m.requestID),
			)
		}
		m.removeTrashedRows(target)
		return m.reloadCurrent()
	case spinnerTickMsg:
		if !m.loading && !m.jobsLoading && !m.processingLoading && !m.processingStarting && !m.processingSearchBusy && !m.naturalRerankPending &&
			!m.operationsInfoBusy && !m.operationsBackupBusy &&
			!m.trashLoading && !m.mutationRunning {
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
		if m.processingConfirmation != nil {
			return m.updateProcessingConfirmationKeys(msg)
		}
		if m.confirmation != nil {
			return m.updateConfirmationKeys(msg)
		}
		if m.trashOpen {
			return m.updateTrashKeys(msg)
		}
		if m.operationsOpen {
			return m.updateOperationsKeys(msg)
		}
		if m.processingOpen {
			return m.updateProcessingKeys(msg)
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
	case keyCtrlC:
		m.quitting = true
		return m, tea.Quit
	case keyTab:
		m.cycleNaturalMode()
		return m, nil
	case keyCtrlR:
		m.toggleNaturalRerank()
		return m, nil
	case keyEscape:
		m.searching = false
		m.searchInput.Blur()
		return m, nil
	case keyEnter:
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
		m.naturalSearchID++
		m.naturalSearchRequest = api.DocumentSearchRequest{}
		m.naturalRerankPending = false
		m.naturalSearchNote = ""
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
	case keyTab:
		if m.mode == modeSearch {
			m.cycleNaturalMode()
		}
	case keyCtrlR:
		if m.mode == modeSearch {
			if m.loading {
				return m, nil
			}
			wasEnabled := m.naturalRerank
			m.toggleNaturalRerank()
			if !wasEnabled && m.naturalRerank && m.naturalSearchRequest.Query != "" &&
				len(m.naturalSearchRequest.Fence.ContentVersionIDs) > 0 && !m.naturalRerankPending {
				m.requestID++
				m.naturalSearchID++
				m.naturalRerankPending = true
				naturalMode := m.naturalResultMode
				if naturalMode == "" {
					naturalMode = m.naturalMode
				}
				return m, m.loadNaturalRerank(m.requestID, m.naturalSearchID, m.naturalSearchRequest, naturalMode)
			}
		}
	case "q", keyCtrlC:
		m.quitting = true
		return m, tea.Quit
	case "/":
		if m.mode == modeBrowse {
			state := m.snapshot()
			m.searchReturn = &state
		}
		m.searchInput.Placeholder = searchPlaceholder
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
		m.trashLoading = true
		m.trashErr = nil
		m.notice = ""
		m.trashRequestID++
		return m, tea.Batch(m.startSpinner(), m.loadTrash(m.trashRequestID))
	case "x":
		if m.loading {
			m.notice = "Wait for the current view to finish loading"
			return m, nil
		}
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
	case "O":
		m.operationsOpen = true
		m.operationsInfo = api.VaultInfo{}
		m.operationsSnapshots = nil
		m.operationsTotal = 0
		m.operationsOffset = 0
		m.operationsInfoBusy = true
		m.operationsBackupBusy = true
		m.operationsStorageErr = nil
		m.operationsBackupErr = nil
		m.operationsRequestID++
		return m, tea.Batch(
			m.startSpinner(),
			m.loadOperationsInfo(m.operationsRequestID),
			m.loadOperationsBackups(m.operationsRequestID),
		)
	case "P", "S":
		selected, ok := m.selected()
		if !ok || selected.node.Kind != nodeKindFile || selected.node.CurrentVersionID == "" {
			return m, nil
		}
		m.cancelProcessingStream()
		m.processingOpen = true
		m.processingNode = selected
		m.processingProfiles = nil
		m.processingProfile = 0
		m.processingPlan = nil
		m.processingCoverage = nil
		m.processingLoading = true
		m.processingErr = nil
		m.processingSearching = false
		m.processingSearch = ""
		m.processingSearchBusy = false
		m.processingSearchErr = nil
		m.processingSearchReport = nil
		m.clearSimilar()
		m.processingSimilarScope = nil
		seenVersions := make(map[string]bool)
		for _, item := range m.rows {
			version := item.node.CurrentVersionID
			if item.node.Kind == nodeKindFile && version != "" && !seenVersions[version] {
				m.processingSimilarScope = append(m.processingSimilarScope, version)
				seenVersions[version] = true
			}
		}
		m.processingSimilarPending = msg.String() == "S"
		m.processingJob, m.processingStatus, m.processingRendition, m.processingRenditionErr = nil, nil, nil, nil
		m.processingStatusErr = nil
		m.processingConfirmation = nil
		m.processingStarting = false
		m.processingRunErr = nil
		m.processingOffset = 0
		m.processingRequestID++
		return m, tea.Batch(m.startSpinner(), m.loadProcessingProfiles(m.processingRequestID))
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
			m.naturalSearchID++
			m.naturalSearchRequest = api.DocumentSearchRequest{}
			m.naturalRerankPending = false
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
		m.loading = true
		m.err = nil
		m.requestID++
		return m, tea.Batch(
			m.startSpinner(),
			m.loadDirectory(0, navigationInitial, m.requestID),
		)
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
	case keyEnter, "right", "l":
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
	case keyEscape, "left", "h", "backspace":
		if m.mode == modeSearch && m.searchReturn != nil {
			return m.revisit(*m.searchReturn)
		}
		if len(m.stack) > 0 {
			state := m.stack[len(m.stack)-1]
			m.stack = m.stack[:len(m.stack)-1]
			return m.revisit(state)
		}
	}
	return m, nil
}

func (m Model) updateConfirmationKeys(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", keyCtrlC:
		if m.mutationRunning {
			return m, nil
		}
		m.quitting = true
		return m, tea.Quit
	case keyEscape:
		if !m.mutationRunning {
			m.confirmation = nil
		}
		return m, nil
	case keyEnter:
		if m.mutationRunning || m.confirmation == nil {
			return m, nil
		}
		m.mutationRunning = true
		if m.confirmation.action == mutationRestore {
			m.trashErr = nil
		} else {
			m.err = nil
		}
		m.notice = ""
		m.mutationRequestID++
		confirmation := *m.confirmation
		return m, tea.Batch(
			m.startSpinner(),
			m.runMutation(confirmation, m.mutationRequestID),
		)
	}
	return m, nil
}

func (m Model) updateTrashKeys(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", keyCtrlC:
		m.quitting = true
		return m, tea.Quit
	case "?":
		m.helpOpen = true
	case keyEscape, "left", "h", "backspace":
		m.trashOpen = false
		m.trashLoading = false
		m.trashErr = nil
		m.trashRequestID++
		if m.trashChanged {
			m.invalidateLiveView()
			m.notice = "Trash changes applied"
			m.loading = true
			m.err = nil
			m.requestID++
			return m, tea.Batch(
				m.startSpinner(),
				m.loadDirectory(0, navigationInitial, m.requestID),
			)
		}
		return m, nil
	case "r":
		m.trashLoading = true
		m.trashErr = nil
		m.notice = ""
		m.trashRequestID++
		return m, tea.Batch(m.startSpinner(), m.loadTrash(m.trashRequestID))
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
	case keyEnter:
		selected, ok := m.selectedTrash()
		if !ok {
			return m, nil
		}
		m.trashErr = nil
		m.notice = ""
		m.confirmation = &mutationConfirmation{
			action: mutationRestore,
			target: row{node: selected},
		}
	}
	return m, nil
}

func (m *Model) invalidateLiveView() {
	m.mode = modeBrowse
	m.directory = api.Node{}
	m.rows = nil
	m.total = 0
	m.truncated = false
	m.cursor, m.offset = 0, 0
	m.stack = nil
	m.searchQuery = ""
	m.searchReturn = nil
}

func (m Model) updateJobsKeys(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.jobDetail {
		return m.updateJobDetailKeys(msg)
	}
	switch msg.String() {
	case "q", keyCtrlC:
		m.quitting = true
		return m, tea.Quit
	case "?":
		m.helpOpen = true
	case keyEscape, "backspace", "left", "h":
		m.jobsOpen = false
		m.jobsLoading = false
		m.jobsRequestID++
	case keyEnter, "i":
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

func (m Model) updateOperationsKeys(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", keyCtrlC:
		m.quitting = true
		return m, tea.Quit
	case "?":
		m.helpOpen = true
	case keyEscape, "backspace", "left", "h":
		m.operationsOpen = false
		m.operationsInfoBusy = false
		m.operationsBackupBusy = false
		m.operationsRequestID++
	case "r":
		m.operationsInfoBusy = true
		m.operationsBackupBusy = true
		m.operationsStorageErr = nil
		m.operationsBackupErr = nil
		m.operationsRequestID++
		return m, tea.Batch(
			m.startSpinner(),
			m.loadOperationsInfo(m.operationsRequestID),
			m.loadOperationsBackups(m.operationsRequestID),
		)
	case "up", "k":
		m.operationsOffset--
		m.clampOperationsOffset()
	case "down", "j":
		m.operationsOffset++
		m.clampOperationsOffset()
	case "pgup":
		m.operationsOffset -= m.operationsViewportHeight()
		m.clampOperationsOffset()
	case "pgdown":
		m.operationsOffset += m.operationsViewportHeight()
		m.clampOperationsOffset()
	case "home", "g":
		m.operationsOffset = 0
	case "end", "G":
		m.operationsOffset = max(
			len(m.operationsLines(m.width))-m.operationsViewportHeight(), 0,
		)
	}
	return m, nil
}

func (m Model) updateProcessingKeys(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.processingSearching {
		switch msg.String() {
		case keyCtrlC:
			m.cancelProcessingStream()
			m.quitting = true
			return m, tea.Quit
		case keyEscape:
			m.processingSearching = false
			m.searchInput.Blur()
			return m, nil
		case keyEnter:
			query := strings.TrimSpace(m.searchInput.Value())
			if query == "" || m.processingPlan == nil {
				return m, nil
			}
			m.processingSearching = false
			m.searchInput.Blur()
			m.processingSearch = query
			m.processingSearchBusy = true
			m.processingSearchErr = nil
			m.processingSearchReport = nil
			m.processingSearchID++
			return m, tea.Batch(m.startSpinner(), m.loadProcessingSearch(m.processingRequestID, query))
		default:
			var cmd tea.Cmd
			m.searchInput, cmd = m.searchInput.Update(msg)
			return m, cmd
		}
	}
	switch msg.String() {
	case "q", keyCtrlC:
		m.cancelProcessingStream()
		m.quitting = true
		return m, tea.Quit
	case "?":
		m.helpOpen = true
		return m, nil
	case keyEscape, "backspace", "left", "h":
		m.cancelProcessingStream()
		m.processingOpen = false
		m.processingStarting = false
		m.processingLoading = false
		m.processingSearchBusy = false
		m.searchInput.Placeholder = searchPlaceholder
		m.clearSimilar()
		m.processingRequestID++
		return m, nil
	case "r":
		if m.processingStarting {
			return m, nil
		}
		m.processingLoading = true
		m.processingErr = nil
		m.processingPlan = nil
		m.processingCoverage = nil
		m.clearProcessingSearch()
		m.processingOffset = 0
		m.processingRequestID++
		commands := []tea.Cmd{m.startSpinner(), m.loadProcessingProfiles(m.processingRequestID)}
		if m.processingJob != nil {
			commands = append(commands, m.loadProcessingStatus(m.processingJob.ID, m.processingRequestID))
		}
		return m, tea.Batch(commands...)
	case "[", "]":
		if m.processingStarting || len(m.processingProfiles) < 2 {
			return m, nil
		}
		m.cancelProcessingStream()
		if msg.String() == "[" {
			m.processingProfile = (m.processingProfile + len(m.processingProfiles) - 1) % len(m.processingProfiles)
		} else {
			m.processingProfile = (m.processingProfile + 1) % len(m.processingProfiles)
		}
		m.processingLoading, m.processingErr, m.processingCoverage = true, nil, nil
		m.processingPlan = nil
		m.clearProcessingSearch()
		m.processingJob, m.processingStatus, m.processingRendition, m.processingRenditionErr = nil, nil, nil, nil
		m.processingStatusErr = nil
		m.processingConfirmation = nil
		m.processingStarting = false
		m.processingRunErr = nil
		m.processingRequestID++
		return m, tea.Batch(m.startSpinner(), m.loadProcessingPlan(m.processingProfiles[m.processingProfile].Name, m.processingRequestID))
	case "S":
		command := m.beginSimilar()
		return m, command
	case "b":
		if m.processingStarting || m.processingLoading || m.processingPlan == nil {
			return m, nil
		}
		if m.processingPlan.ConsentRequired {
			plan := *m.processingPlan
			m.processingConfirmation = &plan
			return m, nil
		}
		return m, tea.Batch(m.startSpinner(), m.beginProcessing(*m.processingPlan, m.processingRequestID, false))
	case "R":
		if m.processingPlan == nil {
			return m, nil
		}
		m.processingRendition, m.processingRenditionErr = nil, nil
		m.processingRenditionID++
		return m, tea.Batch(m.startSpinner(), m.loadProcessingRendition(*m.processingPlan, m.processingRequestID))
	case "up", "k":
		m.processingOffset--
		m.clampProcessingOffset()
	case "down", "j":
		m.processingOffset++
		m.clampProcessingOffset()
	case "pgup":
		m.processingOffset -= m.processingViewportHeight()
		m.clampProcessingOffset()
	case "pgdown":
		m.processingOffset += m.processingViewportHeight()
		m.clampProcessingOffset()
	case "home", "g":
		m.processingOffset = 0
	case "end", "G":
		m.processingOffset = max(len(m.processingLines(m.width))-m.processingViewportHeight(), 0)
	case "/":
		if m.processingPlan == nil {
			return m, nil
		}
		m.processingSearching = true
		m.searchInput.SetValue("")
		m.searchInput.Placeholder = "search this exact document version"
		return m, m.searchInput.Focus()
	}
	return m, nil
}

func (m Model) updateProcessingConfirmationKeys(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", keyCtrlC:
		m.cancelProcessingStream()
		m.quitting = true
		return m, tea.Quit
	case keyEscape:
		m.processingConfirmation = nil
		return m, nil
	case keyEnter:
		if m.processingConfirmation == nil {
			return m, nil
		}
		plan := *m.processingConfirmation
		m.processingConfirmation = nil
		m.processingErr = nil
		return m, tea.Batch(m.startSpinner(), m.beginProcessing(plan, m.processingRequestID, true))
	}
	return m, nil
}

func (m Model) updateJobDetailKeys(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", keyCtrlC:
		m.quitting = true
		return m, tea.Quit
	case "?":
		m.helpOpen = true
	case keyEnter, "i", keyEscape, "backspace", "left", "h":
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
	case "q", keyCtrlC:
		m.quitting = true
		return m, tea.Quit
	case "?":
		m.helpOpen = true
		return m, nil
	case keyEscape, "backspace":
		m.requestID++
		m.closeHistory()
		return m, nil
	case keyEnter, "i":
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
	case "q", keyCtrlC:
		m.quitting = true
		return m, tea.Quit
	case "?":
		m.helpOpen = true
		return m, nil
	case "i", keyEnter, keyEscape, "left", "h", "backspace":
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
	case "q", keyCtrlC:
		m.quitting = true
		return m, tea.Quit
	case "?":
		m.helpOpen = true
		return m, nil
	case "i", keyEnter, keyEscape, "left", "h", "backspace":
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

func (m Model) loadOperationsInfo(requestID uint64) tea.Cmd {
	ctx, backend := m.ctx, m.backend
	return func() tea.Msg {
		info, err := backend.Info(ctx)
		return operationsInfoLoadedMsg{requestID: requestID, info: info, err: err}
	}
}

func (m Model) loadOperationsBackups(requestID uint64) tea.Cmd {
	ctx, backend := m.ctx, m.backend
	return func() tea.Msg {
		snapshots, err := backend.BackupList(ctx)
		return operationsBackupsLoadedMsg{
			requestID: requestID, snapshots: snapshots, err: err,
		}
	}
}

func (m *Model) clearProcessingSearch() {
	m.clearSimilar()
	m.processingSearchBusy = false
	m.processingSearchErr = nil
	m.processingSearchReport = nil
}

func (m *Model) clearSimilar() {
	m.processingSimilarID++
	m.processingSimilarBusy, m.processingSimilarPending = false, false
	m.processingSimilarErr, m.processingSimilarReport = nil, nil
}

func (m *Model) beginSimilar() tea.Cmd {
	m.clearSimilar()
	if len(m.processingSimilarScope) > 4096 {
		m.processingSimilarErr = errors.New("narrow this view to at most 4096 file versions")
		return nil
	}
	if m.processingPlan == nil || len(m.processingSimilarScope) == 0 {
		return nil
	}
	m.processingSimilarBusy = true
	ctx, backend, plan := m.ctx, m.backend, *m.processingPlan
	requestID, similarID := m.processingRequestID, m.processingSimilarID
	scope := append([]string(nil), m.processingSimilarScope...)
	return func() tea.Msg {
		report, err := backend.SimilarDocuments(ctx, api.DocumentSimilarRequest{Selector: plan.Selector, Limit: maxProcessingSearchItems,
			Fence: api.DocumentSourceFence{VaultUID: plan.VaultUID, ContentVersionIDs: scope}})
		return processingSimilarLoadedMsg{requestID: requestID, similarID: similarID, report: report, err: err}
	}
}

func (m Model) loadProcessingProfiles(requestID uint64) tea.Cmd {
	ctx, backend := m.ctx, m.backend
	return func() tea.Msg {
		profiles, err := backend.ProcessingProfiles(ctx)
		return processingProfilesLoadedMsg{requestID: requestID, profiles: profiles, err: err}
	}
}

func (m Model) loadProcessingPlan(profile string, requestID uint64) tea.Cmd {
	ctx, backend, node := m.ctx, m.backend, m.processingNode.node
	return func() tea.Msg {
		plan, err := backend.PlanProcessing(ctx, api.ProcessingPlanRequest{Selector: api.ProcessingSelector{
			NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: profile,
		}})
		return processingPlanLoadedMsg{requestID: requestID, plan: plan, err: err}
	}
}

func (m Model) loadProcessingCoverage(plan api.ProcessingPlan, requestID uint64) tea.Cmd {
	ctx, backend := m.ctx, m.backend
	return func() tea.Msg {
		report, err := backend.DocumentCoverage(ctx, plan.Selector.Profile, api.DocumentSourceFence{
			VaultUID: plan.VaultUID, ContentVersionIDs: []string{plan.Selector.ContentVersionID},
		})
		return processingCoverageLoadedMsg{requestID: requestID, report: report, err: err}
	}
}

func (m Model) loadProcessingSearch(requestID uint64, query string) tea.Cmd {
	ctx, backend, plan := m.ctx, m.backend, m.processingPlan
	searchID := m.processingSearchID
	return func() tea.Msg {
		if plan == nil {
			return processingSearchLoadedMsg{requestID: requestID, searchID: searchID, err: errors.New("processing plan is unavailable")}
		}
		report, err := backend.SearchDocuments(ctx, api.DocumentSearchRequest{
			Query: query, Mode: "auto", Limit: maxProcessingSearchItems, Profile: plan.Selector.Profile,
			Fence: api.DocumentSourceFence{
				VaultUID: plan.VaultUID, ContentVersionIDs: []string{plan.Selector.ContentVersionID},
			}, Explain: true,
		})
		return processingSearchLoadedMsg{requestID: requestID, searchID: searchID, report: report, err: err}
	}
}

func (m *Model) beginProcessing(plan api.ProcessingPlan, requestID uint64, consent bool) tea.Cmd {
	m.cancelProcessingStream()
	ctx, cancel := context.WithCancel(m.ctx)
	m.processingCancel = cancel
	m.processingStarting = true
	m.processingRunID++
	m.processingRunErr = nil
	m.processingJob, m.processingStatus, m.processingStatusErr = nil, nil, nil
	return m.startProcessingWithContext(ctx, plan, requestID, m.processingStreamID, consent)
}

func (m *Model) cancelProcessingStream() {
	if m.processingCancel != nil {
		m.processingCancel()
		m.processingCancel = nil
	}
	m.processingStreamID++
	m.processingStarting = false
}

func (m *Model) finishProcessingStream(streamID uint64) {
	if streamID != m.processingStreamID {
		return
	}
	if m.processingCancel != nil {
		m.processingCancel()
		m.processingCancel = nil
	}
	m.processingStreamID++
}

func (m Model) startProcessingWithContext(
	ctx context.Context, plan api.ProcessingPlan, requestID, streamID uint64, consent bool,
) tea.Cmd {
	backend := m.backend
	return func() tea.Msg {
		stream, err := backend.StartProcessingStream(ctx, api.StartProcessingRequest{
			Selector: plan.Selector, PlanFingerprint: plan.Fingerprint, Consent: consent,
		}, plan.ProfileFingerprint)
		if err != nil {
			return processingStartedMsg{requestID: requestID, streamID: streamID, err: err}
		}
		event, err := stream.Next()
		if err != nil {
			_ = stream.Close()
			return processingStartedMsg{requestID: requestID, streamID: streamID, err: err}
		}
		return processingStartedMsg{requestID: requestID, streamID: streamID, event: event, stream: stream}
	}
}

func (m Model) waitProcessingTerminal(
	stream ProcessingEventStream, requestID, streamID uint64,
) tea.Cmd {
	return func() tea.Msg {
		defer func() { _ = stream.Close() }()
		event, err := stream.Next()
		return processingTerminalMsg{requestID: requestID, streamID: streamID, event: event, err: err}
	}
}

func (m Model) loadProcessingStatus(jobID string, requestID uint64) tea.Cmd {
	ctx, backend := m.ctx, m.backend
	runID := m.processingRunID
	return func() tea.Msg {
		status, err := backend.ProcessingStatus(ctx, jobID)
		return processingStatusLoadedMsg{requestID: requestID, runID: runID, status: status, err: err}
	}
}

func (m Model) loadProcessingRendition(plan api.ProcessingPlan, requestID uint64) tea.Cmd {
	ctx, backend := m.ctx, m.backend
	renditionID := m.processingRenditionID
	return func() tea.Msg {
		rendition, err := backend.RenditionForSelector(ctx, plan.Selector, 256<<10)
		return processingRenditionLoadedMsg{requestID: requestID, renditionID: renditionID, rendition: rendition, err: err}
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
		m.naturalSearchID++
		m.naturalSearchRequest = api.DocumentSearchRequest{}
		m.naturalRerankPending = false
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

func (m Model) revisit(state location) (tea.Model, tea.Cmd) {
	m.requestID++
	m.restore(state)
	if !state.stale {
		return m, nil
	}
	m.loading = true
	m.rows = nil
	m.total = 0
	m.truncated = false
	m.cursor, m.offset = 0, 0
	if state.mode == modeSearch && state.searchQuery != "" {
		return m, tea.Batch(
			m.startSpinner(),
			m.loadSearch(state.searchQuery, m.requestID),
		)
	}
	return m, tea.Batch(
		m.startSpinner(),
		m.loadDirectory(state.directory.ID, navigationRefresh, m.requestID),
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
	if m.naturalMode != naturalNames && m.selectedNaturalProfile() != nil {
		return m.loadNaturalSearch(query, requestID)
	}
	ctx, backend := m.ctx, m.backend
	return func() tea.Msg {
		report, err := backend.Search(ctx, query, maxSearchItems)
		return searchLoadedMsg{requestID: requestID, query: query, report: report, err: err}
	}
}

func (m Model) loadNaturalProfiles(requestID uint64) tea.Cmd {
	ctx, backend := m.ctx, m.backend
	return func() tea.Msg {
		profiles, err := backend.ProcessingProfiles(ctx)
		return naturalProfilesLoadedMsg{requestID: requestID, profiles: profiles, err: err}
	}
}

func (m Model) loadNaturalSearch(query string, requestID uint64) tea.Cmd {
	ctx, backend, mode := m.ctx, m.backend, m.naturalMode
	profile := m.selectedNaturalProfile()
	searchID := m.naturalSearchID
	return func() tea.Msg {
		if profile == nil {
			return naturalSearchBaseLoadedMsg{requestID: requestID, searchID: searchID, query: query, naturalMode: mode,
				err: errors.New("natural search profile is unavailable")}
		}
		wireMode, binding, ok := naturalRequestMode(mode, profile)
		if !ok {
			return naturalSearchBaseLoadedMsg{requestID: requestID, searchID: searchID, query: query, naturalMode: mode,
				err: fmt.Errorf("%s search requires an embedding binding", mode)}
		}
		resolution, err := backend.ResolveDocumentSourceFence(ctx, api.DocumentSourceFenceResolveRequest{
			Filters: &api.DocumentSourceFenceFilters{},
		})
		if err != nil {
			return naturalSearchBaseLoadedMsg{requestID: requestID, searchID: searchID, query: query, naturalMode: mode, err: err}
		}
		request := api.DocumentSearchRequest{Query: query, Mode: wireMode, Limit: maxProcessingSearchItems,
			Profile: profile.Name, BindingID: binding, Fence: api.DocumentSourceFence{
				VaultUID: resolution.Fence.VaultUID, ContentVersionIDs: resolution.Fence.ContentVersionIDs,
			}, Explain: true}
		if len(resolution.Fence.ContentVersionIDs) == 0 {
			return naturalSearchBaseLoadedMsg{requestID: requestID, searchID: searchID, query: query, naturalMode: mode, request: request,
				report: api.DocumentSearchReport{}, rows: []row{}}
		}
		report, err := backend.SearchDocuments(ctx, request)
		if err != nil {
			return naturalSearchBaseLoadedMsg{requestID: requestID, searchID: searchID, query: query, naturalMode: mode, request: request, err: err}
		}
		rows, err := hydrateNaturalRows(ctx, backend, report, mode)
		return naturalSearchBaseLoadedMsg{requestID: requestID, searchID: searchID, query: query, naturalMode: mode,
			request: request, report: report, rows: rows, err: err}
	}
}

func (m Model) loadNaturalRerank(requestID, searchID uint64, request api.DocumentSearchRequest, naturalMode string) tea.Cmd {
	ctx, backend := m.ctx, m.backend
	request.Rerank = true
	return func() tea.Msg {
		report, err := backend.SearchDocuments(ctx, request)
		if err != nil {
			return naturalSearchRerankLoadedMsg{requestID: requestID, searchID: searchID, naturalMode: naturalMode, err: err}
		}
		rows, err := hydrateNaturalRows(ctx, backend, report, naturalMode)
		return naturalSearchRerankLoadedMsg{requestID: requestID, searchID: searchID, naturalMode: naturalMode, report: report, rows: rows, err: err}
	}
}

func hydrateNaturalRows(ctx context.Context, backend Backend, report api.DocumentSearchReport, mode string) ([]row, error) {
	rows := make([]row, 0, len(report.Results))
	for rank, result := range report.Results {
		node, err := backend.Node(ctx, result.NodeID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if node.Kind != nodeKindFile || node.TrashedAt != "" || node.CurrentVersionID != result.ContentVersionID {
			continue
		}
		pathValue := node.Path
		if pathValue == "" {
			pathValue = result.Path
		}
		kinds := make([]string, 0, len(result.Evidence))
		seen := make(map[string]struct{}, len(result.Evidence))
		for _, evidence := range result.Evidence {
			label := naturalEvidenceLabel(evidence.Kind)
			if _, ok := seen[label]; !ok {
				kinds = append(kinds, label)
				seen[label] = struct{}{}
			}
		}
		rows = append(rows, row{node: node, path: pathValue, rank: rank,
			excerpt: result.Excerpt, evidence: kinds, naturalMode: mode})
	}
	return rows, nil
}

func naturalEvidenceLabel(kind string) string {
	switch kind {
	case "node_name":
		return "Name"
	case "content_blob":
		return "Text"
	case "rendition_segment":
		return "Document text"
	case "embedding":
		return "Semantic"
	default:
		return "Evidence"
	}
}

func naturalModeLabel(mode string) string {
	switch mode {
	case naturalNames:
		return "Names and text"
	case naturalAuto:
		return "Auto"
	case naturalLexical:
		return "Lexical"
	case naturalSemantic:
		return "Semantic"
	case naturalHybrid:
		return "Hybrid"
	default:
		return mode
	}
}

func (m Model) selectedNaturalProfile() *api.ProcessingProfileSummary {
	profiles := append([]api.ProcessingProfileSummary(nil), m.naturalProfiles...)
	sort.Slice(profiles, func(left, right int) bool { return profiles[left].Name < profiles[right].Name })
	for index := range profiles {
		if len(profiles[index].EmbeddingBindings) > 0 {
			return &profiles[index]
		}
	}
	if len(profiles) == 0 {
		return nil
	}
	return &profiles[0]
}

func naturalProfileBinding(profiles []api.ProcessingProfileSummary) string {
	model := Model{naturalProfiles: profiles}
	profile := model.selectedNaturalProfile()
	if profile == nil || len(profile.EmbeddingBindings) == 0 {
		return ""
	}
	return profile.EmbeddingBindings[0]
}

func naturalRequestMode(mode string, profile *api.ProcessingProfileSummary) (string, string, bool) {
	binding := ""
	if profile != nil && len(profile.EmbeddingBindings) > 0 {
		binding = profile.EmbeddingBindings[0]
	}
	switch mode {
	case naturalAuto:
		if binding != "" {
			return naturalHybrid, binding, true
		}
		return naturalAuto, "", true
	case naturalLexical:
		return naturalLexical, "", true
	case naturalSemantic, naturalHybrid:
		return mode, binding, binding != ""
	default:
		return "", "", false
	}
}

func (m *Model) naturalModes() []string {
	modes := []string{naturalNames}
	if m.selectedNaturalProfile() == nil {
		return modes
	}
	modes = append(modes, naturalAuto, naturalLexical)
	if naturalProfileBinding(m.naturalProfiles) != "" {
		modes = append(modes, naturalSemantic, naturalHybrid)
	}
	return modes
}

func (m *Model) cycleNaturalMode() {
	modes := m.naturalModes()
	for index, mode := range modes {
		if mode == m.naturalMode {
			m.naturalMode = modes[(index+1)%len(modes)]
			if m.naturalMode == naturalNames {
				m.naturalSearchRequest = api.DocumentSearchRequest{}
				m.naturalRerank = false
			}
			return
		}
	}
	m.naturalMode = modes[0]
}

func (m *Model) toggleNaturalRerank() {
	profile := m.selectedNaturalProfile()
	if profile == nil || !profile.RerankingAvailable || m.naturalMode == naturalNames {
		return
	}
	m.naturalRerank = !m.naturalRerank
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
	m.naturalResultMode = ""
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
	m.naturalRerankPending = false
	m.naturalSearchRequest = api.DocumentSearchRequest{}
	m.naturalResultMode = ""
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

func (m Model) applyNaturalSearchBase(msg naturalSearchBaseLoadedMsg) (tea.Model, tea.Cmd) {
	if msg.requestID != m.requestID || msg.searchID != m.naturalSearchID {
		return m, nil
	}
	m.loading = false
	m.naturalRerankPending = false
	if msg.err != nil {
		m.naturalMode = naturalNames
		m.naturalRerank = false
		m.naturalSearchRequest = api.DocumentSearchRequest{}
		m.naturalResultMode = ""
		m.naturalSearchNote = "Natural-language search unavailable: " + msg.err.Error() + ". Showing Names and text."
		m.loading = true
		return m, m.loadSearch(msg.query, msg.requestID)
	}
	m.applyNaturalRows(msg.query, msg.rows, msg.report.Truncated)
	m.naturalResultMode = msg.naturalMode
	if len(msg.request.Fence.ContentVersionIDs) == 0 {
		m.naturalSearchRequest = api.DocumentSearchRequest{}
		m.naturalSearchNote = naturalSearchReportNote(msg.report)
		return m, nil
	}
	m.naturalSearchRequest = msg.request
	m.naturalSearchNote = naturalSearchReportNote(msg.report)
	profile := m.selectedNaturalProfile()
	if m.naturalRerank && profile != nil && profile.RerankingAvailable {
		m.naturalRerankPending = true
		return m, m.loadNaturalRerank(msg.requestID, msg.searchID, msg.request, msg.naturalMode)
	}
	return m, nil
}

func (m Model) applyNaturalSearchRerank(msg naturalSearchRerankLoadedMsg) (tea.Model, tea.Cmd) {
	if msg.requestID != m.requestID || msg.searchID != m.naturalSearchID {
		return m, nil
	}
	m.naturalRerankPending = false
	if !m.naturalRerank {
		return m, nil
	}
	if msg.err != nil {
		cause := naturalSearchCause(msg.err.Error())
		if cause == "" {
			cause = "unknown cause"
		}
		m.naturalSearchNote = "Reranking unavailable (" + cause + "). Base results remain."
		return m, nil
	}
	if msg.report.Reranking == nil {
		m.naturalSearchNote = naturalRerankNote("failed", "missing receipt")
		return m, nil
	}
	if msg.report.Reranking.Outcome != "applied" {
		m.naturalSearchNote = naturalRerankNote(msg.report.Reranking.Outcome, msg.report.Reranking.Cause)
		return m, nil
	}
	m.naturalResultMode = msg.naturalMode
	m.applyNaturalRows(m.searchQuery, msg.rows, msg.report.Truncated)
	m.naturalSearchNote = naturalSearchReportNote(msg.report)
	return m, nil
}

func (m *Model) applyNaturalRows(query string, rows []row, truncated bool) {
	previousSelectedID, previousOffset := int64(0), m.offset
	if selected, ok := m.selected(); ok {
		previousSelectedID = selected.node.ID
	}
	m.mode = modeSearch
	m.searchQuery = query
	m.sortField = sortByRelevance
	m.sortDesc = false
	m.rows = append([]row(nil), rows...)
	m.total = len(rows)
	m.truncated = truncated
	m.cursor, m.offset = 0, 0
	m.sortRows()
	m.selectNode(previousSelectedID)
	m.offset = previousOffset
	m.err = nil
	m.clampSelection()
}

func naturalSearchReportNote(report api.DocumentSearchReport) string {
	if len(report.Degradations) == 0 {
		return ""
	}
	causes := make([]string, 0, len(report.Degradations))
	for _, degradation := range report.Degradations {
		causes = append(causes, naturalSearchCause(degradation))
	}
	return "Search note: " + strings.Join(causes, "; ")
}

func naturalSearchCause(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > 128 {
		return string(runes[:128])
	}
	return value
}

func naturalRerankNote(outcome, cause string) string {
	cause = naturalSearchCause(cause)
	switch outcome {
	case "skipped":
		if cause == "" {
			return "Reranking skipped. Base results remain."
		}
		return "Reranking skipped (" + cause + "). Base results remain."
	case "degraded":
		if cause == "" {
			cause = "provider unavailable"
		}
		return "Reranking degraded (" + cause + "). Base results remain."
	default:
		if cause == "" {
			cause = "unknown cause"
		}
		return "Reranking failed (" + cause + "). Base results remain."
	}
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
	var removed int
	m.rows, removed = withoutTrashedRows(m.rows, target)
	m.total = max(m.total-removed, 0)
	m.invalidateSavedLocations(target)
	m.clampSelection()
}

func (m *Model) invalidateSavedLocations(target row) {
	stack := m.stack[:0]
	for _, state := range m.stack {
		sanitized, ok := sanitizeLocationAfterTrash(state, target)
		if ok {
			stack = append(stack, sanitized)
		}
	}
	m.stack = stack
	if m.searchReturn == nil {
		return
	}
	sanitized, ok := sanitizeLocationAfterTrash(*m.searchReturn, target)
	if ok {
		m.searchReturn = &sanitized
		return
	}
	if len(m.stack) > 0 {
		sanitized = m.stack[len(m.stack)-1]
		m.stack = m.stack[:len(m.stack)-1]
		m.searchReturn = &sanitized
		return
	}
	m.searchReturn = &location{
		mode: modeBrowse, directory: api.Node{Kind: nodeKindDir, Path: "/"},
		sortField: sortByName, stale: true,
	}
}

func sanitizeLocationAfterTrash(state location, target row) (location, bool) {
	if pathAtOrBelow(state.directory.Path, target.path) {
		return location{}, false
	}
	state.stale = true
	if state.searchReturn != nil {
		nested, ok := sanitizeLocationAfterTrash(*state.searchReturn, target)
		if ok {
			state.searchReturn = &nested
		} else {
			state.searchReturn = nil
		}
	}
	return state, true
}

func pathAtOrBelow(candidate, ancestor string) bool {
	return ancestor != "" &&
		(candidate == ancestor || strings.HasPrefix(candidate, ancestor+"/"))
}

func withoutTrashedRows(rows []row, target row) ([]row, int) {
	filtered := rows[:0]
	for _, item := range rows {
		if item.node.ID == target.node.ID ||
			(target.path != "" && strings.HasPrefix(item.path, target.path+"/")) {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered, len(rows) - len(filtered)
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

func (m Model) operationsViewportHeight() int {
	return m.bodyViewportHeight()
}

func (m Model) processingViewportHeight() int {
	return m.bodyViewportHeight()
}

func (m *Model) clampProcessingOffset() {
	maximum := max(len(m.processingLines(m.width))-m.processingViewportHeight(), 0)
	m.processingOffset = min(max(m.processingOffset, 0), maximum)
}

func (m *Model) clampOperationsOffset() {
	maximum := max(
		len(m.operationsLines(m.width))-m.operationsViewportHeight(), 0,
	)
	m.operationsOffset = min(max(m.operationsOffset, 0), maximum)
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
	if m.offset < 0 {
		m.offset = 0
	}
	if !m.rowFitsInViewport(m.offset, m.cursor) {
		m.offset = m.cursor
	}
	if m.offset >= len(m.rows) {
		m.offset = len(m.rows) - 1
	}
}

func (m Model) visibleRows() int {
	available := max(m.bodyViewportHeight()-2, 1)
	if m.mode != modeSearch {
		return available
	}
	used, rows := 0, 0
	for index := m.offset; index < len(m.rows); index++ {
		rowLines := 1 + len(m.naturalWhyLines(m.rows[index], m.width))
		if used+rowLines > available && rows > 0 {
			break
		}
		used += min(rowLines, available-used)
		rows++
		if used >= available {
			break
		}
	}
	return max(rows, 1)
}

func (m Model) rowFitsInViewport(offset, cursor int) bool {
	if offset < 0 || offset >= len(m.rows) || cursor < offset {
		return false
	}
	available := max(m.bodyViewportHeight()-2, 1)
	used := 0
	for index := offset; index <= cursor; index++ {
		rowLines := 1 + len(m.naturalWhyLines(m.rows[index], m.width))
		if used+rowLines > available && index == cursor {
			return used > 0
		}
		used += min(rowLines, available-used)
		if used >= available && index < cursor {
			return false
		}
	}
	return true
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
