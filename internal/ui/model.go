package ui

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/techdufus/openkanban/internal/agent"
	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/daemonclient"
	"github.com/techdufus/openkanban/internal/git"
	"github.com/techdufus/openkanban/internal/project"
	"github.com/techdufus/openkanban/internal/terminal"
	"github.com/techdufus/openkanban/internal/update"
)

const agentPortBase = 4097

type Mode string

const (
	ModeNormal        Mode = "NORMAL"
	ModeInsert        Mode = "INSERT"
	ModeCommand       Mode = "COMMAND"
	ModeHelp          Mode = "HELP"
	ModeConfirm       Mode = "CONFIRM"
	ModeConfirmExit   Mode = "CONFIRM_EXIT"
	ModeCreateTicket  Mode = "CREATE"
	ModeEditTicket    Mode = "EDIT"
	ModeAgentView     Mode = "AGENT"
	ModeSettings      Mode = "SETTINGS"
	ModeShuttingDown  Mode = "SHUTTING_DOWN"
	ModeSpawning      Mode = "SPAWNING"
	ModeFilter        Mode = "FILTER"
	ModeCreateProject Mode = "NEW_PROJECT"
)

// SortMode controls the order tickets appear within each column.
// SortDefault preserves the store's natural (map-iteration) order so
// existing behavior is unchanged until the user opts in with `o`.
type SortMode string

const (
	SortDefault  SortMode = ""
	SortName     SortMode = "name"
	SortAge      SortMode = "age"
	SortPriority SortMode = "priority"
)

// sortModes is the cycle order the `o` keybinding walks. Kept here so
// the cycle and the label/help text stay in sync.
var sortModes = []SortMode{SortDefault, SortName, SortAge, SortPriority}

func nextSortMode(s SortMode) SortMode {
	for i, m := range sortModes {
		if m == s {
			return sortModes[(i+1)%len(sortModes)]
		}
	}
	return SortDefault
}

func sortModeLabel(s SortMode) string {
	switch s {
	case SortName:
		return "name (A→Z)"
	case SortAge:
		return "age (newest first)"
	case SortPriority:
		return "priority (highest first)"
	default:
		return "default"
	}
}

// SessionFilter narrows the board to tickets matching a session-state
// predicate. "Open" means the ticket has a live daemon session
// (daemonOwned); "Waiting" tightens that to sessions where the agent is
// blocked on user input (AgentStatus == AgentWaiting). Session-only
// (not persisted) — same lifetime convention as sortMode.
type SessionFilter string

const (
	SessionFilterAll     SessionFilter = ""
	SessionFilterOpen    SessionFilter = "open"
	SessionFilterWaiting SessionFilter = "waiting"
)

var sessionFilters = []SessionFilter{SessionFilterAll, SessionFilterOpen, SessionFilterWaiting}

func nextSessionFilter(f SessionFilter) SessionFilter {
	for i, sf := range sessionFilters {
		if sf == f {
			return sessionFilters[(i+1)%len(sessionFilters)]
		}
	}
	return SessionFilterAll
}

func sessionFilterLabel(f SessionFilter) string {
	switch f {
	case SessionFilterOpen:
		return "open sessions"
	case SessionFilterWaiting:
		return "waiting sessions"
	default:
		return "all"
	}
}

const (
	minColumnWidth = 20
	columnOverhead = 5

	// ticketHeight is the fallback estimate of a ticket card's rendered
	// height in rows, used by ensureTicketVisible and hitTestTicket only
	// when the per-render Model.columnTicketHeights cache is empty (e.g.
	// before the first View() call after a window-size change). The actual
	// rendered height varies (8 for a single-row title, 9 when the title
	// wraps to 2 rows) and is measured by renderColumn via lipgloss.Height.
	ticketHeight       = 8
	columnHeaderHeight = 3
	// indicatorReserveRows reserves vertical space for the "▲ N more" and
	// "▼ N more" overflow indicators rendered inside a column. Kept as a
	// constant for the small handful of callers (e.g. ensureTicketVisible
	// fallback) that still want a worst-case reservation without walking
	// columnTicketHeights.
	indicatorReserveRows = 2

	formFieldTitle       = 0
	formFieldDescription = 1
	formFieldBranch      = 2
	formFieldLabels      = 3
	formFieldPriority    = 4
	formFieldWorktree    = 5
	formFieldAgent       = 6
	formFieldBlockedBy   = 7
	formFieldProject     = 8
)

type choiceItem struct {
	Key   rune
	Label string
	Fn    func() tea.Cmd
}

// spawnPlan is the user's chosen direction when openkanban detects a
// stale-brief situation (prior session exists AND the merge would
// change the brief on disk). It is passed by value into prepareSpawnWith
// so the snapshot is unambiguous across the tea.Cmd goroutine boundary —
// do NOT convert this to a pointer.
type spawnPlan struct {
	SkipMerge          bool // option 'n' — don't write the brief
	ForceFresh         bool // option 'd' — caller has already cleared AgentSpawnedAt
	InjectResumeNotice bool // option 'u' — append a "brief updated" message after --continue
}

type Model struct {
	config *config.Config
	theme  config.Theme
	colors uiColors

	globalStore      *project.GlobalTicketStore
	projectRegistry  *project.ProjectRegistry
	columns          []board.Column
	filterProjectIDs map[string]bool

	worktreeMgrs   map[string]*git.WorktreeManager
	agentMgr       *agent.Manager
	opencodeServer *agent.OpencodeServer

	mode          Mode
	activeColumn  int
	activeTicket  int
	width         int
	height        int
	spinner       spinner.Model
	scrollOffset  int
	columnOffsets []int

	dragging         bool
	dragSourceColumn int
	dragSourceTicket int
	dragTargetColumn int

	hoverColumn int
	hoverTicket int

	lastClickTime   time.Time
	lastClickColumn int
	lastClickTicket int

	columnTickets [][]*board.Ticket

	// columnTicketHeights mirrors columnTickets and holds the measured
	// rendered height (lipgloss.Height) of every ticket in every column at
	// the current width. Populated by renderColumn each render. Consumed by
	// ensureTicketVisible and hitTestTicket to translate between ticket
	// index and vertical-pixel space when ticket heights vary (e.g. 2-line
	// title wraps). May be nil/short before the first render — callers
	// fall back to the ticketHeight constant in that case.
	columnTicketHeights [][]int

	// sortMode is the user-selected sort applied to each column in
	// refreshColumnTickets. Session-only (not persisted); SortDefault
	// preserves the store's natural order.
	sortMode SortMode

	// sessionFilter narrows the board to tickets with a live daemon
	// session ("open") or waiting-for-input sessions ("waiting"). Same
	// session-only lifetime as sortMode; cycled with 'w'.
	sessionFilter SessionFilter

	// alwaysShowWorking, when true, exempts daemon-owned ("open")
	// sessions from the project and text-search filters so working
	// sessions remain visible across project narrowing. The session
	// filter ('w') still applies on top. Session-only lifetime;
	// toggled with 'W'.
	alwaysShowWorking bool

	showHelp    bool
	showConfirm bool
	confirmMsg  string
	confirmFn   func() tea.Cmd

	showChoice bool
	choiceMsg  string
	choices    []choiceItem

	titleInput         textinput.Model
	descInput          textarea.Model
	branchInput        textinput.Model
	labelsInput        textinput.Model
	ticketPriority     int
	ticketUseWorktree  bool
	ticketAgent        string
	agentListIndex     int
	projectInput       textinput.Model
	ticketFormField    int
	editingTicketID    board.TicketID
	branchLocked       bool
	agentLocked        bool
	selectedProject    *project.Project
	projectListIndex   int
	showAddProjectForm bool
	addProjectPath     textinput.Model

	blockerCandidates  []*board.Ticket
	selectedBlockers   map[board.TicketID]bool
	blockerListIndex   int
	blockerFilterInput textinput.Model

	formScrollOffset int
	formFieldLines   map[int]int

	notification string
	notifyTime   time.Time

	panes          map[board.TicketID]*daemonclient.PaneView
	focusedPane    board.TicketID
	statusDetector *agent.StatusDetector

	// cycleAttachPrompt is set when the user has used Ctrl+] / Ctrl+\
	// to cycle focus to a peer session that this TUI is not yet
	// attached to. View() renders an "Enter to attach" modal over the
	// agent view; handleAgentViewMode swallows all keys until the user
	// confirms (Enter), cancels (Esc → board), or cycles further. The
	// modal exists specifically to absorb the user's "I want to switch
	// to this session" keystroke so it doesn't get eaten by the
	// AttachFirstMsg handshake the first time they type into the pane.
	cycleAttachPrompt bool

	// daemonClient is the long-lived control connection to openkanbankd.
	// nil when the daemon couldn't be reached at startup — every call
	// site MUST nil-check before use (the TUI degrades to a no-spawn
	// state in that case). Reconstructing the client mid-session is the
	// job of a future PR; this PR is a single-shot New() at startup.
	daemonClient *daemonclient.Client

	// daemonEvents is the push channel returned by
	// daemonClient.Subscribe. nil when the daemon is unreachable or
	// the subscription has ended. daemonUnsub is its cancel func.
	// daemonConnected reflects whether the subscription is currently
	// live — the status-file poll honors this flag to enforce the
	// daemon-wins precedence rule.
	daemonEvents    <-chan daemon.SessionEvent
	daemonUnsub     func()
	daemonConnected atomic.Bool

	// daemonOwned tracks tickets that currently have a live daemon
	// session — populated from the startup List() and maintained by
	// handleDaemonSessionEvent on "started" / "exited". Used by the
	// file-poll precedence rule so daemon-pushed AgentStatus wins
	// regardless of whether THIS TUI is the one with an attached
	// PaneView. Without this, a second TUI watching a session spawned
	// elsewhere falls back to the on-disk status file and clobbers the
	// daemon-pushed "working" with the file's stale "idle".
	daemonOwned map[board.TicketID]struct{}

	// daemonViewing counts how many TUI clients are currently focused on
	// each ticket's daemon session (in ModeAgentView) — a ticket renders
	// the "viewing" indicator when its count is >0. Populated at startup
	// from SessionInfo.ViewerCount and maintained by daemon-pushed
	// "viewing" / "unviewing" SessionEvents (driven by SetViewing RPC
	// calls from every connected TUI's mode transitions). Reset on
	// daemon disconnect.
	daemonViewing map[board.TicketID]int

	// lastPTYActivity tracks the most recent PTY-output timestamp per
	// ticket, populated from SessionEvent.LastActivityAt on every event
	// the daemon emits. The status detector consults this to override a
	// stale file-based "waiting" → "working": Claude Code emits no hook
	// between Notification (permission granted) and PostToolUse (tool
	// finished), so during a long-running tool the file says "waiting"
	// for the whole duration even though the agent is producing output.
	// Cleared on "exited" so the map can't grow unboundedly across the
	// TUI's lifetime.
	lastPTYActivity map[board.TicketID]time.Time

	// viewingSessionID is the daemon SessionID this TUI most recently
	// told the daemon it was viewing (via SetViewing(true)). Used by
	// reconcileViewing to emit SetViewing(prev,false) / SetViewing(new,
	// true) only when the TUI's current view target actually changes —
	// dispatched at the end of every Update so any mode/focusedPane
	// transition gets caught without scattering RPC calls through every
	// case branch.
	viewingSessionID string

	// guardAPI is the subset of daemonclient.Client used by the exit
	// guard (PrepareExit / Kill / ClientID). Held as an interface so
	// tests can substitute a fake without standing up a real daemon. Set
	// from daemonClient in NewModel; nil when the daemon is unreachable.
	guardAPI daemonGuardAPI

	// confirmExit carries modal state for ModeConfirmExit. Populated by
	// handlePrepareExitResult when the user requests quit, cleared when
	// the modal exits (either to ModeNormal on cancel or to tea.Quit).
	confirmExit confirmExitState

	spawningTicketID board.TicketID
	spawningAgent    string

	settingsIndex   int
	settingsEditing bool
	settingsInput   textinput.Model
	themeListIndex  int

	filterInput textinput.Model
	filterQuery string

	sidebarVisible bool
	sidebarFocused bool
	sidebarIndex   int
	sidebarWidth   int

	updateChecker *update.Checker

	// binaryStaleNotified records whether the user has already been
	// shown the "binary has been updated on disk" notification for the
	// current stale-transition. Set when the periodic check first
	// detects update.BinaryStale() == true; reset back to false if the
	// check returns false (defensive — mtime can't go backwards in
	// practice, but rebuilding atop the running binary while it's open
	// could in theory drop us out of stale, and we want a clean
	// re-trigger if it ever does).
	binaryStaleNotified bool

	// recentSelfWrites tracks (mtime, size, deadline) per path for
	// suppressing fsnotify echoes of the TUI's own SaveTicket calls.
	// See internal/ui/reload.go.
	recentSelfWrites map[string]selfWriteRecord

	// lastWindowTitle is the most recent value passed to
	// tea.SetWindowTitle, used to dedupe redundant title updates.
	// See computeWindowTitle / maybeSetWindowTitle.
	lastWindowTitle string
}

func NewModel(cfg *config.Config, globalStore *project.GlobalTicketStore, projectRegistry *project.ProjectRegistry, agentMgr *agent.Manager, opencodeServer *agent.OpencodeServer, filterProjectID string, updateChecker *update.Checker, daemonClient *daemonclient.Client) *Model {
	ti := textinput.New()
	ti.Placeholder = "Enter ticket title..."
	ti.CharLimit = 100
	ti.Width = 40

	di := textarea.New()
	di.Placeholder = "Optional description..."
	di.CharLimit = 0
	di.SetWidth(40)
	di.SetHeight(4)
	di.ShowLineNumbers = false

	bi := textinput.New()
	bi.Placeholder = "Auto-generated from title..."
	bi.CharLimit = 100
	bi.Width = 40

	li := textinput.New()
	li.Placeholder = "bug, urgent, frontend (comma-separated)"
	li.CharLimit = 200
	li.Width = 40

	pi := textinput.New()
	pi.Placeholder = "Select project..."
	pi.CharLimit = 100
	pi.Width = 40

	si := textinput.New()
	si.CharLimit = 200
	si.Width = 40

	fi := textinput.New()
	fi.Placeholder = "Search tickets..."
	fi.CharLimit = 100
	fi.Width = 30

	ap := textinput.New()
	ap.Placeholder = "/path/to/repository"
	ap.CharLimit = 256
	ap.Width = 40

	bf := textinput.New()
	bf.Placeholder = "Filter tickets..."
	bf.CharLimit = 100
	bf.Width = 30

	sp := spinner.New()
	sp.Spinner = spinner.Dot

	worktreeMgrs := make(map[string]*git.WorktreeManager)
	for _, p := range globalStore.Projects() {
		worktreeMgrs[p.ID] = git.NewWorktreeManager(p)
	}

	var selectedProject *project.Project
	projects := globalStore.Projects()
	if len(projects) > 0 {
		if filterProjectID != "" {
			selectedProject = globalStore.GetProject(filterProjectID)
		}
		if selectedProject == nil {
			selectedProject = projects[0]
		}
	}

	theme := cfg.GetTheme()
	m := &Model{
		config:             cfg,
		theme:              theme,
		colors:             newUIColors(theme),
		globalStore:        globalStore,
		projectRegistry:    projectRegistry,
		columns:            board.DefaultColumns(),
		filterProjectIDs:   make(map[string]bool),
		worktreeMgrs:       worktreeMgrs,
		agentMgr:           agentMgr,
		opencodeServer:     opencodeServer,
		mode:               ModeNormal,
		titleInput:         ti,
		descInput:          di,
		branchInput:        bi,
		labelsInput:        li,
		ticketPriority:     3,
		projectInput:       pi,
		settingsInput:      si,
		filterInput:        fi,
		addProjectPath:     ap,
		blockerFilterInput: bf,
		selectedBlockers:   make(map[board.TicketID]bool),
		formFieldLines:     make(map[int]int),
		spinner:            sp,
		panes:              make(map[board.TicketID]*daemonclient.PaneView),
		daemonOwned:        make(map[board.TicketID]struct{}),
		daemonViewing:      make(map[board.TicketID]int),
		lastPTYActivity:    make(map[board.TicketID]time.Time),
		statusDetector:     agent.NewStatusDetector(),
		selectedProject:    selectedProject,
		sidebarVisible:     cfg.UI.SidebarVisible,
		sidebarWidth:       24,
		hoverColumn:        -1,
		hoverTicket:        -1,
		updateChecker:      updateChecker,
		daemonClient:       daemonClient,
	}
	if daemonClient != nil {
		m.guardAPI = daemonClient
	}
	if filterProjectID != "" {
		m.filterProjectIDs[filterProjectID] = true
	}

	// Startup reconciliation. Replaces the old unconditional
	// "wipe all AgentStatus" pass: the daemon may already own live
	// sessions from a previous TUI run (or a sibling TUI), so blindly
	// resetting status would lie about the world.
	//
	// Algorithm:
	//   1. If we have a daemon client, ask it for the current set of
	//      sessions. For every ticket whose ID matches a live session,
	//      construct a PaneView in Unattached state and keep any status
	//      we can read from the on-disk marker (until PR9 wires push
	//      events). For every ticket NOT owned by the daemon, wipe any
	//      stale "working/waiting/etc" status as before.
	//   2. If the daemon is unreachable, fall back to the legacy wipe
	//      so the UI doesn't show ghost-working statuses.
	ownedByDaemon := map[board.TicketID]daemon.SessionInfo{}
	if daemonClient != nil {
		listCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		resp, err := daemonClient.List(listCtx)
		cancel()
		if err == nil {
			for _, s := range resp.Sessions {
				ownedByDaemon[board.TicketID(s.TicketID)] = s
				m.daemonOwned[board.TicketID(s.TicketID)] = struct{}{}
				if s.ViewerCount > 0 {
					m.daemonViewing[board.TicketID(s.TicketID)] = s.ViewerCount
				}
			}
		} else {
			log.Printf("openkanban: daemon list failed at startup: %v", err)
		}
	}

	for _, ticket := range globalStore.All() {
		if info, ok := ownedByDaemon[ticket.ID]; ok {
			pv := daemonclient.NewPaneView(daemonClient, string(ticket.ID), info.SessionID, &info)
			if info.Workdir != "" {
				pv.SetWorkdir(info.Workdir)
			}
			if info.SessionName != "" {
				pv.SetSessionName(info.SessionName)
			}
			m.panes[ticket.ID] = pv
			// Best-effort status read from the existing on-disk marker.
			// PR9 will replace this with push events.
			if st := agent.ReadAgentStatus(info.SessionName); st != board.AgentNone {
				if ticket.AgentStatus != st {
					ticket.AgentStatus = st
					globalStore.Save(ticket)
				}
			}
			continue
		}
		if ticket.AgentStatus != board.AgentNone {
			ticket.AgentStatus = board.AgentNone
			globalStore.Save(ticket)
		}
	}

	m.refreshColumnTickets()

	// Subscribe to daemon push events so status changes that happen in
	// OTHER TUIs (or via daemon-internal pane exits) reach this model.
	// Subscribe in NewModel (rather than Init) so the channel is alive
	// before the first Update tick — Init only emits the tea.Cmd that
	// arms the listener.
	if daemonClient != nil {
		events, unsub, _ := subscribeDaemonEvents(daemonClient)
		if events != nil {
			m.daemonEvents = events
			m.daemonUnsub = unsub
			m.daemonConnected.Store(true)
			log.Printf("openkanban model: daemon Subscribe ok; push channel armed")
		} else {
			log.Printf("openkanban model: daemon Subscribe returned nil channel; push events disabled")
		}
	}

	return m
}

func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{
		tickAgentStatus(m.agentMgr.StatusPollInterval()),
		m.spinner.Tick,
		m.checkForUpdates(),
		m.maybeSetWindowTitle(),
		checkBinaryStaleness(),
	}
	if m.daemonEvents != nil {
		cmds = append(cmds, readNextDaemonEvent(m.daemonEvents))
	}
	return tea.Batch(cmds...)
}

// computeWindowTitle returns the title we want the host terminal to
// display. Reflects the focused pane's OSC-set title when in agent
// view, falling back to the ticket title, with an "openkanban: "
// prefix. Returns "openkanban" outside of agent view.
func (m *Model) computeWindowTitle() string {
	const prefix = "openkanban"
	if m.mode != ModeAgentView || m.focusedPane == "" {
		return prefix
	}
	pane, ok := m.panes[m.focusedPane]
	var sub string
	if ok {
		sub = pane.Title()
	}
	if sub == "" {
		if ticket, _ := m.globalStore.Get(m.focusedPane); ticket != nil {
			sub = ticket.Title
		}
	}
	if sub == "" {
		return prefix
	}
	return prefix + ": " + sub
}

// maybeSetWindowTitle returns a tea.Cmd to update the host terminal's
// window title if the desired title has changed. Returns nil if no
// change.
func (m *Model) maybeSetWindowTitle() tea.Cmd {
	want := m.computeWindowTitle()
	if want == m.lastWindowTitle {
		return nil
	}
	m.lastWindowTitle = want
	return tea.SetWindowTitle(want)
}

func (m *Model) checkForUpdates() tea.Cmd {
	if m.updateChecker == nil {
		return nil
	}
	return func() tea.Msg {
		return updateCheckMsg(m.updateChecker.Check())
	}
}

// Update is the BubbleTea entry point. The actual case-dispatch lives
// in dispatchUpdate; this wrapper reconciles the daemon's viewing
// state (a single "what session is this TUI focused on right now"
// signal) against whatever mode/focusedPane mutation the inner
// dispatch may have performed. Centralizing the reconcile here means
// no individual case branch has to remember to fire SetViewing —
// the diff between viewingSessionID and the post-update truth covers
// every transition path.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.dispatchUpdate(msg)
	nm, ok := next.(*Model)
	if !ok {
		return next, cmd
	}
	if viewingCmd := nm.reconcileViewing(); viewingCmd != nil {
		return nm, tea.Batch(cmd, viewingCmd)
	}
	return nm, cmd
}

// reconcileViewing computes the session this TUI is currently focused
// on (ModeAgentView + a pane for the focused ticket), compares it to
// the last value we told the daemon, and returns the fire-and-forget
// tea.Cmd that calls SetViewing(prev,false) and/or SetViewing(new,
// true) for the diff. Returns nil when nothing changed.
//
// Two calls when both prev and new are non-empty (a focus change
// between sessions); one call on enter-from-board and one on
// leave-to-board; zero work in the steady state.
func (m *Model) reconcileViewing() tea.Cmd {
	target := ""
	if m.mode == ModeAgentView && m.focusedPane != "" {
		if pv, ok := m.panes[m.focusedPane]; ok && pv != nil {
			target = pv.SessionID()
		}
	}
	if target == m.viewingSessionID {
		return nil
	}
	prev := m.viewingSessionID
	m.viewingSessionID = target
	var cmds []tea.Cmd
	if prev != "" {
		cmds = append(cmds, m.setViewingCmd(prev, false))
	}
	if target != "" {
		cmds = append(cmds, m.setViewingCmd(target, true))
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// setViewingCmd returns a fire-and-forget tea.Cmd that calls
// daemonClient.SetViewing. The RPC is idempotent on the daemon side
// (duplicate true / duplicate false is a silent no-op), so callers
// don't need to gate. Returns nil when there's no daemon client or
// the sessionID is empty.
func (m *Model) setViewingCmd(sessionID string, viewing bool) tea.Cmd {
	if m.daemonClient == nil || sessionID == "" {
		return nil
	}
	client := m.daemonClient
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		if _, err := client.SetViewing(ctx, sessionID, viewing); err != nil {
			log.Printf("openkanban: SetViewing(%s, %v) failed: %v", sessionID, viewing, err)
		}
		return nil
	}
}

func (m *Model) dispatchUpdate(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Daemon push events must be handled regardless of mode so the
	// readNextDaemonEvent listener is always re-armed. If we let these
	// fall through to a mode-specific switch (ModeSpawning, ModeShuttingDown)
	// that doesn't list them as a case, the msg is silently dropped and
	// handleDaemonSessionEvent never fires — which means the re-arm cmd
	// is never returned and every subsequent push event piles up in the
	// subscriber channel buffer with no reader.
	switch msg := msg.(type) {
	case daemonSessionEventMsg:
		return m.handleDaemonSessionEvent(msg)
	case daemonSubscribeFailedMsg:
		return m.handleDaemonSubscribeFailed(msg)
	case daemonSubscribeEndedMsg:
		return m.handleDaemonSubscribeEnded(msg)
	}

	if m.mode == ModeShuttingDown {
		switch msg := msg.(type) {
		case shutdownCompleteMsg:
			return m, tea.Quit
		case spinner.TickMsg:
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
		return m, nil
	}

	if m.mode == ModeSpawning {
		switch msg := msg.(type) {
		case agentStatusMsg:
			return m, tea.Batch(
				m.pollAgentStatusesAsync(),
				tickAgentStatus(m.agentMgr.StatusPollInterval()),
			)
		case spawnReadyMsg:
			if msg.ticketID != m.spawningTicketID {
				return m, nil
			}

			ticket, _ := m.globalStore.Get(msg.ticketID)
			if ticket != nil {
				ticket.AgentType = m.spawningAgent
				// Don't clobber AgentStatus here. The daemon's "started"
				// SessionEvent (which handleDaemonSessionEvent sets to
				// AgentWorking) can race with spawnReadyMsg and arrive
				// first; a blind reset here would replace the correct
				// "working" with AgentNone and leave the card blank.
				// The daemon push is authoritative for AgentStatus.
				if ticket.AgentSpawnedAt == nil {
					now := time.Now()
					ticket.AgentSpawnedAt = &now
				}
				if msg.worktreePath != "" && ticket.WorktreePath == "" {
					ticket.WorktreePath = msg.worktreePath
					ticket.BranchName = msg.branchName
					ticket.BaseBranch = msg.baseBranch
				}
				m.saveTicket(ticket)
			}

			m.panes[msg.ticketID] = msg.pane
			m.focusedPane = msg.ticketID
			if msg.notice != "" {
				m.notify(msg.notice)
			}
			// Switch to attached view and start listening for pane msgs.
			// The pane is already attached (Spawn returned + Attach
			// happened on the goroutine), so we just need to drain its
			// tea channel.
			m.mode = ModeAgentView
			m.spawningTicketID = ""
			m.spawningAgent = ""
			return m, tea.Batch(
				m.listenPaneMessages(msg.pane),
				m.maybeSetWindowTitle(),
			)

		case spawnErrorMsg:
			if msg.ticketID == m.spawningTicketID {
				m.mode = ModeNormal
				m.spawningTicketID = ""
				m.spawningAgent = ""
				m.notify(msg.err)
			}
			return m, nil

		case daemonclient.PaneOutputMsg:
			// Pane started producing output while we're still showing
			// the "Spawning…" splash (rare — Spawn usually returns
			// spawnReadyMsg before any output arrives). Promote to
			// attached view so the user actually sees it.
			if board.TicketID(msg.PaneID) == m.spawningTicketID {
				m.mode = ModeAgentView
				m.spawningTicketID = ""
				m.spawningAgent = ""
			}
			return m.handleTerminalMsg(msg)

		case daemonclient.PaneExitMsg:
			if board.TicketID(msg.PaneID) == m.spawningTicketID {
				m.resetSpawnState(board.TicketID(msg.PaneID))
				if msg.Err != nil {
					m.notify("Agent failed: " + msg.Err.Error())
				} else {
					m.notify("Agent exited unexpectedly")
				}
			}
			return m, nil

		case spinner.TickMsg:
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd

		case tea.KeyMsg:
			if msg.String() == "esc" {
				if pane, ok := m.panes[m.spawningTicketID]; ok {
					pane.Stop()
					delete(m.panes, m.spawningTicketID)
				}
				m.mode = ModeNormal
				m.spawningTicketID = ""
				m.spawningAgent = ""
				m.notify("Spawn cancelled")
				return m, nil
			}
		}
		return m, nil
	}

	// Bubbletea v1 silently drops CSI sequences it doesn't have in its
	// hardcoded table — notably xterm modifyOtherKeys
	// (\x1b[27;<mod>;<key>~), which Ghostty emits for shift+enter,
	// ctrl+enter, etc. When the user is attached to a pane, forward
	// the raw bytes so the inner agent (Claude Code, etc.) can
	// interpret the sequence natively.
	if m.mode == ModeAgentView {
		if raw := daemonclient.ExtractRawCSIBytes(msg); raw != nil {
			if pane, ok := m.panes[m.focusedPane]; ok {
				pane.WriteRaw(raw)
			}
			return m, m.maybeSetWindowTitle()
		}
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)

	case QuitRequestedMsg:
		return m.handleQuitRequested()

	case prepareExitResultMsg:
		return m.handlePrepareExitResult(msg)

	case prepareExitFailedMsg:
		return m.handlePrepareExitFailed(msg)

	case sessionKilledMsg:
		return m.handleSessionKilled(msg)

	case sessionKillFailedMsg:
		return m.handleSessionKillFailed(msg)

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if m.focusedPane != "" {
			if pane, ok := m.panes[m.focusedPane]; ok {
				pane.SetSize(m.width, m.height-2)
			}
		}
		return m, nil

	case tea.MouseMsg:
		if m.mode == ModeNormal {
			return m.handleMouse(msg)
		}
		if m.mode == ModeAgentView {
			return m.handleAgentViewMouse(msg)
		}
		if m.mode == ModeCreateTicket || m.mode == ModeEditTicket {
			return m.handleTicketFormMouse(msg)
		}
		if m.mode == ModeFilter {
			return m.handleFilterMouse(msg)
		}
		if m.mode == ModeSettings {
			return m.handleSettingsMouse(msg)
		}
		if m.showHelp {
			if msg.Action == tea.MouseActionPress {
				m.showHelp = false
			}
			return m, nil
		}
		if m.showConfirm {
			return m.handleConfirmMouse(msg)
		}
		return m, nil

	case daemonclient.PaneOutputMsg, daemonclient.PaneRenderTickMsg, daemonclient.PaneAttachedMsg:
		return m.handleTerminalMsg(msg)

	case daemonclient.PaneDetachedMsg:
		// Detach is the signal we get when the binary attach conn
		// closes — either the agent exited (claude /q), the daemon
		// killed the session, or another TUI took over. In all three
		// cases, the local PaneView's vt has been torn down and View()
		// returns "" (blank pane). Surface that by returning the user
		// to the board if they're currently focused on this pane.
		// They can re-enter via Enter if the session is still alive in
		// the daemon (e.g. takeover case).
		ticketID := board.TicketID(msg.PaneID)
		if m.focusedPane == ticketID {
			m.mode = ModeNormal
			m.focusedPane = ""
			m.selectTicketByID(ticketID)
		}
		return m.handleTerminalMsg(msg)

	case daemonclient.PaneExitMsg:
		ticketID := board.TicketID(msg.PaneID)
		// The PaneExitMsg path fires when the local PaneView's binary
		// reader saw the conn close (e.g. detach + remote pane death).
		// The authoritative "was this exit expected?" signal arrives
		// separately as daemonSessionEventMsg{Event:"exited", Expected:...}
		// and is handled in handleDaemonSessionEvent, which preserves
		// AgentCompleted when appropriate. From this path we cannot
		// know intent, so we conservatively reset to AgentNone here;
		// when the daemon event lands (which it will, before or after
		// this msg) it will overwrite to AgentCompleted if Expected=true.
		if pv, ok := m.panes[ticketID]; ok {
			if pv != nil {
				_ = pv.Close()
			}
			delete(m.panes, ticketID)
		}
		if ticket, _ := m.globalStore.Get(ticketID); ticket != nil {
			// Only reset if not already AgentCompleted — the daemon
			// session-event may have raced ahead and set Completed,
			// in which case we must not clobber it.
			if ticket.AgentStatus != board.AgentCompleted {
				ticket.AgentStatus = board.AgentNone
				m.saveTicket(ticket)
			}
		}
		if m.focusedPane == ticketID {
			m.mode = ModeNormal
			m.focusedPane = ""
			m.notify("Agent exited")
			m.selectTicketByID(ticketID)
		}
		return m, m.maybeSetWindowTitle()

	case daemonclient.AttachFirstMsg:
		// HandleKey returned this because the user typed into an
		// unattached pane. Attach (or takeover, if another client is
		// already attached) and resume listening.
		ticketID := board.TicketID(msg.PaneID)
		if pv, ok := m.panes[ticketID]; ok {
			cmd := m.attachExisting(ticketID, pv)
			return m, cmd
		}
		return m, nil

	case daemonclient.DaemonDisconnectedMsg:
		// Daemon vanished mid-session. Detach every PaneView; the model
		// keeps running but with no live attaches. PR8b/PR9 will
		// auto-reconnect; for PR8 we just degrade gracefully.
		for id, pv := range m.panes {
			_ = pv.Close()
			delete(m.panes, id)
		}
		if m.focusedPane != "" {
			m.mode = ModeNormal
			m.focusedPane = ""
		}
		// Daemon push channel is gone; the file-poll takes over as the
		// AgentStatus source. Clear the subscribe handles so we don't
		// keep dangling references, and drop daemonOwned so the poll
		// can reassert authority on the next tick.
		m.daemonConnected.Store(false)
		if m.daemonUnsub != nil {
			m.daemonUnsub()
			m.daemonUnsub = nil
		}
		m.daemonEvents = nil
		for id := range m.daemonOwned {
			delete(m.daemonOwned, id)
		}
		for id := range m.daemonViewing {
			delete(m.daemonViewing, id)
		}
		m.viewingSessionID = ""
		if msg.Err != nil {
			m.notify("Daemon disconnected: " + msg.Err.Error())
		} else {
			m.notify("Daemon disconnected")
		}
		return m, m.maybeSetWindowTitle()

	case terminal.ExitFocusMsg:
		m.mode = ModeNormal
		m.focusedPane = ""
		return m, m.maybeSetWindowTitle()

	case agentStatusMsg:
		return m, tea.Batch(
			m.pollAgentStatusesAsync(),
			tickAgentStatus(m.agentMgr.StatusPollInterval()),
		)

	case agentStatusResultMsg:
		// The file-poll value is the authoritative source for the
		// intra-session transitions Claude Code's hooks emit
		// (working / idle / waiting / error), regardless of whether
		// the ticket is daemon-owned. Two narrow guards:
		//
		//   - A poll value of AgentNone means the file is absent and
		//     the terminal scrape produced no hit. That's "I don't
		//     know," not a transition — preserve whatever was set
		//     (typically AgentWorking from the daemon's "started"
		//     SessionEvent, or AgentCompleted from ticket-done).
		//
		//   - AgentCompleted is terminal. Only another terminal value
		//     (Completed, Error) may overwrite it. This mirrors the
		//     symmetric guard in cmd/status.go that prevents Claude's
		//     Stop hook racing TicketDone during the SIGTERM grace
		//     window from downgrading the completion signal to idle.
		//
		// The pre-fix rule unconditionally skipped poll values for
		// daemon-owned tickets, which froze AgentStatus at the daemon's
		// "started" event (AgentWorking) for the entire session — hooks
		// updated the file but the TUI never reflected it.
		for ticketID, status := range msg {
			if status == board.AgentNone {
				continue
			}
			ticket, _ := m.globalStore.Get(ticketID)
			if ticket == nil {
				continue
			}
			if ticket.AgentStatus == board.AgentCompleted &&
				status != board.AgentCompleted &&
				status != board.AgentError {
				continue
			}
			ticket.AgentStatus = status

			// T2 of the integration plan removed the edge-triggered
			// auto-stop on AgentCompleted: ticket-done now flows
			// CLI → daemon (TicketDoneReq) → SessionEvent broadcast,
			// and the daemon's authoritative Expected=true signal lands
			// via handleDaemonSessionEvent. The poll's job is reduced
			// to refreshing AgentStatus for visibility — it no longer
			// kills panes.
		}

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case notificationMsg:
		if time.Since(m.notifyTime) > 3*time.Second {
			m.notification = ""
		}
		return m, nil

	case updateCheckMsg:
		if msg.UpdateAvailable {
			result := update.CheckResult(msg)
			m.notify(fmt.Sprintf("Update %s available: %s", msg.LatestVersion, result.UpdateHint()))
		}
		return m, nil

	case binaryStaleCheckMsg:
		// Periodic self-staleness check. The binary may have been
		// replaced under us by `go install` / `openkanban update`
		// running in another shell; long-lived TUI sessions otherwise
		// have no signal that an upgrade has landed. We surface the
		// notification once per stale-transition (not every 30s) and
		// re-arm the tick unconditionally. See update.BinaryStale.
		if update.BinaryStale() {
			if !m.binaryStaleNotified {
				m.notify("openkanban binary updated on disk — press Ctrl-R to restart, or 'q' to quit and relaunch")
				m.binaryStaleNotified = true
			}
		} else {
			m.binaryStaleNotified = false
		}
		return m, checkBinaryStaleness()

	case FsChangedMsg:
		m.handleFsChanged(msg)
		return m, nil
	}

	return m, nil
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// ModeConfirmExit owns the keyboard entirely while the exit-guard
	// modal is up — including q / Ctrl-C / Esc / ?. Route to its
	// dedicated handler before the global key map runs, so the modal's
	// own bindings (x kill, X kill-all, Esc cancel) take precedence.
	if m.mode == ModeConfirmExit {
		return m.handleConfirmExitMode(msg)
	}

	switch msg.String() {
	case "ctrl+c", "q":
		if m.mode == ModeNormal {
			return m.handleQuit()
		}
	case "esc":
		if m.mode == ModeAgentView {
			break
		}
		if m.mode == ModeNormal && (m.filterQuery != "" || len(m.filterProjectIDs) > 0) {
			m.clearFilter()
			m.notify("Filter cleared")
			return m, nil
		}
		m.mode = ModeNormal
		m.showHelp = false
		m.showConfirm = false
		m.titleInput.Blur()
		return m, nil
	case "?":
		if m.mode == ModeNormal || m.mode == ModeHelp {
			m.showHelp = !m.showHelp
			return m, nil
		}
	}

	if m.showHelp {
		m.showHelp = false
		return m, nil
	}

	if m.showChoice {
		return m.handleChoice(msg)
	}

	if m.showConfirm {
		return m.handleConfirm(msg)
	}

	switch m.mode {
	case ModeNormal:
		return m.handleNormalMode(msg)
	case ModeCommand:
		return m.handleCommandMode(msg)
	case ModeCreateTicket:
		return m.handleCreateTicketMode(msg)
	case ModeEditTicket:
		return m.handleEditTicketMode(msg)
	case ModeAgentView:
		return m.handleAgentViewMode(msg)
	case ModeSettings:
		return m.handleSettingsMode(msg)
	case ModeFilter:
		return m.handleFilterMode(msg)
	case ModeCreateProject:
		return m.handleCreateProjectMode(msg)
	}

	return m, nil
}

func (m *Model) handleNormalMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "tab":
		if m.sidebarVisible {
			m.sidebarFocused = !m.sidebarFocused
			return m, nil
		}
	case "[":
		m.sidebarVisible = !m.sidebarVisible
		if !m.sidebarVisible {
			m.sidebarFocused = false
		}
		return m, nil
	}

	if m.sidebarFocused {
		return m.handleSidebarNav(msg)
	}

	switch msg.String() {
	case "ctrl+r":
		// Stale-binary restart shortcut. We only honor this when the
		// on-disk binary has actually been replaced — otherwise an
		// errant Ctrl-R would silently kill the TUI. The exit path
		// reuses the existing quit-with-guard flow so live agent
		// sessions are not orphaned; the user re-launches with
		// `openkanban` after the guard clears.
		if update.BinaryStale() {
			m.notify("Restarting to pick up new binary — re-launch with `openkanban`")
			return m.handleQuit()
		}
		return m, nil
	case "h", "left":
		if m.activeColumn == 0 && m.sidebarVisible {
			m.sidebarFocused = true
			return m, nil
		}
		m.moveColumn(-1)
	case "l", "right":
		m.moveColumn(1)
	case "j", "down":
		m.moveTicket(1)
	case "k", "up":
		m.moveTicket(-1)
	case "g":
		m.activeTicket = 0
		m.ensureTicketVisible()
	case "G":
		if len(m.columnTickets) > m.activeColumn {
			m.activeTicket = max(len(m.columnTickets[m.activeColumn])-1, 0)
		}
		m.ensureTicketVisible()

	case "n":
		return m.createNewTicket()
	case "e":
		return m.editTicket()
	case "enter", "s":
		// Single entry point: spawnAgent dispatches to spawn or
		// re-attach based on the current pane state. Pre-consolidation,
		// Enter only attached and 's' only spawned, which produced
		// cross-key bounces ("press Enter to attach" / "press 's' to
		// spawn") for the user.
		return m.spawnAgent()
	case "d":
		return m.confirmDeleteTicket()
	case " ":
		return m.quickMoveTicket()
	case "-", "backspace":
		return m.quickMoveTicketBackward()
	case "S":
		return m.stopAgent()

	case "K":
		return m.adjustPriority(-1)
	case "J":
		return m.adjustPriority(1)
	case "o":
		return m.cycleSortMode()
	case "w":
		return m.cycleSessionFilter()
	case "W":
		return m.toggleAlwaysShowWorking()

	case ":":
		m.mode = ModeCommand

	case "/":
		m.filterInput.SetValue(m.filterQuery)
		m.filterInput.Focus()
		m.mode = ModeFilter

	case "O":
		m.mode = ModeSettings
		m.settingsIndex = 0
		m.settingsEditing = false
	}

	return m, nil
}

func (m *Model) openAddProjectForm() (tea.Model, tea.Cmd) {
	m.addProjectPath.SetValue("")
	m.addProjectPath.Focus()
	m.mode = ModeCreateProject
	m.notification = ""
	return m, textinput.Blink
}

func (m *Model) sidebarAllY() int          { return 2 }
func (m *Model) sidebarProjectStartY() int { return 4 }
func (m *Model) sidebarAddProjectY(projectCount int) int {
	return m.sidebarProjectStartY() + projectCount + 1
}

func (m *Model) handleSidebarMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	y := msg.Y - m.headerHeight()

	if y < 0 {
		return m, nil
	}

	projects := m.globalStore.Projects()

	if y == m.sidebarAllY() {
		m.sidebarIndex = 0
		m.toggleAllProjects()
		return m, nil
	}

	for i := range projects {
		if y == m.sidebarProjectStartY()+i {
			m.sidebarIndex = i + 1
			m.toggleProjectFilter(projects[i].ID)
			return m, nil
		}
	}

	if y == m.sidebarAddProjectY(len(projects)) {
		return m.openAddProjectForm()
	}

	m.sidebarFocused = true
	return m, nil
}

func (m *Model) handleSidebarNav(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	projects := m.globalStore.Projects()
	addIndex := len(projects) + 1

	switch msg.String() {
	case "j", "down":
		if m.sidebarIndex < addIndex {
			m.sidebarIndex++
		}
	case "k", "up":
		if m.sidebarIndex > 0 {
			m.sidebarIndex--
		}
	case "enter", " ":
		if m.sidebarIndex == 0 {
			m.toggleAllProjects()
		} else if m.sidebarIndex == addIndex {
			return m.openAddProjectForm()
		} else {
			idx := m.sidebarIndex - 1
			if idx < len(projects) {
				m.toggleProjectFilter(projects[idx].ID)
			}
		}
	case "l", "right":
		m.sidebarFocused = false
		return m, nil
	case "a":
		return m.openAddProjectForm()
	case "d":
		if m.sidebarIndex > 0 && m.sidebarIndex <= len(projects) {
			m.confirmDeleteProject(projects[m.sidebarIndex-1])
		}
		return m, nil
	case "esc":
		m.sidebarFocused = false
	}

	return m, nil
}

func (m *Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	switch msg.Action {
	case tea.MouseActionPress:
		if msg.Button == tea.MouseButtonLeft {
			if m.hitTestHeader(msg.X, msg.Y) {
				return m, nil
			}
			if m.sidebarVisible && msg.X < m.sidebarWidth {
				return m.handleSidebarMouse(msg)
			}
			col, ticket := m.hitTest(msg.X, msg.Y)
			if col >= 0 {
				m.sidebarFocused = false
				m.activeColumn = col
				if ticket >= 0 {
					now := time.Now()
					isDoubleClick := ticket == m.lastClickTicket &&
						col == m.lastClickColumn &&
						now.Sub(m.lastClickTime) < 400*time.Millisecond

					if isDoubleClick {
						m.lastClickTime = time.Time{}
						m.lastClickColumn = -1
						m.lastClickTicket = -1
						return m.handleDoubleClick()
					}

					m.lastClickTime = now
					m.lastClickColumn = col
					m.lastClickTicket = ticket

					m.activeTicket = ticket
					m.dragging = true
					m.dragSourceColumn = col
					m.dragSourceTicket = ticket
					m.dragTargetColumn = col
				}
				m.ensureColumnVisible()
			}
		}

	case tea.MouseActionMotion:
		if m.dragging && msg.Button == tea.MouseButtonLeft {
			col, _ := m.hitTest(msg.X, msg.Y)
			if col >= 0 {
				m.dragTargetColumn = col
			}
		} else {
			if m.sidebarVisible && msg.X < m.sidebarWidth {
				m.hoverColumn = -1
				m.hoverTicket = -1
			} else {
				col, ticket := m.hitTest(msg.X, msg.Y)
				m.hoverColumn = col
				m.hoverTicket = ticket
			}
		}

	case tea.MouseActionRelease:
		if m.dragging {
			if m.dragTargetColumn != m.dragSourceColumn && m.dragTargetColumn >= 0 {
				return m.dropTicket()
			}
			m.dragging = false
			m.dragTargetColumn = 0
		}
		col, ticket := m.hitTest(msg.X, msg.Y)
		m.hoverColumn = col
		m.hoverTicket = ticket

	default:
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			m.moveTicket(-1)
		case tea.MouseButtonWheelDown:
			m.moveTicket(1)
		}
	}

	return m, nil
}

func (m *Model) hitTestHeader(x, y int) bool {
	if y > 2 {
		return false
	}

	if m.filterQuery != "" || len(m.filterProjectIDs) > 0 {
		clearStart := 20 + len(m.filterQuery) + 15
		if x >= clearStart && x <= clearStart+10 {
			m.clearFilter()
			return true
		}
	}

	if x >= 15 && x <= 30 {
		m.filterInput.SetValue(m.filterQuery)
		m.filterInput.Focus()
		m.mode = ModeFilter
		return true
	}

	return false
}

func (m *Model) hitTest(x, y int) (column, ticket int) {
	if m.width == 0 || len(m.columns) == 0 {
		return -1, -1
	}

	if m.sidebarVisible {
		x = x - m.sidebarWidth - 1
	}

	headerHeight := 2
	if y < headerHeight {
		return -1, -1
	}

	columnWidth := m.calcColumnWidth()
	visibleCols := m.visibleColumnCount(columnWidth)
	numVisible := visibleCols
	if m.scrollOffset+visibleCols > len(m.columns) {
		numVisible = len(m.columns) - m.scrollOffset
	}

	baseWidth, remainder := m.distributeWidth(numVisible)

	hasLeftIndicator := m.scrollOffset > 0
	startX := 0
	if hasLeftIndicator {
		startX = 2
	}

	for i := 0; i < numVisible; i++ {
		colWidth := baseWidth + 3
		if i < remainder {
			colWidth++
		}

		if x >= startX && x < startX+colWidth {
			actualCol := m.scrollOffset + i
			ticketIdx := m.hitTestTicket(y-headerHeight, actualCol)
			return actualCol, ticketIdx
		}
		startX += colWidth
	}

	return -1, -1
}

func (m *Model) hitTestTicket(relativeY, column int) int {
	if column < 0 || column >= len(m.columnTickets) {
		return -1
	}

	tickets := m.columnTickets[column]
	if len(tickets) == 0 {
		return -1
	}

	ticketY := relativeY - columnHeaderHeight
	if ticketY < 0 {
		return -1
	}

	offset := 0
	if column < len(m.columnOffsets) {
		offset = m.columnOffsets[column]
	}

	// Walk per-ticket heights from `offset`, accumulating until cumulative
	// height crosses ticketY. With a "▲ N more" indicator above (when
	// offset > 0) the first card sits one row down, so consume that row
	// from ticketY before walking.
	//
	// Example (offset=0, heights = [8, 9, 8], ticketY=12):
	//   cum=0  → 8  : 12 not in [0,8)   → index 1 candidate
	//   cum=8  → 17 : 12 in [8,17)      → return 1
	if offset > 0 {
		ticketY-- // ▲ N more row
		if ticketY < 0 {
			return -1
		}
	}

	var heights []int
	if column < len(m.columnTicketHeights) {
		heights = m.columnTicketHeights[column]
	}
	heightOf := func(i int) int {
		if i < len(heights) && heights[i] > 0 {
			return heights[i]
		}
		return ticketHeight
	}

	cum := 0
	for i := offset; i < len(tickets); i++ {
		next := cum + heightOf(i)
		if ticketY < next {
			return i
		}
		cum = next
	}
	return -1
}

func (m *Model) dropTicket() (tea.Model, tea.Cmd) {
	if len(m.columnTickets) <= m.dragSourceColumn {
		m.dragging = false
		return m, nil
	}

	tickets := m.columnTickets[m.dragSourceColumn]
	if len(tickets) <= m.dragSourceTicket {
		m.dragging = false
		return m, nil
	}

	ticket := tickets[m.dragSourceTicket]
	targetStatus := m.columns[m.dragTargetColumn].Status

	if targetStatus == board.StatusInProgress && ticket.WorktreePath == "" {
		if ticket.UseWorktree {
			if err := m.setupWorktree(ticket); err != nil {
				m.notify("Worktree failed: " + err.Error())
				m.dragging = false
				return m, nil
			}
		} else {
			if err := m.setupMainRepoBranch(ticket); err != nil {
				m.notify("Branch setup failed: " + err.Error())
				m.dragging = false
				return m, nil
			}
		}
	}

	promoted, _ := m.globalStore.Move(ticket.ID, targetStatus)
	m.refreshColumnTickets()
	m.saveTicket(ticket)

	m.activeColumn = m.dragTargetColumn
	m.activeTicket = 0
	m.ensureColumnVisible()
	m.ensureTicketVisible()

	m.notify(moveAndPromoteMsg(targetStatus, promoted))
	m.dragging = false
	m.dragTargetColumn = 0

	return m, nil
}

func (m *Model) handleCommandMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		m.mode = ModeNormal
	case "esc":
		m.mode = ModeNormal
	}
	return m, nil
}

func (m *Model) handleConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		m.showConfirm = false
		if m.confirmFn != nil {
			return m, m.confirmFn()
		}
	case "n", "N", "esc":
		m.showConfirm = false
	}
	return m, nil
}

func (m *Model) handleChoice(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if key == "esc" {
		m.showChoice = false
		m.choices = nil
		m.choiceMsg = ""
		return m, nil
	}
	for _, c := range m.choices {
		if string(c.Key) == key {
			fn := c.Fn
			m.showChoice = false
			m.choices = nil
			m.choiceMsg = ""
			if fn != nil {
				return m, fn()
			}
			return m, nil
		}
	}
	// Non-matching keys are no-ops while a choice modal is active.
	return m, nil
}

func (m *Model) handleQuit() (tea.Model, tea.Cmd) {
	// If a daemon client is wired up, defer to the daemon-aware exit
	// guard so we never silently kill (or orphan) live agent sessions.
	// The guard's PrepareExit RPC tells us the authoritative ClientCount
	// + Sessions snapshot, and the modal (ModeConfirmExit) gates exit on
	// the user explicitly killing them. See handleQuitRequested.
	if m.guardAPI != nil {
		return m.handleQuitRequested()
	}

	// Daemon unreachable — fall back to the legacy local-only path so the
	// user is never trapped in the TUI when the daemon is missing.
	runningCount := m.RunningAgentCount()
	if runningCount == 0 {
		return m, tea.Quit
	}

	if !m.config.Behavior.ConfirmQuitWithAgents {
		m.mode = ModeShuttingDown
		return m, tea.Batch(m.spinner.Tick, m.cleanupAsync())
	}

	m.showConfirm = true
	m.confirmMsg = fmt.Sprintf("%d agent(s) running. Quit anyway? [y/N]", runningCount)
	m.confirmFn = func() tea.Cmd {
		m.mode = ModeShuttingDown
		m.showConfirm = false
		return tea.Batch(m.spinner.Tick, m.cleanupAsync())
	}
	return m, nil
}

func (m *Model) cleanupAsync() tea.Cmd {
	return func() tea.Msg {
		m.Cleanup()
		return shutdownCompleteMsg{}
	}
}

func (m *Model) handleAgentViewMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// The cycle-attach modal swallows all keys until the user resolves
	// it (Enter attaches, Esc returns to the board, Ctrl+\ / Ctrl+]
	// continue cycling). Handled before any pane dispatch so the PTY
	// never sees these keys.
	if m.cycleAttachPrompt {
		return m.handleCycleAttachPromptKey(msg)
	}

	pane, ok := m.panes[m.focusedPane]
	if !ok {
		m.mode = ModeNormal
		m.focusedPane = ""
		return m, m.maybeSetWindowTitle()
	}

	// Session-cycle bindings are intercepted before the pane forwards
	// keystrokes to the PTY child — otherwise claude/whatever-agent
	// would consume them. Ctrl+\ and Ctrl+] are the closest survivable
	// pair to the original Ctrl+[/Ctrl+] request (Ctrl+[ is bytewise
	// indistinguishable from Esc in this bubbletea build).
	switch msg.String() {
	case "ctrl+]":
		return m.cycleUnattachedSession(1)
	case "ctrl+\\":
		return m.cycleUnattachedSession(-1)
	}

	if result := pane.HandleKey(msg); result != nil {
		switch r := result.(type) {
		case terminal.ExitFocusMsg:
			log.Printf("openkanban model: ExitFocusMsg received, mode -> ModeNormal")
			m.mode = ModeNormal
			m.focusedPane = ""
		case daemonclient.AttachFirstMsg:
			// User typed into an unattached pane — attach now and
			// re-deliver the key would be nice but is out of scope for
			// PR8. The model just kicks off the attach; the user can
			// retype after the snapshot arrives.
			cmd := m.attachExisting(board.TicketID(r.PaneID), pane)
			return m, tea.Batch(cmd, m.maybeSetWindowTitle())
		}
	}

	return m, m.maybeSetWindowTitle()
}

// handleCycleAttachPromptKey resolves the modal opened by
// cycleUnattachedSession. Enter attaches to the currently focused
// pane and clears the modal; Esc cancels and returns to the board;
// Ctrl+\ / Ctrl+] keep cycling; every other key is swallowed so the
// modal can't be bypassed by a stray keystroke landing in the PTY.
func (m *Model) handleCycleAttachPromptKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		pv, ok := m.panes[m.focusedPane]
		if !ok || pv == nil {
			m.cycleAttachPrompt = false
			m.mode = ModeNormal
			m.focusedPane = ""
			return m, m.maybeSetWindowTitle()
		}
		m.cycleAttachPrompt = false
		cmd := m.attachExisting(m.focusedPane, pv)
		return m, tea.Batch(cmd, m.maybeSetWindowTitle())
	case "esc":
		m.cycleAttachPrompt = false
		m.mode = ModeNormal
		m.focusedPane = ""
		return m, m.maybeSetWindowTitle()
	case "ctrl+]":
		return m.cycleUnattachedSession(1)
	case "ctrl+\\":
		return m.cycleUnattachedSession(-1)
	}
	return m, nil
}

func (m *Model) handleAgentViewMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	pane, ok := m.panes[m.focusedPane]
	if !ok {
		return m, nil
	}
	adjusted, action := routeAgentViewMouse(msg, m.width, m.agentViewChromeHeight())
	switch action {
	case agentViewMouseCloseModal:
		m.mode = ModeNormal
		return m, m.maybeSetWindowTitle()
	case agentViewMouseForward:
		pane.HandleMouse(adjusted)
	}
	return m, nil
}

// agentViewMouseAction is the routing decision for an incoming mouse
// event in the agent view modal.
type agentViewMouseAction int

const (
	agentViewMouseDrop agentViewMouseAction = iota
	agentViewMouseForward
	agentViewMouseCloseModal
)

// routeAgentViewMouse decides what handleAgentViewMouse should do
// with a mouse event: close the modal (close-button hit), forward
// to the pane with pane-relative coordinates, or drop it.
//
// BubbleTea reports mouse Y relative to the host terminal (row 0
// = top of TUI). The pane's content sits below the agent view's
// chrome (1-row header, plus optional 1-row deps line). We subtract
// chrome height so the pane sees pane-relative coordinates;
// otherwise selection lands one (or two) rows below the cursor.
//
// Position-sensitive events (click/drag) landing on the chrome
// rows are dropped. Wheel events are position-insensitive — the
// user expects scroll to work regardless of which row the cursor
// happens to sit on — so they are clamped to row 0 and forwarded.
func routeAgentViewMouse(msg tea.MouseMsg, width, chrome int) (tea.MouseMsg, agentViewMouseAction) {
	if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
		if msg.Y == 0 && msg.X >= width-25 {
			return msg, agentViewMouseCloseModal
		}
	}
	adjusted := msg
	adjusted.Y = msg.Y - chrome
	if adjusted.Y < 0 {
		if !tea.MouseEvent(msg).IsWheel() {
			return msg, agentViewMouseDrop
		}
		adjusted.Y = 0
	}
	return adjusted, agentViewMouseForward
}

// agentViewChromeHeight returns the height in rows of the non-pane
// content rendered above the agent terminal pane: 1 for the header,
// plus 1 for the deps line when the focused ticket has any
// BlockedBy / Blocks relationships. Used to translate host-terminal
// mouse coords into pane-relative coords.
func (m *Model) agentViewChromeHeight() int {
	ticket, _ := m.globalStore.Get(m.focusedPane)
	return agentChromeHeight(ticketHasDeps(m.globalStore, ticket))
}

// ticketHasDeps reports whether a ticket has any incoming or outgoing
// dependency relationships (i.e. the agent view should render its
// deps line for this ticket).
func ticketHasDeps(g *project.GlobalTicketStore, t *board.Ticket) bool {
	if g == nil || t == nil {
		return false
	}
	return len(g.GetBlockedBy(t.ID)) > 0 || len(g.GetBlocks(t.ID)) > 0
}

// agentChromeHeight is the pure mapping from "does the deps line
// render?" to chrome height. Kept separate from the Model so it can
// be unit-tested without constructing a store.
//
// Layout: 1 row header + (0 or 1) deps row + 1 row heavy rule. The
// rule is the visual boundary between openkanban chrome and the
// embedded PTY, so mouse coords must account for it when translating
// host-terminal Y into pane-relative Y.
func agentChromeHeight(hasDeps bool) int {
	if hasDeps {
		return 3
	}
	return 2
}

func (m *Model) handleTicketFormMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		m.formScrollOffset -= 3
		if m.formScrollOffset < 0 {
			m.formScrollOffset = 0
		}
		return m, nil
	case tea.MouseButtonWheelDown:
		m.formScrollOffset += 3
		return m, nil
	}

	formWidth := 50
	formLeft := (m.width - formWidth) / 2
	formRight := formLeft + formWidth

	if msg.X < formLeft || msg.X > formRight {
		return m, nil
	}

	formTop := (m.height - 28) / 2
	relY := msg.Y - formTop

	var clickedField int = -1
	switch {
	case relY >= 3 && relY <= 4:
		clickedField = formFieldTitle
	case relY >= 6 && relY <= 9:
		clickedField = formFieldDescription
	case relY >= 11 && relY <= 13:
		clickedField = formFieldBranch
	case relY >= 15 && relY <= 17:
		clickedField = formFieldLabels
	case relY >= 19 && relY <= 21:
		clickedField = formFieldPriority
	case relY >= 23:
		clickedField = formFieldProject
	}

	if clickedField >= 0 && msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
		m.blurAllFormFields()
		m.ticketFormField = clickedField
		m.focusCurrentField()

		if clickedField == formFieldProject && !m.showAddProjectForm {
			projects := m.globalStore.Projects()
			projectRelY := relY - 24
			if projectRelY >= 0 && projectRelY <= len(projects) {
				m.projectListIndex = projectRelY
				if projectRelY == len(projects) {
					m.showAddProjectForm = true
					m.addProjectPath.SetValue("")
					m.addProjectPath.Focus()
					return m, textinput.Blink
				}
				if projectRelY < len(projects) {
					m.selectedProject = projects[projectRelY]
				}
			}
		}
	}

	var cmd tea.Cmd
	switch m.ticketFormField {
	case formFieldTitle:
		m.titleInput, cmd = m.titleInput.Update(msg)
	case formFieldDescription:
		m.descInput, cmd = m.descInput.Update(msg)
	case formFieldBranch:
		if !m.branchLocked {
			m.branchInput, cmd = m.branchInput.Update(msg)
		}
	case formFieldLabels:
		m.labelsInput, cmd = m.labelsInput.Update(msg)
	}

	return m, cmd
}

func (m *Model) handleCreateTicketMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	return m.handleTicketForm(msg, false)
}

func (m *Model) handleEditTicketMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	return m.handleTicketForm(msg, true)
}

func (m *Model) handleTicketForm(msg tea.KeyMsg, isEdit bool) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		m.mode = ModeNormal
		m.blurAllFormFields()
		m.editingTicketID = ""
		m.branchLocked = false
		m.showAddProjectForm = false
		return m, nil

	case "tab":
		if m.showAddProjectForm && m.addProjectPath.Value() != "" {
			m.createProjectFromPath()
			if m.showAddProjectForm {
				return m, nil
			}
		} else if m.showAddProjectForm {
			m.showAddProjectForm = false
		}
		return m.nextFormField(isEdit), nil
	case "shift+tab":
		if m.showAddProjectForm {
			m.showAddProjectForm = false
		}
		return m.prevFormField(isEdit), nil

	case "ctrl+s":
		return m.saveTicketForm(isEdit)

	case "enter":
		if m.ticketFormField == formFieldTitle {
			return m.saveTicketForm(isEdit)
		}
		if m.ticketFormField == formFieldProject {
			return m.handleProjectSelection()
		}

	case "esc":
		if m.showAddProjectForm {
			m.showAddProjectForm = false
			m.addProjectPath.Blur()
			return m, nil
		}
		m.mode = ModeNormal
		m.blurAllFormFields()
		m.editingTicketID = ""
		m.branchLocked = false
		return m, nil
	}

	var cmd tea.Cmd
	switch m.ticketFormField {
	case formFieldTitle:
		m.titleInput, cmd = m.titleInput.Update(msg)
	case formFieldDescription:
		m.descInput, cmd = m.descInput.Update(msg)
	case formFieldBranch:
		if !m.branchLocked {
			m.branchInput, cmd = m.branchInput.Update(msg)
		}
	case formFieldLabels:
		m.labelsInput, cmd = m.labelsInput.Update(msg)
	case formFieldPriority:
		cmd = m.handlePriorityNav(msg)
	case formFieldWorktree:
		cmd = m.handleWorktreeToggle(msg)
	case formFieldAgent:
		if !m.agentLocked {
			cmd = m.handleAgentNav(msg)
		}
	case formFieldBlockedBy:
		cmd = m.handleBlockerNav(msg)
	case formFieldProject:
		if m.showAddProjectForm {
			m.addProjectPath, cmd = m.addProjectPath.Update(msg)
		} else {
			cmd = m.handleProjectListNav(msg)
		}
	}
	return m, cmd
}

func (m *Model) handlePriorityNav(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "j", "down", "l", "right":
		m.ticketPriority++
		if m.ticketPriority > 5 {
			m.ticketPriority = 1
		}
	case "k", "up", "h", "left":
		m.ticketPriority--
		if m.ticketPriority < 1 {
			m.ticketPriority = 5
		}
	case "1", "2", "3", "4", "5":
		m.ticketPriority = int(msg.String()[0] - '0')
	}
	return nil
}

func (m *Model) handleWorktreeToggle(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case " ", "enter", "h", "l", "left", "right":
		m.ticketUseWorktree = !m.ticketUseWorktree
	case "y", "Y":
		m.ticketUseWorktree = true
	case "n", "N":
		m.ticketUseWorktree = false
	}
	return nil
}

func (m *Model) handleAgentNav(msg tea.KeyMsg) tea.Cmd {
	agents := m.getAgentNames()
	if len(agents) == 0 {
		return nil
	}

	switch msg.String() {
	case "j", "down", "l", "right":
		m.agentListIndex++
		if m.agentListIndex >= len(agents) {
			m.agentListIndex = 0
		}
	case "k", "up", "h", "left":
		m.agentListIndex--
		if m.agentListIndex < 0 {
			m.agentListIndex = len(agents) - 1
		}
	}
	m.ticketAgent = agents[m.agentListIndex]
	return nil
}

func (m *Model) handleProjectListNav(msg tea.KeyMsg) tea.Cmd {
	projects := m.globalStore.Projects()
	maxIndex := len(projects)

	switch msg.String() {
	case "j", "down":
		m.projectListIndex++
		if m.projectListIndex > maxIndex {
			m.projectListIndex = 0
		}
	case "k", "up":
		m.projectListIndex--
		if m.projectListIndex < 0 {
			m.projectListIndex = maxIndex
		}
	case "d":
		if m.projectListIndex < len(projects) {
			m.confirmDeleteProject(projects[m.projectListIndex])
		}
	}

	// Auto-select the highlighted project (if not on "+ Add project" option)
	if m.projectListIndex < len(projects) {
		m.selectedProject = projects[m.projectListIndex]
	}

	return nil
}

func (m *Model) handleBlockerNav(msg tea.KeyMsg) tea.Cmd {
	visibleCandidates := m.getFilteredBlockerCandidates()

	switch msg.Type {
	case tea.KeyDown, tea.KeyCtrlN:
		if len(visibleCandidates) > 0 {
			m.blockerListIndex++
			if m.blockerListIndex >= len(visibleCandidates) {
				m.blockerListIndex = 0
			}
		}
		return nil
	case tea.KeyUp, tea.KeyCtrlP:
		if len(visibleCandidates) > 0 {
			m.blockerListIndex--
			if m.blockerListIndex < 0 {
				m.blockerListIndex = len(visibleCandidates) - 1
			}
		}
		return nil
	case tea.KeySpace, tea.KeyEnter:
		if m.blockerListIndex < len(visibleCandidates) {
			ticket := visibleCandidates[m.blockerListIndex]
			if m.selectedBlockers[ticket.ID] {
				delete(m.selectedBlockers, ticket.ID)
			} else {
				m.selectedBlockers[ticket.ID] = true
			}
		}
		return nil
	}

	var cmd tea.Cmd
	m.blockerFilterInput, cmd = m.blockerFilterInput.Update(msg)

	newVisible := m.getFilteredBlockerCandidates()
	if m.blockerListIndex >= len(newVisible) && len(newVisible) > 0 {
		m.blockerListIndex = len(newVisible) - 1
	} else if len(newVisible) == 0 {
		m.blockerListIndex = 0
	}

	return cmd
}

func (m *Model) getFilteredBlockerCandidates() []*board.Ticket {
	filterVal := m.blockerFilterInput.Value()
	if filterVal == "" {
		return m.blockerCandidates
	}

	var visible []*board.Ticket
	for _, t := range m.blockerCandidates {
		if strings.Contains(strings.ToLower(t.Title), strings.ToLower(filterVal)) {
			visible = append(visible, t)
		}
	}
	return visible
}

func (m *Model) initBlockerCandidates(excludeTicketID board.TicketID) {
	m.blockerCandidates = nil
	for _, ticket := range m.globalStore.All() {
		if ticket.ID == excludeTicketID {
			continue
		}
		if ticket.Status == board.StatusArchived {
			continue
		}
		m.blockerCandidates = append(m.blockerCandidates, ticket)
	}
	sort.Slice(m.blockerCandidates, func(i, j int) bool {
		return m.blockerCandidates[i].Title < m.blockerCandidates[j].Title
	})
}

func (m *Model) collectSelectedBlockers() []board.TicketID {
	var blockers []board.TicketID
	for id := range m.selectedBlockers {
		blockers = append(blockers, id)
	}
	sort.Slice(blockers, func(i, j int) bool {
		return string(blockers[i]) < string(blockers[j])
	})
	return blockers
}

func (m *Model) confirmDeleteProject(p *project.Project) {
	ticketCount := 0
	for _, t := range m.globalStore.All() {
		if t.ProjectID == p.ID {
			ticketCount++
		}
	}

	if ticketCount > 0 {
		m.confirmMsg = fmt.Sprintf("Delete '%s' and its %d ticket(s)?", p.Name, ticketCount)
	} else {
		m.confirmMsg = fmt.Sprintf("Delete project '%s'?", p.Name)
	}

	m.showConfirm = true
	m.confirmFn = func() tea.Cmd {
		if err := m.globalStore.RemoveProject(p.ID); err != nil {
			m.notify("Failed to delete: " + err.Error())
			return nil
		}
		delete(m.worktreeMgrs, p.ID)

		projects := m.globalStore.Projects()
		if len(projects) > 0 {
			if m.projectListIndex >= len(projects) {
				m.projectListIndex = len(projects) - 1
			}
			m.selectedProject = projects[m.projectListIndex]
		} else {
			m.selectedProject = nil
		}

		delete(m.filterProjectIDs, p.ID)

		m.notify("Deleted: " + p.Name)
		return nil
	}
}

func (m *Model) handleProjectSelection() (tea.Model, tea.Cmd) {
	projects := m.globalStore.Projects()

	if m.showAddProjectForm {
		return m.createProjectFromPath()
	}

	if m.projectListIndex < len(projects) {
		m.selectedProject = projects[m.projectListIndex]
		return m, nil
	}

	m.showAddProjectForm = true
	m.addProjectPath.SetValue("")
	m.addProjectPath.Focus()
	return m, textinput.Blink
}

func (m *Model) createProjectFromPath() (tea.Model, tea.Cmd) {
	path := strings.TrimSpace(m.addProjectPath.Value())
	if path == "" {
		m.notify("Path cannot be empty")
		return m, nil
	}

	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			path = filepath.Join(home, path[2:])
		}
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		m.notify("Invalid path: " + err.Error())
		return m, nil
	}

	gitDir := filepath.Join(absPath, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		m.notify("Not a git repository")
		return m, nil
	}

	name := filepath.Base(absPath)

	newProject := project.NewProject(name, absPath)
	// Project settings only store explicit user overrides.
	// Empty values cascade to global config via getDefaultAgent() and GetBranchPrefix().

	if err := m.projectRegistry.Add(newProject); err != nil {
		m.notify("Failed to save: " + err.Error())
		return m, nil
	}

	m.globalStore.AddProject(newProject)
	m.worktreeMgrs[newProject.ID] = git.NewWorktreeManager(newProject)
	m.selectedProject = newProject
	m.showAddProjectForm = false
	m.addProjectPath.Blur()
	m.projectListIndex = len(m.globalStore.Projects()) - 1

	if m.mode == ModeCreateProject {
		m.mode = ModeNormal
	}

	m.notify("Added project: " + name)
	return m, nil
}

func (m *Model) nextFormField(isEdit bool) *Model {
	m.blurAllFormFields()
	m.ticketFormField++

	for {
		if m.ticketFormField > formFieldProject {
			m.ticketFormField = formFieldTitle
		}
		if m.ticketFormField == formFieldBranch && m.branchLocked {
			m.ticketFormField++
			continue
		}
		if m.ticketFormField == formFieldAgent && m.agentLocked {
			m.ticketFormField++
			continue
		}
		break
	}
	m.focusCurrentField()
	return m
}

func (m *Model) prevFormField(isEdit bool) *Model {
	m.blurAllFormFields()
	m.ticketFormField--

	for {
		if m.ticketFormField < formFieldTitle {
			m.ticketFormField = formFieldProject
		}
		if m.ticketFormField == formFieldBranch && m.branchLocked {
			m.ticketFormField--
			continue
		}
		if m.ticketFormField == formFieldAgent && m.agentLocked {
			m.ticketFormField--
			continue
		}
		break
	}
	m.focusCurrentField()
	return m
}

func (m *Model) blurAllFormFields() {
	m.titleInput.Blur()
	m.descInput.Blur()
	m.branchInput.Blur()
	m.labelsInput.Blur()
	m.blockerFilterInput.Blur()
	m.projectInput.Blur()
}

func (m *Model) focusCurrentField() {
	switch m.ticketFormField {
	case formFieldTitle:
		m.titleInput.Focus()
	case formFieldDescription:
		m.descInput.Focus()
	case formFieldBranch:
		m.branchInput.Focus()
	case formFieldLabels:
		m.labelsInput.Focus()
	case formFieldPriority:
		break
	case formFieldWorktree:
		break
	case formFieldBlockedBy:
		m.blockerFilterInput.Focus()
	case formFieldProject:
		m.projectInput.Focus()
	}
}

func (m *Model) saveTicketForm(isEdit bool) (tea.Model, tea.Cmd) {
	title := strings.TrimSpace(m.titleInput.Value())
	if title == "" {
		m.notify("Title cannot be empty")
		return m, nil
	}

	if m.selectedProject == nil {
		m.notify("No project selected")
		return m, nil
	}

	desc := strings.TrimSpace(m.descInput.Value())
	branchName := strings.TrimSpace(m.branchInput.Value())
	if branchName == "" {
		branchName = m.generateBranchNameFromTitle(title, m.selectedProject)
	}

	labels := m.parseLabels(m.labelsInput.Value())

	blockedBy := m.collectSelectedBlockers()

	if isEdit && m.editingTicketID != "" {
		ticket, _ := m.globalStore.Get(m.editingTicketID)
		if ticket != nil {
			if ticket.ProjectID != m.selectedProject.ID {
				if err := m.globalStore.MoveProject(ticket.ID, m.selectedProject.ID); err != nil {
					switch err {
					case project.ErrTicketHasWorktree:
						m.notify("Cannot change project: ticket has an active worktree")
					default:
						m.notify("Failed to change project: " + err.Error())
					}
					return m, nil
				}
			}
			ticket.Title = title
			ticket.Description = desc
			if !m.branchLocked {
				ticket.BranchName = branchName
			}
			ticket.Labels = labels
			ticket.Priority = m.ticketPriority
			ticket.UseWorktree = m.ticketUseWorktree
			if !m.agentLocked {
				ticket.AgentType = m.ticketAgent
			}
			ticket.BlockedBy = blockedBy
			ticket.Touch()
			m.saveTicket(ticket)
			m.refreshColumnTickets()
			m.notify("Updated: " + title)
		}
	} else {
		ticket := board.NewTicket(title, m.selectedProject.ID)
		ticket.Description = desc
		ticket.BranchName = branchName
		ticket.Labels = labels
		ticket.Priority = m.ticketPriority
		ticket.UseWorktree = m.ticketUseWorktree
		ticket.AgentType = m.ticketAgent
		ticket.BlockedBy = blockedBy
		ticket.Status = m.columns[m.activeColumn].Status
		m.globalStore.Add(ticket)
		m.refreshColumnTickets()
		m.selectTicketByID(ticket.ID)
		m.saveTicket(ticket)
		m.notify("Created: " + title)
	}

	m.mode = ModeNormal
	m.blurAllFormFields()
	m.editingTicketID = ""
	m.branchLocked = false
	return m, nil
}

func (m *Model) parseLabels(input string) []string {
	if strings.TrimSpace(input) == "" {
		return []string{}
	}
	parts := strings.Split(input, ",")
	var labels []string
	for _, p := range parts {
		label := strings.TrimSpace(p)
		if label != "" {
			labels = append(labels, label)
		}
	}
	return labels
}

type settingsField struct {
	key         string
	label       string
	kind        string
	description string
}

var settingsFields = []settingsField{
	{"theme", "Theme", "theme", "Color theme for the UI"},
	{"default_agent", "Default Agent", "agent", "Agent to spawn for new tickets (opencode, claude, aider)"},
	{"confirm_quit", "Confirm Quit", "toggle", "Prompt before quitting with running agents"},
	{"branch_prefix", "Branch Prefix", "text", "Prefix for auto-generated branch names (e.g. task/, feature/)"},
	{"delete_worktree", "Delete Worktree", "toggle", "Remove git worktree when deleting tickets"},
	{"delete_branch", "Delete Branch", "toggle", "Delete git branch when deleting tickets"},
	{"force_cleanup", "Force Cleanup", "toggle", "Force worktree removal even with uncommitted changes"},
	{"sidebar_visible", "Show Sidebar", "toggle", "Toggle the project sidebar visibility"},
	{"filter_project", "Filter Project", "project", "Show only tickets from a specific project"},
}

func (m *Model) handleSettingsMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.settingsEditing {
		return m.handleSettingsEdit(msg)
	}

	switch msg.String() {
	case "j", "down":
		m.settingsIndex++
		if m.settingsIndex >= len(settingsFields) {
			m.settingsIndex = len(settingsFields) - 1
		}
	case "k", "up":
		m.settingsIndex--
		m.settingsIndex = max(m.settingsIndex, 0)
	case "enter", " ":
		return m.enterSettingsEdit()
	case "esc", "q":
		m.mode = ModeNormal
	}
	return m, nil
}

func (m *Model) handleSettingsEdit(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	field := settingsFields[m.settingsIndex]

	if field.kind == "project" {
		m.filterInput.SetValue(m.filterQuery)
		m.filterInput.Focus()
		m.mode = ModeFilter
		return m, textinput.Blink
	}

	if field.kind == "theme" {
		return m.handleThemeNav(msg)
	}

	switch msg.String() {
	case "enter":
		m.applySettingsValue(field.key, m.settingsInput.Value())
		m.settingsEditing = false
		m.settingsInput.Blur()
		m.notify("Settings saved")
		return m, nil
	case "esc":
		m.settingsEditing = false
		m.settingsInput.Blur()
		return m, nil
	}

	var cmd tea.Cmd
	m.settingsInput, cmd = m.settingsInput.Update(msg)
	return m, cmd
}

func (m *Model) handleThemeNav(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	themes := config.ThemeNames()
	if len(themes) == 0 {
		return m, nil
	}

	switch msg.String() {
	case "j", "down":
		m.themeListIndex++
		if m.themeListIndex >= len(themes) {
			m.themeListIndex = 0
		}
		m.applySettingsValue("theme", themes[m.themeListIndex])
	case "k", "up":
		m.themeListIndex--
		if m.themeListIndex < 0 {
			m.themeListIndex = len(themes) - 1
		}
		m.applySettingsValue("theme", themes[m.themeListIndex])
	case "enter":
		m.settingsEditing = false
		m.notify("Theme: " + themes[m.themeListIndex])
	case "esc":
		m.settingsEditing = false
	}

	return m, nil
}

func (m *Model) handleSettingsMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return m, nil
	}

	formTop := (m.height - 10) / 2
	relY := msg.Y - formTop - 3

	if relY >= 0 && relY < len(settingsFields) {
		m.settingsIndex = relY
		return m.enterSettingsEdit()
	}

	return m, nil
}

func (m *Model) handleConfirmMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return m, nil
	}

	formCenterY := m.height / 2
	formCenterX := m.width / 2

	yesX := formCenterX - 10
	noX := formCenterX + 5

	if msg.Y == formCenterY+2 {
		if msg.X >= yesX && msg.X <= yesX+5 {
			m.showConfirm = false
			if m.confirmFn != nil {
				return m, m.confirmFn()
			}
		}
		if msg.X >= noX && msg.X <= noX+4 {
			m.showConfirm = false
		}
	}

	return m, nil
}

func (m *Model) enterSettingsEdit() (tea.Model, tea.Cmd) {
	field := settingsFields[m.settingsIndex]

	switch field.kind {
	case "project":
		m.filterInput.SetValue(m.filterQuery)
		m.filterInput.Focus()
		m.mode = ModeFilter
		return m, textinput.Blink

	case "toggle":
		m.applySettingsValue(field.key, "")
		status := m.getSettingsValue(field.key)
		m.notify(field.label + ": " + status)
		return m, nil

	case "theme":
		themes := config.ThemeNames()
		current := m.config.UI.Theme
		m.themeListIndex = 0
		for i, t := range themes {
			if t == current {
				m.themeListIndex = i
				break
			}
		}
		m.settingsEditing = true
		return m, nil

	case "agent":
		agents := m.getAgentNames()
		current := m.config.Defaults.DefaultAgent
		currentIndex := 0
		for i, a := range agents {
			if a == current {
				currentIndex = i
				break
			}
		}
		nextIndex := (currentIndex + 1) % len(agents)
		nextAgent := agents[nextIndex]
		m.applySettingsValue(field.key, nextAgent)
		m.notify("Default agent: " + nextAgent)
		return m, nil

	case "text":
		m.settingsEditing = true
		m.settingsInput.SetValue(m.getSettingsValue(field.key))
		m.settingsInput.Focus()
		return m, textinput.Blink

	default:
		m.settingsEditing = true
		m.settingsInput.SetValue(m.getSettingsValue(field.key))
		m.settingsInput.Focus()
		return m, textinput.Blink
	}
}

func (m *Model) getSettingsValue(key string) string {
	switch key {
	case "theme":
		return m.config.UI.Theme
	case "default_agent":
		return m.config.Defaults.DefaultAgent
	case "confirm_quit":
		if m.config.Behavior.ConfirmQuitWithAgents {
			return "On"
		}
		return "Off"
	case "branch_prefix":
		return m.config.Defaults.BranchPrefix
	case "delete_worktree":
		if m.config.Cleanup.DeleteWorktree {
			return "On"
		}
		return "Off"
	case "delete_branch":
		if m.config.Cleanup.DeleteBranch {
			return "On"
		}
		return "Off"
	case "force_cleanup":
		if m.config.Cleanup.ForceWorktreeRemoval {
			return "On"
		}
		return "Off"
	case "filter_project":
		count := len(m.filterProjectIDs)
		if count == 0 {
			return "All Projects"
		}
		return fmt.Sprintf("%d selected", count)
	case "sidebar_visible":
		if m.sidebarVisible {
			return "On"
		}
		return "Off"
	}
	return ""
}

func (m *Model) applySettingsValue(key, value string) {
	switch key {
	case "theme":
		m.config.UI.Theme = value
		m.theme = m.config.GetTheme()
		m.colors = newUIColors(m.theme)
		m.config.Save("")
	case "default_agent":
		m.config.Defaults.DefaultAgent = value
		m.config.Save("")
	case "confirm_quit":
		m.config.Behavior.ConfirmQuitWithAgents = !m.config.Behavior.ConfirmQuitWithAgents
		m.config.Save("")
	case "branch_prefix":
		m.config.Defaults.BranchPrefix = value
		m.config.Save("")
	case "delete_worktree":
		m.config.Cleanup.DeleteWorktree = !m.config.Cleanup.DeleteWorktree
		m.config.Save("")
	case "delete_branch":
		m.config.Cleanup.DeleteBranch = !m.config.Cleanup.DeleteBranch
		m.config.Save("")
	case "force_cleanup":
		m.config.Cleanup.ForceWorktreeRemoval = !m.config.Cleanup.ForceWorktreeRemoval
		m.config.Save("")
	case "sidebar_visible":
		m.sidebarVisible = !m.sidebarVisible
		m.config.UI.SidebarVisible = m.sidebarVisible
		if !m.sidebarVisible {
			m.sidebarFocused = false
		}
		m.config.Save("")
	}
}

func (m *Model) handleFilterMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		m.filterInput.Blur()
		m.mode = ModeNormal
		return m, nil
	case "esc":
		m.filterQuery = ""
		m.filterInput.SetValue("")
		m.filterInput.Blur()
		m.mode = ModeNormal
		m.refreshColumnTickets()
		return m, nil
	}

	var cmd tea.Cmd
	m.filterInput, cmd = m.filterInput.Update(msg)
	m.filterQuery = m.filterInput.Value()
	m.refreshColumnTickets()
	return m, cmd
}

func (m *Model) handleFilterMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	m.filterInput, cmd = m.filterInput.Update(msg)
	return m, cmd
}

func (m *Model) handleCreateProjectMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		return m.createProjectFromPath()
	case "esc":
		m.mode = ModeNormal
		m.addProjectPath.Blur()
		return m, nil
	case "ctrl+c":
		m.mode = ModeNormal
		m.addProjectPath.Blur()
		return m, nil
	}

	var cmd tea.Cmd
	m.addProjectPath, cmd = m.addProjectPath.Update(msg)
	return m, cmd
}

func (m *Model) clearFilter() {
	m.filterQuery = ""
	m.filterProjectIDs = make(map[string]bool)
	m.refreshColumnTickets()
}

func (m *Model) toggleProjectFilter(projectID string) {
	if m.filterProjectIDs[projectID] {
		delete(m.filterProjectIDs, projectID)
	} else {
		m.filterProjectIDs[projectID] = true
	}
	m.filterQuery = ""
	m.refreshColumnTickets()
}

func (m *Model) toggleAllProjects() {
	projects := m.globalStore.Projects()
	allSelected := len(m.filterProjectIDs) == len(projects) && len(projects) > 0
	for _, p := range projects {
		if !m.filterProjectIDs[p.ID] {
			allSelected = false
			break
		}
	}

	if allSelected || len(m.filterProjectIDs) == 0 {
		m.filterProjectIDs = make(map[string]bool)
		for _, p := range projects {
			m.filterProjectIDs[p.ID] = true
		}
		m.notify("All projects selected")
	} else {
		m.filterProjectIDs = make(map[string]bool)
		m.notify("All projects deselected")
	}
	m.filterQuery = ""
	m.refreshColumnTickets()
}

func (m *Model) moveColumn(delta int) {
	m.activeColumn += delta
	m.activeColumn = max(m.activeColumn, 0)
	if m.activeColumn >= len(m.columns) {
		m.activeColumn = len(m.columns) - 1
	}
	m.activeTicket = 0
	m.ensureColumnVisible()
	m.ensureTicketVisible()
}

func (m *Model) ensureColumnVisible() {
	colWidth := m.calcColumnWidth()
	visibleCols := m.visibleColumnCount(colWidth)

	if m.activeColumn < m.scrollOffset {
		m.scrollOffset = m.activeColumn
	} else if m.activeColumn >= m.scrollOffset+visibleCols {
		m.scrollOffset = m.activeColumn - visibleCols + 1
	}

	maxOffset := max(len(m.columns)-visibleCols, 0)
	if m.scrollOffset > maxOffset {
		m.scrollOffset = maxOffset
	}
}

func (m *Model) headerHeight() int {
	const (
		content      = 1
		borderBottom = 1
		spacing      = 2
	)
	return content + borderBottom + spacing
}

func (m *Model) calcColumnWidth() int {
	boardW := m.boardWidth()
	if boardW == 0 || len(m.columns) == 0 {
		return minColumnWidth
	}

	numCols := len(m.columns)
	totalOverhead := numCols * columnOverhead
	colWidth := (boardW - totalOverhead) / numCols

	return max(colWidth, minColumnWidth)
}

func (m *Model) visibleColumnCount(colWidth int) int {
	boardW := m.boardWidth()
	if boardW == 0 {
		return len(m.columns)
	}
	visible := boardW / (colWidth + columnOverhead)
	visible = max(visible, 1)
	if visible > len(m.columns) {
		visible = len(m.columns)
	}
	return visible
}

func (m *Model) distributeWidth(numCols int) (baseWidth, remainder int) {
	boardW := m.boardWidth()
	if numCols == 0 || boardW == 0 {
		return minColumnWidth, 0
	}
	borders := numCols * 2
	margins := numCols - 1
	available := boardW - borders - margins
	baseWidth = available / numCols
	remainder = available % numCols
	if baseWidth < minColumnWidth {
		baseWidth = minColumnWidth
		remainder = 0
	}
	return baseWidth, remainder
}

func (m *Model) moveTicket(delta int) {
	if len(m.columnTickets) <= m.activeColumn {
		return
	}
	tickets := m.columnTickets[m.activeColumn]
	m.activeTicket += delta
	m.activeTicket = max(m.activeTicket, 0)
	if m.activeTicket >= len(tickets) {
		m.activeTicket = max(len(tickets)-1, 0)
	}
	m.ensureTicketVisible()
}

// boardAreaHeight is the vertical space available for the column row, between
// the header (with its trailing newline) and the status bar (with its
// preceding newline). headerHeight() already includes its own padding/border.
func (m *Model) boardAreaHeight() int {
	const (
		newlineAfterHeader      = 1
		newlineBeforeStatusBar  = 1
		statusBarHeight         = 1
	)
	return m.height - m.headerHeight() - newlineAfterHeader - newlineBeforeStatusBar - statusBarHeight
}

func (m *Model) columnContentHeight() int {
	const (
		columnBottomBorder = 1
	)
	return m.boardAreaHeight() - columnHeaderHeight - columnBottomBorder
}

func (m *Model) ensureTicketVisible() {
	if m.activeColumn < 0 || m.activeColumn >= len(m.columnOffsets) {
		return
	}

	offset := m.columnOffsets[m.activeColumn]

	// Scroll up if the active ticket is above the visible window.
	if m.activeTicket < offset {
		m.columnOffsets[m.activeColumn] = m.activeTicket
		return
	}

	// Scroll down if the active ticket would fall outside the rendered
	// budget at the current offset. Walk per-ticket heights (or fall back
	// to ticketHeight if the heights cache hasn't been populated yet, e.g.
	// before the first View() after a window-size change).
	var heights []int
	if m.activeColumn < len(m.columnTicketHeights) {
		heights = m.columnTicketHeights[m.activeColumn]
	}
	heightOf := func(i int) int {
		if i < len(heights) && heights[i] > 0 {
			return heights[i]
		}
		return ticketHeight
	}

	budget := m.columnContentHeight()
	if offset > 0 {
		budget -= 1 // ▲ indicator
	}

	// From the current offset, see if activeTicket fits. If not, advance
	// the offset one ticket at a time until it does.
	for {
		fits := false
		used := 0
		// Account for trailing ▼ indicator if there's anything after the
		// active ticket — keep the active card off the indicator row.
		for i := offset; i < len(m.columnTickets[m.activeColumn]); i++ {
			cost := heightOf(i)
			reserve := 0
			if i < len(m.columnTickets[m.activeColumn])-1 {
				reserve = 1
			}
			if used+cost+reserve > budget {
				break
			}
			used += cost
			if i == m.activeTicket {
				fits = true
				break
			}
		}
		if fits || offset >= m.activeTicket {
			break
		}
		offset++
		// Recompute budget: once offset > 0 the ▲ indicator row is reserved.
		budget = m.columnContentHeight() - 1
	}

	m.columnOffsets[m.activeColumn] = max(offset, 0)
}

func (m *Model) createNewTicket() (tea.Model, tea.Cmd) {
	m.mode = ModeCreateTicket
	m.ticketFormField = formFieldTitle
	m.editingTicketID = ""
	m.branchLocked = false
	m.agentLocked = false
	m.showAddProjectForm = false

	if len(m.filterProjectIDs) == 1 {
		for id := range m.filterProjectIDs {
			m.selectedProject = m.globalStore.GetProject(id)
			break
		}
	} else if m.selectedProject == nil {
		projects := m.globalStore.Projects()
		if len(projects) > 0 {
			m.selectedProject = projects[0]
		}
	}

	m.projectListIndex = 0
	if m.selectedProject != nil {
		for i, p := range m.globalStore.Projects() {
			if p.ID == m.selectedProject.ID {
				m.projectListIndex = i
				break
			}
		}
	}

	m.ticketAgent = m.getDefaultAgent()
	m.agentListIndex = m.getAgentIndex(m.ticketAgent)

	m.titleInput.Reset()
	m.descInput.Reset()
	m.branchInput.Reset()
	m.labelsInput.Reset()
	m.ticketPriority = 3
	m.ticketUseWorktree = true

	m.initBlockerCandidates("")
	m.selectedBlockers = make(map[board.TicketID]bool)
	m.blockerListIndex = 0
	m.blockerFilterInput.Reset()
	m.formScrollOffset = 0

	m.blurAllFormFields()
	m.titleInput.Focus()
	return m, m.titleInput.Cursor.BlinkCmd()
}

func (m *Model) editTicket() (tea.Model, tea.Cmd) {
	ticket := m.selectedTicket()
	if ticket == nil {
		m.notify("No ticket selected")
		return m, nil
	}

	m.mode = ModeEditTicket
	m.ticketFormField = formFieldTitle
	m.editingTicketID = ticket.ID
	m.branchLocked = ticket.WorktreePath != ""
	m.agentLocked = ticket.AgentSpawnedAt != nil
	m.selectedProject = m.globalStore.GetProjectForTicket(ticket)
	m.projectListIndex = 0
	if m.selectedProject != nil {
		for i, p := range m.globalStore.Projects() {
			if p.ID == m.selectedProject.ID {
				m.projectListIndex = i
				break
			}
		}
	}
	m.showAddProjectForm = false
	m.titleInput.SetValue(ticket.Title)
	m.descInput.SetValue(ticket.Description)
	if ticket.BranchName != "" {
		m.branchInput.SetValue(ticket.BranchName)
	} else if m.selectedProject != nil {
		m.branchInput.SetValue(m.generateBranchNameFromTitle(ticket.Title, m.selectedProject))
	}
	m.labelsInput.SetValue(strings.Join(ticket.Labels, ", "))
	m.ticketPriority = ticket.Priority
	if m.ticketPriority < 1 || m.ticketPriority > 5 {
		m.ticketPriority = 3
	}
	m.ticketUseWorktree = ticket.UseWorktree
	if ticket.AgentType != "" {
		m.ticketAgent = ticket.AgentType
	} else {
		m.ticketAgent = m.getDefaultAgent()
	}
	m.agentListIndex = m.getAgentIndex(m.ticketAgent)

	m.initBlockerCandidates(ticket.ID)
	m.selectedBlockers = make(map[board.TicketID]bool)
	for _, blockerID := range ticket.BlockedBy {
		m.selectedBlockers[blockerID] = true
	}
	m.blockerListIndex = 0
	m.blockerFilterInput.Reset()
	m.formScrollOffset = 0

	m.blurAllFormFields()
	m.titleInput.Focus()
	return m, m.titleInput.Cursor.BlinkCmd()
}

// attachExisting performs the daemon attach (or takeover, if another
// client owns the binary stream) for a PaneView the model already
// holds. Returns a tea.Cmd that runs the attach in the background and
// then arms the pane's tea message reader.
//
// Used by:
//   - spawnAgent (Enter/s on a daemon-owned ticket from board view)
//   - handleAgentViewMode (AttachFirstMsg fallback when the user types
//     into an unattached pane)
//   - Update's AttachFirstMsg routing.
func (m *Model) attachExisting(ticketID board.TicketID, pv *daemonclient.PaneView) tea.Cmd {
	if pv == nil || m.daemonClient == nil {
		return nil
	}
	// Decide attach vs takeover based on the most recent List snapshot.
	// We don't List again here — the model already has the info that
	// was attached at construction or refresh time. If another client
	// holds the binary stream, the daemon would reject a plain Attach;
	// Takeover unconditionally displaces them. Per the design, takeover
	// is the desired UX (no destructive prompt).
	takeover := false
	if m.daemonClient != nil {
		listCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		resp, err := m.daemonClient.List(listCtx)
		cancel()
		if err == nil {
			for _, s := range resp.Sessions {
				if s.SessionID != pv.SessionID() {
					continue
				}
				if s.AttachedClient != 0 && s.AttachedClient != m.daemonClient.ClientID() {
					takeover = true
				}
				pv.Refresh(s)
				break
			}
		}
	}
	id := ticketID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var err error
		if takeover {
			err = pv.Takeover(ctx)
		} else {
			err = pv.Attach(ctx)
		}
		if err != nil {
			return spawnErrorMsg{ticketID: id, err: "attach failed: " + err.Error()}
		}
		// Drain one message from the pane's tea channel so the update
		// loop keeps spinning.
		select {
		case msg, ok := <-pv.TeaMessages():
			if !ok {
				return daemonclient.PaneExitMsg{PaneID: pv.ID(), Err: io.EOF}
			}
			return msg
		case <-time.After(50 * time.Millisecond):
			// No event yet — return a synthetic attached message so the
			// model arms the reader.
			return daemonclient.PaneAttachedMsg{PaneID: pv.ID()}
		}
	}
}

func (m *Model) handleDoubleClick() (tea.Model, tea.Cmd) {
	// spawnAgent now dispatches to spawn vs attach based on the pane
	// state itself, so the double-click handler doesn't need to
	// pre-decide. Same behavior as 's' / Enter on the board view.
	return m.spawnAgent()
}

func (m *Model) confirmDeleteTicket() (tea.Model, tea.Cmd) {
	ticket := m.selectedTicket()
	if ticket == nil {
		return m, nil
	}

	proj := m.globalStore.GetProjectForTicket(ticket)
	hasUncommitted := false
	if ticket.WorktreePath != "" && m.config.Cleanup.DeleteWorktree && proj != nil {
		if mgr := m.worktreeMgrs[proj.ID]; mgr != nil {
			var err error
			hasUncommitted, err = mgr.HasUncommittedChanges(ticket.WorktreePath)
			if err != nil {
				hasUncommitted = false
			}
		}
	}

	// performTicketCleanup honors config.Cleanup.DeleteBranch silently.
	// When that flag is false (the default), the branch survives and we
	// chain a follow-on confirm so the user has an obvious moment to
	// drop it without editing config first. Capture proj here — the
	// store entry is gone by the time the second confirm fires.
	doDelete := func() tea.Cmd {
		m.performTicketCleanup(ticket)
		if ticket.BranchName != "" && !m.config.Cleanup.DeleteBranch && proj != nil {
			branchName := ticket.BranchName
			projID := proj.ID
			m.showConfirm = true
			m.confirmMsg = "Also delete branch '" + branchName + "'? [y/N]"
			m.confirmFn = func() tea.Cmd {
				m.deleteBranchOnly(projID, branchName)
				return nil
			}
		}
		return nil
	}

	if hasUncommitted && !m.config.Cleanup.ForceWorktreeRemoval {
		m.showConfirm = true
		m.confirmMsg = "Worktree has uncommitted changes. Force delete?"
		m.confirmFn = doDelete
	} else {
		m.showConfirm = true
		m.confirmMsg = "Delete ticket: " + ticket.Title + "?"
		m.confirmFn = doDelete
	}
	return m, nil
}

// deleteBranchOnly removes the branch via the project's worktree
// manager. Invoked from the follow-on confirm after a ticket delete
// when config.Cleanup.DeleteBranch is false. Tolerates a missing
// manager (project unloaded between confirms) by no-op'ing silently.
func (m *Model) deleteBranchOnly(projID string, branchName string) {
	mgr := m.worktreeMgrs[projID]
	if mgr == nil {
		return
	}
	if err := mgr.DeleteBranch(branchName); err != nil {
		m.notify("Failed to delete branch: " + err.Error())
		return
	}
	m.notify("Deleted branch: " + branchName)
}

func (m *Model) performTicketCleanup(ticket *board.Ticket) {
	ticketTitle := ticket.Title // Capture before deletion

	if pane, ok := m.panes[ticket.ID]; ok {
		pane.Stop()
		delete(m.panes, ticket.ID)
	}

	proj := m.globalStore.GetProjectForTicket(ticket)
	if proj != nil {
		mgr := m.worktreeMgrs[proj.ID]
		if mgr != nil {
			if ticket.WorktreePath != "" && m.config.Cleanup.DeleteWorktree {
				err := mgr.RemoveWorktree(ticket.WorktreePath)
				if err != nil {
					m.notify("Failed to remove worktree: " + err.Error())
				}
			}

			if ticket.BranchName != "" && m.config.Cleanup.DeleteBranch {
				err := mgr.DeleteBranch(ticket.BranchName)
				if err != nil {
					m.notify("Failed to delete branch: " + err.Error())
				}
			}
		}
	}

	// SessionOwned=true is the ticket's explicit claim on the session
	// JSONL (set via `openkanban ticket new --session ... --migrate`).
	// Link-mode sessions (SessionOwned=false) belong to the spawning
	// agent and must survive ticket deletion. The pane.Stop above has
	// already killed the writer process, so unlink is safe.
	if ticket.SessionOwned && ticket.AgentSessionID != "" {
		path, err := agent.SessionPath(ticket.AgentSessionID)
		switch {
		case err == nil:
			if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
				m.notify("Failed to remove session file: " + rmErr.Error())
			}
		case !os.IsNotExist(err):
			m.notify("Failed to locate session file: " + err.Error())
		}
	}

	m.globalStore.RemoveBlockerReferences(ticket.ID)
	m.globalStore.Delete(ticket.ID)
	m.refreshColumnTickets()
	m.globalStore.SaveAll()
	m.notify("Deleted: " + ticketTitle)
}

func (m *Model) quickMoveTicket() (tea.Model, tea.Cmd) {
	ticket := m.selectedTicket()
	if ticket == nil {
		return m, nil
	}

	nextStatus := m.nextStatus(ticket.Status)
	if nextStatus == ticket.Status {
		return m, nil
	}

	if nextStatus == board.StatusInProgress && ticket.WorktreePath == "" {
		if ticket.UseWorktree {
			if err := m.setupWorktree(ticket); err != nil {
				m.notify("Worktree failed: " + err.Error())
				return m, nil
			}
		} else {
			if err := m.setupMainRepoBranch(ticket); err != nil {
				m.notify("Branch setup failed: " + err.Error())
				return m, nil
			}
		}
	}

	promoted, _ := m.globalStore.Move(ticket.ID, nextStatus)
	m.refreshColumnTickets()
	m.selectTicketByID(ticket.ID)
	m.saveTicket(ticket)
	m.notify(moveAndPromoteMsg(nextStatus, promoted))

	return m, nil
}

// adjustPriority shifts the active ticket's priority by delta (negative
// = raise, positive = lower) within the valid 1..5 range. Priority 1 is
// the highest, so "raise" maps to a smaller number. The selected ticket
// stays selected after the column rebuilds even when a priority sort
// would otherwise drift it under the cursor.
func (m *Model) adjustPriority(delta int) (tea.Model, tea.Cmd) {
	ticket := m.selectedTicket()
	if ticket == nil {
		m.notify("No ticket selected")
		return m, nil
	}

	current := effectivePriority(ticket.Priority)
	next := current + delta
	if next < 1 {
		m.notify("Already at highest priority")
		return m, nil
	}
	if next > 5 {
		m.notify("Already at lowest priority")
		return m, nil
	}

	ticket.Priority = next
	ticket.Touch()
	m.saveTicket(ticket)
	m.refreshColumnTickets()
	m.selectTicketByID(ticket.ID)

	if delta < 0 {
		m.notify(fmt.Sprintf("Priority raised to %d", next))
	} else {
		m.notify(fmt.Sprintf("Priority lowered to %d", next))
	}
	return m, nil
}

// cycleUnattachedSession moves the agent-view focus to the next
// (delta=+1) or previous (delta=-1) open session that this TUI is not
// currently attached to. Ordering is board order — the same order the
// user sees in columnTickets, so the cycle is predictable from the
// board layout. The currently-attached pane is excluded by definition
// (its State() is PaneViewAttached, not PaneViewUnattached); cycling
// from the modal landing state continues to advance through unattached
// peers and never returns to the original attached one. The user
// presses Esc to exit back to the board, from which the original pane
// is still attached and reachable via the normal flow.
//
// Sets cycleAttachPrompt so View renders the "press Enter to attach"
// modal — this absorbs the user's switch-to-session keystroke so it
// doesn't get eaten by the AttachFirstMsg handshake.
func (m *Model) cycleUnattachedSession(delta int) (tea.Model, tea.Cmd) {
	if delta != 1 && delta != -1 {
		return m, nil
	}

	var candidates []board.TicketID
	for _, col := range m.columnTickets {
		for _, t := range col {
			pv, ok := m.panes[t.ID]
			if !ok || pv == nil {
				continue
			}
			if pv.State() != daemonclient.PaneViewUnattached {
				continue
			}
			candidates = append(candidates, t.ID)
		}
	}

	if len(candidates) == 0 {
		m.notify("No other open sessions")
		return m, nil
	}

	cur := -1
	for i, id := range candidates {
		if id == m.focusedPane {
			cur = i
			break
		}
	}
	var next board.TicketID
	if cur == -1 {
		if delta > 0 {
			next = candidates[0]
		} else {
			next = candidates[len(candidates)-1]
		}
	} else {
		next = candidates[(cur+delta+len(candidates))%len(candidates)]
	}

	m.focusedPane = next
	if pv, ok := m.panes[next]; ok && pv != nil {
		pv.SetSize(m.width, m.height-2)
	}
	m.cycleAttachPrompt = true
	return m, m.maybeSetWindowTitle()
}

// cycleSortMode advances to the next sort mode and re-renders the board.
// The currently-selected ticket stays selected so the cursor doesn't
// jump to whatever happens to land at its old index after sorting.
func (m *Model) cycleSortMode() (tea.Model, tea.Cmd) {
	ticket := m.selectedTicket()
	m.sortMode = nextSortMode(m.sortMode)
	m.refreshColumnTickets()
	if ticket != nil {
		m.selectTicketByID(ticket.ID)
	}
	m.notify("Sort: " + sortModeLabel(m.sortMode))
	return m, nil
}

// cycleSessionFilter advances to the next session filter (all → open →
// waiting → all) and re-renders the board. Preserves selection like
// cycleSortMode; the selected ticket may scroll off-screen if it no
// longer matches the active filter, but its identity is retained so a
// subsequent cycle back to "all" restores the cursor where it was.
func (m *Model) cycleSessionFilter() (tea.Model, tea.Cmd) {
	ticket := m.selectedTicket()
	m.sessionFilter = nextSessionFilter(m.sessionFilter)
	m.refreshColumnTickets()
	if ticket != nil {
		m.selectTicketByID(ticket.ID)
	}
	m.notify("Filter: " + sessionFilterLabel(m.sessionFilter))
	return m, nil
}

func (m *Model) toggleAlwaysShowWorking() (tea.Model, tea.Cmd) {
	ticket := m.selectedTicket()
	m.alwaysShowWorking = !m.alwaysShowWorking
	m.refreshColumnTickets()
	if ticket != nil {
		m.selectTicketByID(ticket.ID)
	}
	state := "off"
	if m.alwaysShowWorking {
		state = "on"
	}
	m.notify("Always show working: " + state)
	return m, nil
}

func (m *Model) quickMoveTicketBackward() (tea.Model, tea.Cmd) {
	ticket := m.selectedTicket()
	if ticket == nil {
		return m, nil
	}

	prevStatus := m.previousStatus(ticket.Status)
	if prevStatus == ticket.Status {
		return m, nil
	}

	promoted, _ := m.globalStore.Move(ticket.ID, prevStatus)
	m.refreshColumnTickets()
	m.selectTicketByID(ticket.ID)
	m.saveTicket(ticket)
	m.notify(moveAndPromoteMsg(prevStatus, promoted))

	return m, nil
}

func (m *Model) setupWorktree(ticket *board.Ticket) error {
	proj := m.globalStore.GetProjectForTicket(ticket)
	if proj == nil {
		return fmt.Errorf("project not found for ticket")
	}

	mgr := m.worktreeMgrs[proj.ID]
	if mgr == nil {
		return fmt.Errorf("worktree manager not found")
	}

	branchName := m.generateBranchName(ticket, proj)
	baseBranch, _ := mgr.GetDefaultBranch()

	path, err := mgr.CreateWorktree(branchName, baseBranch)
	if err != nil {
		return err
	}

	if err := agent.SeedClaudeSettings(path, proj.RepoPath); err != nil {
		log.Printf("openkanban: seed claude settings (%s): %v", path, err)
	}

	ticket.WorktreePath = path
	ticket.BranchName = branchName
	ticket.BaseBranch = baseBranch
	return nil
}

func (m *Model) setupMainRepoBranch(ticket *board.Ticket) error {
	proj := m.globalStore.GetProjectForTicket(ticket)
	if proj == nil {
		return fmt.Errorf("project not found for ticket")
	}

	mgr := m.worktreeMgrs[proj.ID]
	if mgr == nil {
		return fmt.Errorf("worktree manager not found")
	}

	branchName := m.generateBranchName(ticket, proj)
	baseBranch, _ := mgr.GetDefaultBranch()

	ticket.WorktreePath = proj.RepoPath
	ticket.BranchName = branchName
	ticket.BaseBranch = baseBranch
	return nil
}

func (m *Model) generateBranchNameFromTitle(title string, proj *project.Project) string {
	maxLen := m.getSlugMaxLength(proj)
	slug := board.Slugify(title, maxLen)

	template := m.getBranchTemplate(proj)
	prefix := m.getBranchPrefix(proj)

	result := strings.ReplaceAll(template, "{prefix}", prefix)
	result = strings.ReplaceAll(result, "{slug}", slug)

	return result
}

func (m *Model) generateBranchName(ticket *board.Ticket, proj *project.Project) string {
	if ticket.BranchName != "" {
		return ticket.BranchName
	}
	return m.generateBranchNameFromTitle(ticket.Title, proj)
}

func (m *Model) allocateAgentPort() int {
	usedPorts := make(map[int]bool)
	for _, t := range m.globalStore.All() {
		if t.AgentPort > 0 {
			usedPorts[t.AgentPort] = true
		}
	}

	port := agentPortBase
	for usedPorts[port] {
		port++
	}
	return port
}

// ownsProbeTimeout is the cap we put on the daemon Owns RPC during the
// spawn-path dead-session gate. The probe is best-effort — if the
// daemon is slow or unreachable we fall back to the on-disk dead-check.
// Keep this small: the user just pressed Enter on a ticket and is
// waiting for the spawn flow to proceed.
const ownsProbeTimeout = 500 * time.Millisecond

// shouldCleanupDeadSession decides whether spawnAgent should fire the
// IsClaudeSessionDead / DeleteClaudeSession cleanup for ticket's prior
// session. Returns (cleanup, deadJSONLPath).
//
// The decision tree is:
//  1. If the daemon owns a live PTY for ticket.AgentSessionID, return
//     (false, "") — never delete the JSONL of a session the daemon is
//     actively writing. The on-disk transcript may look "dead" because
//     the assistant hasn't replied yet; deleting it would break a
//     future `--continue`.
//  2. Otherwise, fall through to agent.IsClaudeSessionDead and report
//     its verdict (and the JSONL path so the caller can unlink it).
//
// The Owns probe is bounded by ownsProbeTimeout; on timeout / RPC
// error we conservatively fall through to the disk check (and log so
// the timeout is visible).
func (m *Model) shouldCleanupDeadSession(ticket *board.Ticket) (bool, string) {
	if ticket == nil {
		return false, ""
	}
	if m.guardAPI != nil && ticket.AgentSessionID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), ownsProbeTimeout)
		resp, err := m.guardAPI.Owns(ctx, ticket.AgentSessionID)
		cancel()
		switch {
		case err == nil && resp.Owned:
			// Daemon owns the live session — skip the dead-session
			// cleanup entirely. The wouldChange/modal path in
			// spawnAgent still runs.
			return false, ""
		case err != nil:
			// Probe failed (timeout, connection refused, etc.). Log
			// and fall through to the on-disk check — refusing to
			// spawn because we couldn't reach the daemon would be
			// strictly worse than the rare edge case of cleaning up
			// a session whose ownership we couldn't confirm.
			log.Printf("openkanban: spawn-gate Owns(%s) failed: %v", ticket.AgentSessionID, err)
		}
	}
	dead, deadPath, _ := agent.IsClaudeSessionDead(ticket.WorktreePath)
	return dead, deadPath
}

// spawnAgent is the single entry point for the "open this ticket's
// agent" action: both 's' and Enter on the board view route here, as
// does double-click. It dispatches based on the current pane state:
//
//   - no pane / PaneViewDetached  → spawn a fresh session
//   - PaneViewUnattached          → attach to the daemon-owned session
//   - PaneViewAttached            → just switch to the agent view
//
// The pre-consolidation behavior split this between spawnAgent and
// attachToAgent and produced "press the OTHER key" bounce
// notifications when the user pressed the wrong one for the current
// state.
func (m *Model) spawnAgent() (tea.Model, tea.Cmd) {
	ticket := m.selectedTicket()
	if ticket == nil {
		m.notify("No ticket selected")
		return m, nil
	}

	if existing, exists := m.panes[ticket.ID]; exists {
		switch existing.State() {
		case daemonclient.PaneViewAttached:
			// Already attached in this TUI — just switch to its view.
			m.mode = ModeAgentView
			m.focusedPane = ticket.ID
			existing.SetSize(m.width, m.height-2)
			return m, m.maybeSetWindowTitle()
		case daemonclient.PaneViewUnattached:
			// Daemon owns it (likely from a prior TUI run or sibling
			// instance). Re-attach instead of spawning a duplicate.
			m.mode = ModeAgentView
			m.focusedPane = ticket.ID
			existing.SetSize(m.width, m.height-2)
			cmd := m.attachExisting(ticket.ID, existing)
			return m, tea.Batch(cmd, m.maybeSetWindowTitle())
		}
		// PaneViewDetached falls through to the spawn path so a stale
		// view (daemon vanished, etc.) gets refreshed.
	}

	// From here on we are spawning fresh, which requires the ticket to
	// actually be ready to work on. Putting the in-progress check after
	// the attach branches lets Enter/s on an already-running session
	// reach the view without the user first having to clear this gate.
	if ticket.Status != board.StatusInProgress {
		m.notify("Press Space to move to In Progress first")
		return m, nil
	}

	proj := m.globalStore.GetProjectForTicket(ticket)
	if proj == nil {
		m.notify("Project not found for this ticket")
		return m, nil
	}

	if !ticket.UseWorktree {
		for otherID := range m.panes {
			if otherID == ticket.ID {
				continue
			}
			other, _ := m.globalStore.Get(otherID)
			if other != nil && !other.UseWorktree {
				otherProj := m.globalStore.GetProjectForTicket(other)
				if otherProj != nil && otherProj.ID == proj.ID {
					m.notify("Another main-repo agent is running in this project")
					return m, nil
				}
			}
		}
	}

	agentType := ticket.AgentType
	if agentType == "" {
		agentType = m.config.Defaults.DefaultAgent
	}
	agentCfg, ok := m.config.Agents[agentType]
	if !ok {
		m.notify("Agent '" + agentType + "' not configured")
		return m, nil
	}

	// Start opencode server on-demand if spawning opencode agent
	if agentType == "opencode" {
		_ = m.opencodeServer.Start() // Best effort, ignore errors
	}

	// Stale-brief detection (claude only): if a prior session exists AND
	// the merge would change the brief on disk, ask the user how to
	// proceed before transitioning to ModeSpawning.
	if agentType == "claude" && ticket.AgentSpawnedAt != nil {
		// T3: Dead-session auto-cleanup is gated by daemon ownership.
		// If the daemon currently owns the live PTY for this session
		// UUID, the on-disk JSONL may legitimately look "dead" (no
		// assistant content yet, mid-write) while the runtime session
		// is fine. Deleting the JSONL in that case would break a
		// future `--continue`. shouldCleanupDeadSession encapsulates
		// the Owns probe + IsClaudeSessionDead decision.
		shouldCleanup, deadPath := m.shouldCleanupDeadSession(ticket)
		if shouldCleanup {
			if deadPath != "" {
				_ = agent.DeleteClaudeSession(deadPath)
			}
			ticket.AgentSpawnedAt = nil
			m.saveTicket(ticket)
			// fall through to the empty-plan spawn path below
		} else {
			_, _, wouldChange, _, _ := agent.PreviewBriefMerge(ticket, ticket.WorktreePath)
			if wouldChange {
				// Capture ticket/proj/agentCfg into each callback. Each option
				// sets its own plan and proceeds with the existing tea.Batch.
				m.showChoice = true
				m.choiceMsg = "Brief was updated since this session started. What should I do?"
				ticketCopy := ticket // pointer — fine, the closures don't outlive the ticket
				projCopy := proj
				cfgCopy := agentCfg
				m.choices = []choiceItem{
					{
						Key:   'd',
						Label: "Discard prior session, start fresh",
						Fn: func() tea.Cmd {
							ticketCopy.AgentSpawnedAt = nil
							m.saveTicket(ticketCopy)
							m.mode = ModeSpawning
							m.spawningTicketID = ticketCopy.ID
							m.spawningAgent = agentType
							return tea.Batch(m.spinner.Tick, m.prepareSpawnWith(ticketCopy, projCopy, cfgCopy, spawnPlan{ForceFresh: true}))
						},
					},
					{
						Key:   'u',
						Label: "Resume; tell agent the brief changed",
						Fn: func() tea.Cmd {
							m.mode = ModeSpawning
							m.spawningTicketID = ticketCopy.ID
							m.spawningAgent = agentType
							return tea.Batch(m.spinner.Tick, m.prepareSpawnWith(ticketCopy, projCopy, cfgCopy, spawnPlan{InjectResumeNotice: true}))
						},
					},
					{
						Key:   'n',
						Label: "Resume; leave brief unchanged",
						Fn: func() tea.Cmd {
							m.mode = ModeSpawning
							m.spawningTicketID = ticketCopy.ID
							m.spawningAgent = agentType
							return tea.Batch(m.spinner.Tick, m.prepareSpawnWith(ticketCopy, projCopy, cfgCopy, spawnPlan{SkipMerge: true}))
						},
					},
				}
				return m, nil
			}
		}
	}

	m.mode = ModeSpawning
	m.spawningTicketID = ticket.ID
	m.spawningAgent = agentType

	return m, tea.Batch(m.spinner.Tick, m.prepareSpawnWith(ticket, proj, agentCfg, spawnPlan{}))
}

// resolveBrief decides what to do with the in-repo brief file at
// tickets/<slug>.md based on the spawnPlan:
//
//   - SkipMerge=false (default / ForceFresh / InjectResumeNotice):
//     call agent.MergeTicketBrief which writes the card description
//     into the brief file (atomic rename).
//   - SkipMerge=true: leave the file's bytes untouched, but still stat
//     it so the prompt template's {{if .HasBrief}} branch behaves
//     correctly.
//
// Returns the worktree-relative path (or "" if no brief), whether the
// file exists on disk after the operation, and any merge error.
// Extracted as a separate function so the SkipMerge "file bytes
// preserved" property can be unit-tested in isolation from the rest
// of prepareSpawnWith.
func resolveBrief(ticket *board.Ticket, worktreePath string, plan spawnPlan) (string, bool, error) {
	if !plan.SkipMerge {
		return agent.MergeTicketBrief(ticket, worktreePath)
	}
	slug := agent.BranchSlug(ticket.BranchName)
	if slug == "" || worktreePath == "" {
		return "", false, nil
	}
	rel := "tickets/" + slug + ".md"
	full := filepath.Join(worktreePath, "tickets", slug+".md")
	if _, statErr := os.Stat(full); statErr != nil {
		return "", false, nil
	}
	return rel, true, nil
}

// spawnReqInputs collects the resolved inputs needed to construct a
// daemon.SpawnReq. All fields are values (no Model receiver, no live
// filesystem lookups) so buildSpawnReq below is a pure function and
// can be unit-tested per spawnPlan branch in isolation.
//
// Callers (today: prepareSpawnWith's closure) are responsible for
// performing the filesystem I/O — agent.FindOpencodeSession,
// agent.MergeTicketBrief, etc. — and passing the resolved values in.
// That separation keeps the SpawnReq shape decisions in one tested
// place; the I/O side-effects live in the closure.
type spawnReqInputs struct {
	ticket         *board.Ticket
	plan           spawnPlan
	sessionName    string
	command        string
	workdir        string
	cols           int
	rows           int
	agentType      string
	cleanArgs      []string // agentCfg.Args with empty entries stripped
	isNewSession   bool
	promptTemplate string
	ctxData        agent.ContextData
	agentPort      int
	// Session IDs resolved by the caller via agent.Find{Opencode,Gemini,Codex}Session.
	// Empty when the corresponding session-file isn't present on disk.
	opencodeSessionID string
	geminiSessionID   string
	codexSessionID    string
	// forwardNotifications is the effective config.Behavior.ForwardAgentNotifications
	// value at spawn time. Threaded through SpawnReq so the daemon's
	// terminal.Pane gates its OSC 9 → desktop-notification handler on
	// this per session, rather than relying on a process-wide global.
	forwardNotifications bool
}

// buildSpawnReq constructs the daemon.SpawnReq for a ticket given the
// chosen spawnPlan. Pure function — no I/O, no Model receiver. Tested
// separately from the prepareSpawnWith integration path so each
// spawnPlan branch's argv + env shape is pinned by the test suite and
// cannot regress.
//
// The Env field carries OPENKANBAN_SESSION and OPENKANBAN_TICKET_ID
// explicitly so the wire-level SpawnReq is self-describing. The daemon
// side ALSO synthesizes them from req.SessionName / req.TicketID via
// terminal.Pane.SetSessionName / SetTicketID + buildCleanEnv — the two
// paths agree, and a downstream consumer of SpawnReq.Env (e.g. a
// future RPC log) sees the env contract on the wire.
func buildSpawnReq(in spawnReqInputs) daemon.SpawnReq {
	args := make([]string, len(in.cleanArgs))
	copy(args, in.cleanArgs)

	switch in.agentType {
	case "claude":
		if in.isNewSession {
			// New claude sessions always start in plan mode so the user
			// reviews the proposed approach before any tree mutation.
			// Strip anything that would conflict — --dangerously-skip-permissions
			// (alias for bypassPermissions) and any pre-existing
			// --permission-mode pair from the user's config — then
			// append --permission-mode plan as the single authority.
			args = stripPermissionFlags(args)
			args = append(args, "--permission-mode", "plan")
			if !hasClaudeNameFlag(args) && strings.TrimSpace(in.ticket.Title) != "" {
				args = append(args, "-n", in.ticket.Title)
			}
			if in.ticket.AgentSessionID != "" && agent.SessionUUIDPattern.MatchString(in.ticket.AgentSessionID) {
				args = append(args, "--resume", in.ticket.AgentSessionID)
				if !in.ticket.SessionOwned {
					args = append(args, "--fork-session")
				}
			}
			if in.promptTemplate != "" {
				prompt := agent.BuildContextPrompt(in.promptTemplate, in.ctxData)
				if prompt != "" {
					args = append(args, prompt)
				}
			}
		} else {
			hasFlag := false
			for _, arg := range args {
				if arg == "--continue" || arg == "-c" {
					hasFlag = true
					break
				}
			}
			if !hasFlag {
				args = append(args, "--continue")
				// plan.InjectResumeNotice (option 'u'): append a positional
				// message after --continue so the resumed claude session
				// sees the brief-updated notice as the first new user turn.
				if in.plan.InjectResumeNotice {
					slug := agent.BranchSlug(in.ticket.BranchName)
					if slug != "" {
						args = append(args, fmt.Sprintf("Brief updated at tickets/%s.md — please re-read before continuing.", slug))
					}
				}
			}
		}
	case "opencode":
		args = []string{in.workdir, "--port", fmt.Sprintf("%d", in.agentPort)}
		if in.isNewSession {
			if in.promptTemplate != "" {
				prompt := agent.BuildContextPrompt(in.promptTemplate, in.ctxData)
				if prompt != "" {
					args = append(args, "--prompt", prompt)
				}
			}
		} else if in.opencodeSessionID != "" {
			args = append(args, "--session", in.opencodeSessionID)
		} else {
			args = append(args, "--continue")
		}
	case "gemini":
		if !in.isNewSession {
			if in.geminiSessionID != "" {
				args = append(args, "--resume")
			}
		} else if in.promptTemplate != "" {
			prompt := agent.BuildContextPrompt(in.promptTemplate, in.ctxData)
			if prompt != "" {
				args = append(args, "-i", prompt)
			}
		}
	case "codex":
		if !in.isNewSession {
			if in.codexSessionID != "" {
				if in.codexSessionID == "last" {
					args = []string{"resume", "--last"}
				} else {
					args = []string{"resume", in.codexSessionID}
				}
				args = append(args, in.cleanArgs...)
			}
		} else if in.promptTemplate != "" {
			prompt := agent.BuildContextPrompt(in.promptTemplate, in.ctxData)
			if prompt != "" {
				args = append(args, prompt)
			}
		}
	}

	// SpawnReq.Env duplicates OPENKANBAN_SESSION + OPENKANBAN_TICKET_ID
	// that the daemon-side buildCleanEnv would synthesize anyway. That
	// redundancy is intentional: the wire shape now carries the env
	// contract explicitly, so the test asserts on req.Env directly and
	// any future caller of Spawn (not just this closure) automatically
	// inherits the same env without a separate plumbing step.
	var env []string
	if in.sessionName != "" {
		env = append(env, "OPENKANBAN_SESSION="+in.sessionName)
	}
	ticketIDStr := string(in.ticket.ID)
	if ticketIDStr != "" {
		env = append(env, "OPENKANBAN_TICKET_ID="+ticketIDStr)
	}

	return daemon.SpawnReq{
		TicketID:             ticketIDStr,
		SessionName:          in.sessionName,
		Command:              in.command,
		Args:                 args,
		Workdir:              in.workdir,
		Env:                  env,
		Cols:                 in.cols,
		Rows:                 in.rows,
		Scrollback:           0,
		AgentSessionUUID:     in.ticket.AgentSessionID,
		ForwardNotifications: in.forwardNotifications,
	}
}

// prepareSpawnWith returns a tea.Cmd that performs the actual spawn off
// the event loop. The `plan` value is captured by value into the
// returned closure — keep it a value type (not a pointer) so future
// callers cannot accidentally mutate it after the modal callback fires.
func (m *Model) prepareSpawnWith(ticket *board.Ticket, proj *project.Project, agentCfg config.AgentConfig, plan spawnPlan) tea.Cmd {
	ticketID := ticket.ID
	worktreePath := ticket.WorktreePath
	branchName := ticket.BranchName
	baseBranch := ticket.BaseBranch
	useWorktree := ticket.UseWorktree
	width, height := m.width, m.height-2

	agentType := agentCfg.Command
	if strings.Contains(agentType, "/") {
		agentType = filepath.Base(agentType)
	}

	agentPort := ticket.AgentPort
	if agentPort == 0 && agentType == "opencode" {
		agentPort = m.allocateAgentPort()
		ticket.AgentPort = agentPort
		m.saveTicket(ticket)
	}

	mgr := m.worktreeMgrs[proj.ID]
	cfg := m.config
	daemonClient := m.daemonClient

	return func() tea.Msg {
		if mgr == nil {
			return spawnErrorMsg{ticketID: ticketID, err: "worktree manager not found"}
		}
		if daemonClient == nil {
			return spawnErrorMsg{ticketID: ticketID, err: "daemon unreachable — cannot spawn agent"}
		}

		generatedBranch := branchName
		if generatedBranch == "" {
			maxLen := m.getSlugMaxLength(proj)
			slug := board.Slugify(ticket.Title, maxLen)
			template := m.getBranchTemplate(proj)
			prefix := m.getBranchPrefix(proj)
			generatedBranch = strings.ReplaceAll(template, "{prefix}", prefix)
			generatedBranch = strings.ReplaceAll(generatedBranch, "{slug}", slug)
		}

		base, _ := mgr.GetDefaultBranch()
		if baseBranch != "" {
			base = baseBranch
		}

		if useWorktree {
			if worktreePath == "" {
				path, err := mgr.CreateWorktree(generatedBranch, base)
				if err != nil {
					return spawnErrorMsg{ticketID: ticketID, err: "worktree failed: " + err.Error()}
				}
				if err := agent.SeedClaudeSettings(path, proj.RepoPath); err != nil {
					log.Printf("openkanban: seed claude settings (%s): %v", path, err)
				}
				worktreePath = path
			}
		} else {
			if err := mgr.SetupBranch(generatedBranch, base); err != nil {
				return spawnErrorMsg{ticketID: ticketID, err: "branch setup failed: " + err.Error()}
			}
			worktreePath = proj.RepoPath
		}
		branchName = generatedBranch
		baseBranch = base

		// Session name for terminal identification (priority:
		// AgentSessionID > branch > ticket). The daemon picks this up
		// in SpawnReq.SessionName and wires it into OPENKANBAN_SESSION
		// via the terminal pane's buildCleanEnv.
		sessionName := string(ticketID)
		if branchName != "" {
			sessionName = branchName
		}
		if ticket.AgentSessionID != "" {
			sessionName = ticket.AgentSessionID
		}
		// sessionName + ticketID flow through SpawnReq below; daemon-side
		// pane.SetSessionName + pane.SetTicketID happen in StartHeadless.

		// Clean up any stale status file from previous sessions that may not have
		// been properly cleaned up (e.g., if the app was closed while an agent was running)
		agent.CleanupStatusFile(sessionName)

		isNewSession := ticket.AgentSpawnedAt == nil
		// cleanArgs strips empty-string entries from the configured args so a
		// user can omit a default flag by leaving an empty placeholder without
		// poisoning argv (claude in particular gets confused by a leading "").
		cleanArgs := make([]string, 0, len(agentCfg.Args))
		for _, a := range agentCfg.Args {
			if a != "" {
				cleanArgs = append(cleanArgs, a)
			}
		}

		promptTemplate := cfg.GetEffectiveInitPrompt(agentType)

		// Sync the openkanban card's description into the in-repo brief
		// file at tickets/<slug>.md (worktree-relative) before rendering
		// the priming prompt. A brief write failure is logged but does
		// not abort the spawn — the agent can still proceed with the
		// inline title/description from the prompt. Stays CLIENT-side
		// because the daemon doesn't touch the worktree filesystem; the
		// brief must be written before Spawn so the resumed agent sees
		// it.
		briefRelPath, hasBrief, briefErr := resolveBrief(ticket, worktreePath, plan)
		if briefErr != nil {
			fmt.Fprintf(os.Stderr, "openkanban: merge brief failed: %v\n", briefErr)
		}

		// readyNotice is surfaced via m.notify() in the spawnReadyMsg
		// handler. For option 'u' (InjectResumeNotice), we toast the user
		// so they know the brief was rewritten under the resumed session.
		var readyNotice string
		if plan.InjectResumeNotice {
			if slug := agent.BranchSlug(ticket.BranchName); slug != "" {
				readyNotice = fmt.Sprintf("Brief at tickets/%s.md updated.", slug)
			}
		}
		// External resume: spawn was given an AgentSessionID up front
		// (via `openkanban ticket new --session <uuid>`), so this is the
		// first openkanban-spawn but the underlying claude session is
		// already populated with prior context. The template uses this
		// to shorten the priming preamble.
		isExternalResume := isNewSession && ticket.AgentSessionID != "" && agent.SessionUUIDPattern.MatchString(ticket.AgentSessionID)
		ctxData := agent.NewContextData(ticket, briefRelPath, hasBrief, isExternalResume)

		command := agentCfg.Command

		// Resolve agent-specific session IDs from the worktree
		// filesystem. These are inputs to buildSpawnReq below — the
		// helper itself is pure, so the I/O happens here.
		var opencodeSessionID, geminiSessionID, codexSessionID string
		switch agentType {
		case "opencode":
			opencodeSessionID = agent.FindOpencodeSession(worktreePath)
		case "gemini":
			if !isNewSession {
				geminiSessionID = agent.FindGeminiSession(worktreePath)
			}
		case "codex":
			if !isNewSession {
				codexSessionID = agent.FindCodexSession(worktreePath)
			}
		}

		// Hand the spawn off to the daemon. The daemon runs the PTY in
		// its own process; we then build a PaneView and attach
		// immediately so the snapshot frames flow into the local
		// emulator before the model sees the spawnReadyMsg.
		req := buildSpawnReq(spawnReqInputs{
			ticket:               ticket,
			plan:                 plan,
			sessionName:          sessionName,
			command:              command,
			workdir:              worktreePath,
			cols:                 width,
			rows:                 height,
			agentType:            agentType,
			cleanArgs:            cleanArgs,
			isNewSession:         isNewSession,
			promptTemplate:       promptTemplate,
			ctxData:              ctxData,
			agentPort:            agentPort,
			opencodeSessionID:    opencodeSessionID,
			geminiSessionID:      geminiSessionID,
			codexSessionID:       codexSessionID,
			forwardNotifications: cfg.Behavior.ForwardAgentNotifications,
		})
		spawnCtx, spawnCancel := context.WithTimeout(context.Background(), 5*time.Second)
		resp, err := daemonClient.Spawn(spawnCtx, req)
		spawnCancel()
		if err != nil {
			return spawnErrorMsg{ticketID: ticketID, err: "spawn failed: " + err.Error()}
		}

		pv := daemonclient.NewPaneView(daemonClient, string(ticketID), resp.SessionID, nil)
		pv.SetWorkdir(worktreePath)
		pv.SetSessionName(sessionName)
		pv.SetSize(width, height)

		attachCtx, attachCancel := context.WithTimeout(context.Background(), 5*time.Second)
		attachErr := pv.Attach(attachCtx)
		attachCancel()
		if attachErr != nil {
			log.Printf("openkanban model: attach failed after spawn ticket=%s session=%s err=%v", ticketID, resp.SessionID, attachErr)
			// Spawn succeeded but we couldn't get a binary channel.
			// Keep the PaneView so the user can retry attach; surface
			// the error.
			return spawnReadyMsg{
				ticketID:     ticketID,
				pane:         pv,
				worktreePath: worktreePath,
				branchName:   branchName,
				baseBranch:   baseBranch,
			}
		}

		return spawnReadyMsg{
			ticketID:     ticketID,
			pane:         pv,
			worktreePath: worktreePath,
			branchName:   branchName,
			baseBranch:   baseBranch,
			notice:       readyNotice,
		}
	}
}

func (m *Model) stopAgent() (tea.Model, tea.Cmd) {
	ticket := m.selectedTicket()
	if ticket == nil {
		return m, nil
	}

	if pane, ok := m.panes[ticket.ID]; ok {
		pane.Stop()
		delete(m.panes, ticket.ID)
	}

	// Preserve AgentCompleted on a Done ticket — manually stopping the
	// pane after the agent reported completion shouldn't wipe the badge.
	if ticket.Status != board.StatusDone {
		ticket.AgentStatus = board.AgentNone
	}
	m.saveTicket(ticket)
	m.notify("Agent stopped")
	return m, nil
}

func (m *Model) selectedTicket() *board.Ticket {
	if len(m.columnTickets) <= m.activeColumn {
		return nil
	}
	tickets := m.columnTickets[m.activeColumn]
	if len(tickets) <= m.activeTicket {
		return nil
	}
	return tickets[m.activeTicket]
}

func (m *Model) selectTicketByID(ticketID board.TicketID) {
	for colIdx, tickets := range m.columnTickets {
		for ticketIdx, t := range tickets {
			if t.ID == ticketID {
				m.activeColumn = colIdx
				m.activeTicket = ticketIdx
				m.ensureTicketVisible()
				return
			}
		}
	}
	// Target no longer visible (filtered out by a refresh). Clamp
	// activeTicket to the current column's bounds so callers don't
	// see an out-of-range index. Without this, toggling a filter that
	// hides the selected ticket leaves the cursor pointing past the
	// end of a now-shorter column.
	if m.activeColumn >= 0 && m.activeColumn < len(m.columnTickets) {
		if n := len(m.columnTickets[m.activeColumn]); m.activeTicket >= n {
			if n > 0 {
				m.activeTicket = n - 1
			} else {
				m.activeTicket = 0
			}
		}
	}
}

func (m *Model) refreshColumnTickets() {
	m.columnTickets = make([][]*board.Ticket, len(m.columns))
	for i, col := range m.columns {
		allForStatus := m.globalStore.GetByStatus(col.Status)
		var filtered []*board.Ticket
		for _, t := range allForStatus {
			if !m.ticketMatchesFilter(t) {
				continue
			}
			filtered = append(filtered, t)
		}
		sortTickets(filtered, m.sortMode)
		m.columnTickets[i] = filtered
	}

	if len(m.columnOffsets) != len(m.columns) {
		m.columnOffsets = make([]int, len(m.columns))
	}
	if len(m.columnTicketHeights) != len(m.columns) {
		m.columnTicketHeights = make([][]int, len(m.columns))
	}
}

// sortTickets reorders the slice in place per the given mode. Priority 0
// (unset) is treated as the default value 3 so cards predating the
// priority field don't all clump at one end of the sort.
func sortTickets(tickets []*board.Ticket, mode SortMode) {
	switch mode {
	case SortName:
		sort.SliceStable(tickets, func(i, j int) bool {
			return strings.ToLower(tickets[i].Title) < strings.ToLower(tickets[j].Title)
		})
	case SortAge:
		sort.SliceStable(tickets, func(i, j int) bool {
			return tickets[i].CreatedAt.After(tickets[j].CreatedAt)
		})
	case SortPriority:
		sort.SliceStable(tickets, func(i, j int) bool {
			a := effectivePriority(tickets[i].Priority)
			b := effectivePriority(tickets[j].Priority)
			if a != b {
				return a < b
			}
			return tickets[i].CreatedAt.After(tickets[j].CreatedAt)
		})
	}
}

func effectivePriority(p int) int {
	if p < 1 || p > 5 {
		return 3
	}
	return p
}

func (m *Model) ticketMatchesFilter(t *board.Ticket) bool {
	_, isOpenSession := m.daemonOwned[t.ID]
	// alwaysShowWorking exempts daemon-owned sessions from the project
	// and text-search filters, but the session filter ('w') below still
	// applies — narrowing to "waiting" must keep hiding working-status
	// sessions even with the bypass on.
	bypassProjectAndQuery := m.alwaysShowWorking && isOpenSession

	if !bypassProjectAndQuery && len(m.filterProjectIDs) > 0 && !m.filterProjectIDs[t.ProjectID] {
		return false
	}
	switch m.sessionFilter {
	case SessionFilterOpen:
		if !isOpenSession {
			return false
		}
	case SessionFilterWaiting:
		if !isOpenSession {
			return false
		}
		if t.AgentStatus != board.AgentWaiting {
			return false
		}
	}
	if bypassProjectAndQuery {
		return true
	}
	if m.filterQuery == "" {
		return true
	}

	query := strings.ToLower(m.filterQuery)

	if strings.HasPrefix(query, "@") {
		parts := strings.SplitN(query, " ", 2)
		projectName := strings.TrimPrefix(parts[0], "@")
		proj := m.globalStore.GetProjectForTicket(t)
		if proj == nil || !strings.Contains(strings.ToLower(proj.Name), projectName) {
			return false
		}
		if len(parts) == 1 {
			return true
		}
		query = strings.TrimSpace(parts[1])
	}

	title := strings.ToLower(t.Title)
	desc := strings.ToLower(t.Description)
	return strings.Contains(title, query) || strings.Contains(desc, query)
}

func (m *Model) nextStatus(current board.TicketStatus) board.TicketStatus {
	switch current {
	case board.StatusBacklog:
		return board.StatusInProgress
	case board.StatusInProgress:
		return board.StatusInReview
	case board.StatusInReview:
		return board.StatusDone
	default:
		return current
	}
}

func (m *Model) previousStatus(current board.TicketStatus) board.TicketStatus {
	switch current {
	case board.StatusDone:
		return board.StatusInReview
	case board.StatusInReview:
		return board.StatusInProgress
	case board.StatusInProgress:
		return board.StatusBacklog
	default:
		return current
	}
}

// moveAndPromoteMsg formats the post-Move status-bar toast. When the
// transition into in_review/done promoted N claude-code approvals from
// the worktree into the source repo's settings.local.json, it appends a
// "promoted N approval(s)" suffix so the user sees what just went
// global. Empty promoted → only the "Moved to <status>" core message.
func moveAndPromoteMsg(target board.TicketStatus, promoted []string) string {
	msg := "Moved to " + string(target)
	switch n := len(promoted); n {
	case 0:
	case 1:
		msg += " · promoted 1 approval to repo defaults"
	default:
		msg += fmt.Sprintf(" · promoted %d approvals to repo defaults", n)
	}
	return msg
}

func (m *Model) notify(msg string) {
	m.notification = msg
	m.notifyTime = time.Now()
	// Mirror every notification to stderr so the user has a durable
	// record after the TUI exits — the in-UI toast disappears on
	// timeout (and is hard to even select for copy without the click
	// hitting another control). With stderr logging, the same message
	// is in /tmp/<wherever-the-user-redirects>.log.
	log.Printf("openkanban notify: %s", msg)
}

func (m *Model) saveTicket(ticket *board.Ticket) {
	if err := m.globalStore.Save(ticket); err != nil {
		log.Printf("openkanban saveTicket: ticket=%s err: %v", ticket.ID, err)
		m.notify("Failed to save: " + err.Error())
		return
	}
	m.recordSavedTicket(ticket)
}

// hasClaudeNameFlag returns true if the args slice already contains
// a Claude Code session-name flag (-n or --name). Used to avoid
// double-naming when the user pre-set it in their agent config.
func hasClaudeNameFlag(args []string) bool {
	for _, a := range args {
		if a == "-n" || a == "--name" || strings.HasPrefix(a, "--name=") {
			return true
		}
	}
	return false
}

// stripPermissionFlags removes any claude permission-related flags the
// user may have configured (--dangerously-skip-permissions, any form
// of --permission-mode) so the caller can install its own authoritative
// permission mode without ambiguity.
func stripPermissionFlags(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--dangerously-skip-permissions" {
			continue
		}
		if a == "--permission-mode" {
			// Skip the flag and its value (if present).
			if i+1 < len(args) {
				i++
			}
			continue
		}
		if strings.HasPrefix(a, "--permission-mode=") {
			continue
		}
		out = append(out, a)
	}
	return out
}

func (m *Model) resetSpawnState(ticketID board.TicketID) {
	if ticket, _ := m.globalStore.Get(ticketID); ticket != nil {
		ticket.AgentSpawnedAt = nil
		// Same rule as stopAgent: a Done ticket keeps its completed badge.
		if ticket.Status != board.StatusDone {
			ticket.AgentStatus = board.AgentNone
		}
		m.saveTicket(ticket)
	}
	m.mode = ModeNormal
	m.spawningTicketID = ""
	m.spawningAgent = ""
	delete(m.panes, ticketID)
}

func (m *Model) RunningAgentCount() int {
	count := 0
	for _, pane := range m.panes {
		if pane.Running() {
			count++
		}
	}
	return count
}

func (m *Model) getAgentNames() []string {
	names := make([]string, 0, len(m.config.Agents))
	for name := range m.config.Agents {
		names = append(names, name)
	}
	if len(names) == 0 {
		return []string{"opencode", "claude", "gemini", "codex", "aider"}
	}
	sort.Strings(names)
	return names
}

func (m *Model) getDefaultAgent() string {
	return m.config.Defaults.DefaultAgent
}

func (m *Model) getBranchPrefix(proj *project.Project) string {
	if proj != nil && proj.Settings.BranchPrefix != "" {
		return proj.Settings.BranchPrefix
	}
	if m.config.Defaults.BranchPrefix != "" {
		return m.config.Defaults.BranchPrefix
	}
	return "task/"
}

func (m *Model) getBranchTemplate(proj *project.Project) string {
	if proj != nil && proj.Settings.BranchTemplate != "" {
		return proj.Settings.BranchTemplate
	}
	if m.config.Defaults.BranchTemplate != "" {
		return m.config.Defaults.BranchTemplate
	}
	return "{prefix}{slug}"
}

func (m *Model) getSlugMaxLength(proj *project.Project) int {
	if proj != nil && proj.Settings.SlugMaxLength > 0 {
		return proj.Settings.SlugMaxLength
	}
	if m.config.Defaults.SlugMaxLength > 0 {
		return m.config.Defaults.SlugMaxLength
	}
	return 40
}

func (m *Model) getAgentIndex(agentName string) int {
	agents := m.getAgentNames()
	for i, name := range agents {
		if name == agentName {
			return i
		}
	}
	return 0
}

// T2 of the integration plan removed maybeAutoStopCompletedPane.
// Ticket-done now flows CLI → daemon (TicketDoneReq) → SessionEvent
// broadcast; subscribed TUIs react via handleDaemonSessionEvent with
// the authoritative Expected=true signal. No per-TUI poll-driven kill
// path remains.

// Cleanup detaches every pane this TUI holds from its daemon-side
// session. It does NOT kill the underlying agents: daemon sessions
// outlive any single TUI, and other TUIs may still be attached. The
// daemon's last-client-disconnect handler (server.go) is the only
// place sessions die on TUI exit, and it only fires when the actual
// last connection drops.
func (m *Model) Cleanup() {
	for _, pane := range m.panes {
		_ = pane.Close()
	}
}

func (m *Model) pollAgentStatusesAsync() tea.Cmd {
	type paneInfo struct {
		ticketID        board.TicketID
		agentType       string
		worktreePath    string
		branchName      string
		agentPort       int
		agentSessionID  string
		running         bool
		terminalContent string
		lastActivity    time.Time
	}

	var panes []paneInfo
	for ticketID, pane := range m.panes {
		ticket, _ := m.globalStore.Get(ticketID)
		if ticket == nil {
			continue
		}
		worktreePath := pane.GetWorkdir()
		if worktreePath == "" {
			worktreePath = ticket.WorktreePath
		}
		panes = append(panes, paneInfo{
			ticketID:        ticketID,
			agentType:       ticket.AgentType,
			worktreePath:    worktreePath,
			branchName:      ticket.BranchName,
			agentPort:       ticket.AgentPort,
			agentSessionID:  ticket.AgentSessionID,
			running:         pane.Running(),
			terminalContent: pane.GetContent(),
			lastActivity:    m.lastPTYActivity[ticketID],
		})
	}

	detector := m.statusDetector
	globalStore := m.globalStore

	return func() tea.Msg {
		results := make(agentStatusResultMsg)
		for _, p := range panes {
			if !p.running {
				results[p.ticketID] = board.AgentNone
				continue
			}

			sessionID := p.agentSessionID
			if sessionID == "" && p.agentType == "opencode" && p.worktreePath != "" {
				if id := agent.FindOpencodeSession(p.worktreePath); id != "" {
					sessionID = id
					if ticket, _ := globalStore.Get(p.ticketID); ticket != nil {
						ticket.AgentSessionID = sessionID
						globalStore.Save(ticket)
					}
				}
			}
			if sessionID == "" {
				sessionID = p.branchName
			}
			if sessionID == "" {
				sessionID = string(p.ticketID)
			}

			status := detector.DetectStatusWithActivity(p.agentType, sessionID, p.worktreePath, p.agentPort, true, p.terminalContent, p.lastActivity)
			results[p.ticketID] = status
		}
		return results
	}
}

func (m *Model) handleTerminalMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	for _, pane := range m.panes {
		if cmd := pane.Update(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	// Always re-arm the listener on the pane the message was addressed
	// to, even if Pane.Update returned nil. PaneView.Update only emits
	// a follow-up readNextMsg Cmd from a small set of messages today;
	// safe to bridge here regardless.
	if pid, ok := paneIDOf(msg); ok {
		if pv, exists := m.panes[board.TicketID(pid)]; exists {
			cmds = append(cmds, m.listenPaneMessages(pv))
		}
	}
	// The child may have emitted an OSC title sequence in this batch
	// of output — reflect any change in the host window title. Also
	// runs on RenderTickMsg as a steady-state safety net.
	if cmd := m.maybeSetWindowTitle(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

// paneIDOf returns the PaneID of any daemonclient pane-scoped message,
// or "" for messages that aren't pane-scoped. Lets handleTerminalMsg
// re-arm the right pane's listener without a giant type switch.
func paneIDOf(msg tea.Msg) (string, bool) {
	switch m := msg.(type) {
	case daemonclient.PaneOutputMsg:
		return m.PaneID, true
	case daemonclient.PaneRenderTickMsg:
		return m.PaneID, true
	case daemonclient.PaneAttachedMsg:
		return m.PaneID, true
	case daemonclient.PaneDetachedMsg:
		return m.PaneID, true
	}
	return "", false
}

// listenPaneMessages returns a tea.Cmd that reads one event from pv's
// teaMsgs channel and returns it as a tea.Msg. The model's Update
// re-arms the listener every time it consumes a pane-scoped message
// (see handleTerminalMsg).
//
// The Cmd is also resilient to channel closure (pane Close()) — the
// reader returns PaneExitMsg in that case so the model can clean up
// the entry.
func (m *Model) listenPaneMessages(pv *daemonclient.PaneView) tea.Cmd {
	if pv == nil {
		return nil
	}
	id := pv.ID()
	ch := pv.TeaMessages()
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return daemonclient.PaneExitMsg{PaneID: id, Err: io.EOF}
		}
		return msg
	}
}

type agentStatusMsg time.Time
type agentStatusResultMsg map[board.TicketID]board.AgentStatus
type notificationMsg time.Time
type shutdownCompleteMsg struct{}
type updateCheckMsg update.CheckResult

// binaryStaleCheckMsg fires every update.BinaryStaleCheckInterval to
// trigger a re-stat of os.Executable() against the captured process
// start time. Handled by the main Update loop, which surfaces a
// one-shot notification when the binary on disk is newer than the
// running process. See checkBinaryStaleness.
type binaryStaleCheckMsg struct{}

type spawnReadyMsg struct {
	ticketID     board.TicketID
	pane         *daemonclient.PaneView
	worktreePath string
	branchName   string
	baseBranch   string
	// notice, if non-empty, is shown via m.notify() once the spawn-ready
	// handler runs. Used to surface a "Brief at tickets/<slug>.md updated."
	// toast for option 'u' (InjectResumeNotice) — the closure that emits
	// this message cannot safely call m.notify() itself, so it routes
	// through this field.
	notice string
}

type spawnErrorMsg struct {
	ticketID board.TicketID
	err      string
}

func tickAgentStatus(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg {
		return agentStatusMsg(t)
	})
}

// checkBinaryStaleness returns a tea.Cmd that fires a
// binaryStaleCheckMsg after update.BinaryStaleCheckInterval. The Update
// handler re-arms it on every receipt; the work itself (an os.Stat of
// the executable) happens on the bubbletea goroutine and is effectively
// free.
func checkBinaryStaleness() tea.Cmd {
	return tea.Tick(update.BinaryStaleCheckInterval, func(t time.Time) tea.Msg {
		return binaryStaleCheckMsg{}
	})
}
