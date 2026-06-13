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

const (
	minColumnWidth = 20
	columnOverhead = 5

	// ticketHeight is the worst-case rendered height of a ticket card in rows
	// (2 border + 5 content lines + 1 bottom margin). Used to estimate how many
	// fit in a column; over-packing here pushes the column past the terminal
	// height and the top is clipped off-screen.
	ticketHeight       = 8
	columnHeaderHeight = 3
	// indicatorReserveRows reserves vertical space for the "▲ N more" and
	// "▼ N more" overflow indicators rendered inside a column.
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

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
				ticket.AgentStatus = board.AgentNone
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

	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)

	case quitRequestedMsg:
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

	case daemonclient.PaneOutputMsg, daemonclient.PaneRenderTickMsg, daemonclient.PaneAttachedMsg, daemonclient.PaneDetachedMsg:
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
		// keep dangling references.
		m.daemonConnected.Store(false)
		if m.daemonUnsub != nil {
			m.daemonUnsub()
			m.daemonUnsub = nil
		}
		m.daemonEvents = nil
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
		// Precedence rule (PR9): when the daemon push channel is live,
		// daemon-pushed SessionEvents are the authoritative source of
		// AgentStatus for daemon-owned panes. The local file-poll only
		// fills in for panes the daemon doesn't own (and as graceful
		// degradation when the push channel is down).
		daemonLive := m.daemonConnected.Load()
		for ticketID, status := range msg {
			if daemonLive {
				if _, owned := m.panes[ticketID]; owned {
					// Daemon-owned pane; let push events drive its
					// status and ignore the file-poll value.
					continue
				}
			}
			ticket, _ := m.globalStore.Get(ticketID)
			if ticket == nil {
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

	case daemonSessionEventMsg:
		return m.handleDaemonSessionEvent(msg)

	case daemonSubscribeFailedMsg:
		return m.handleDaemonSubscribeFailed(msg)

	case daemonSubscribeEndedMsg:
		return m.handleDaemonSubscribeEnded(msg)

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
	case "enter":
		return m.attachToAgent()
	case "d":
		return m.confirmDeleteTicket()
	case " ":
		return m.quickMoveTicket()
	case "-", "backspace":
		return m.quickMoveTicketBackward()
	case "s":
		return m.spawnAgent()
	case "S":
		return m.stopAgent()

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

	ticketIdx := offset + (ticketY / ticketHeight)
	if ticketIdx >= len(tickets) {
		return -1
	}

	return ticketIdx
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

	m.globalStore.Move(ticket.ID, targetStatus)
	m.refreshColumnTickets()
	m.saveTicket(ticket)

	m.activeColumn = m.dragTargetColumn
	m.activeTicket = 0
	m.ensureColumnVisible()
	m.ensureTicketVisible()

	m.notify("Moved to " + string(targetStatus))
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
	pane, ok := m.panes[m.focusedPane]
	if !ok {
		m.mode = ModeNormal
		m.focusedPane = ""
		return m, m.maybeSetWindowTitle()
	}

	if result := pane.HandleKey(msg); result != nil {
		switch r := result.(type) {
		case terminal.ExitFocusMsg:
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

func (m *Model) handleAgentViewMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	pane, ok := m.panes[m.focusedPane]
	if !ok {
		return m, nil
	}

	if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
		if msg.Y == 0 && msg.X >= m.width-25 {
			m.mode = ModeNormal
			return m, m.maybeSetWindowTitle()
		}
	}

	// BubbleTea reports mouse Y relative to the host terminal (row 0
	// = top of TUI). The pane's content sits below the agent view's
	// chrome (1-row header, plus optional 1-row deps line). Subtract
	// the chrome height so the pane sees pane-relative coordinates;
	// otherwise selection lands one (or two) rows below the cursor.
	chrome := m.agentViewChromeHeight()
	adjusted := msg
	adjusted.Y = msg.Y - chrome
	if adjusted.Y < 0 {
		// Click landed on the chrome itself (header or deps line).
		// Row 0 / close-button case is handled above; other chrome
		// clicks are no-ops.
		return m, nil
	}

	pane.HandleMouse(adjusted)
	return m, nil
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
func agentChromeHeight(hasDeps bool) int {
	if hasDeps {
		return 2
	}
	return 1
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
		if err := m.projectRegistry.Delete(p.ID); err != nil {
			m.notify("Failed to delete: " + err.Error())
			return nil
		}

		m.globalStore.RemoveProject(p.ID)
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

func (m *Model) visibleTicketCount() int {
	availableHeight := m.columnContentHeight() - indicatorReserveRows
	if availableHeight <= 0 {
		return 1
	}
	count := availableHeight / ticketHeight
	return max(count, 1)
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
	visible := m.visibleTicketCount()

	if m.activeTicket < offset {
		m.columnOffsets[m.activeColumn] = m.activeTicket
	} else if m.activeTicket >= offset+visible {
		m.columnOffsets[m.activeColumn] = m.activeTicket - visible + 1
	}

	m.columnOffsets[m.activeColumn] = max(m.columnOffsets[m.activeColumn], 0)
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

func (m *Model) attachToAgent() (tea.Model, tea.Cmd) {
	ticket := m.selectedTicket()
	if ticket == nil {
		m.notify("No ticket selected")
		return m, nil
	}

	pane, ok := m.panes[ticket.ID]
	if !ok || !pane.Running() {
		m.notify("No agent running — press 's' to spawn")
		return m, nil
	}

	m.mode = ModeAgentView
	m.focusedPane = ticket.ID
	paneHeight := m.height - 2
	pane.SetSize(m.width, paneHeight)

	// If we have a daemon-owned but unattached pane, do the binary
	// upgrade now so the user sees a live screen.
	var attachCmd tea.Cmd
	if pane.State() == daemonclient.PaneViewUnattached {
		attachCmd = m.attachExisting(ticket.ID, pane)
	}
	return m, tea.Batch(attachCmd, m.maybeSetWindowTitle())
}

// attachExisting performs the daemon attach (or takeover, if another
// client owns the binary stream) for a PaneView the model already
// holds. Returns a tea.Cmd that runs the attach in the background and
// then arms the pane's tea message reader.
//
// Used by:
//   - attachToAgent (Enter on a daemon-owned ticket from board view)
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
	ticket := m.selectedTicket()
	if ticket == nil {
		return m, nil
	}

	pane, ok := m.panes[ticket.ID]
	if ok && pane.Running() {
		return m.attachToAgent()
	}

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

	m.globalStore.Move(ticket.ID, nextStatus)
	m.refreshColumnTickets()
	m.selectTicketByID(ticket.ID)
	m.saveTicket(ticket)
	m.notify("Moved to " + string(nextStatus))

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

	m.globalStore.Move(ticket.ID, prevStatus)
	m.refreshColumnTickets()
	m.selectTicketByID(ticket.ID)
	m.saveTicket(ticket)
	m.notify("Moved to " + string(prevStatus))

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

func (m *Model) spawnAgent() (tea.Model, tea.Cmd) {
	ticket := m.selectedTicket()
	if ticket == nil {
		return m, nil
	}

	if ticket.Status != board.StatusInProgress {
		m.notify("Press Space to move to In Progress first")
		return m, nil
	}

	if existing, exists := m.panes[ticket.ID]; exists {
		switch existing.State() {
		case daemonclient.PaneViewAttached:
			m.notify("Agent already running — press Enter to attach")
			return m, nil
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
		TicketID:         ticketIDStr,
		SessionName:      in.sessionName,
		Command:          in.command,
		Args:             args,
		Workdir:          in.workdir,
		Env:              env,
		Cols:             in.cols,
		Rows:             in.rows,
		Scrollback:       0,
		AgentSessionUUID: in.ticket.AgentSessionID,
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
			ticket:            ticket,
			plan:              plan,
			sessionName:       sessionName,
			command:           command,
			workdir:           worktreePath,
			cols:              width,
			rows:              height,
			agentType:         agentType,
			cleanArgs:         cleanArgs,
			isNewSession:      isNewSession,
			promptTemplate:    promptTemplate,
			ctxData:           ctxData,
			agentPort:         agentPort,
			opencodeSessionID: opencodeSessionID,
			geminiSessionID:   geminiSessionID,
			codexSessionID:    codexSessionID,
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
		m.columnTickets[i] = filtered
	}

	if len(m.columnOffsets) != len(m.columns) {
		m.columnOffsets = make([]int, len(m.columns))
	}
}

func (m *Model) ticketMatchesFilter(t *board.Ticket) bool {
	if len(m.filterProjectIDs) > 0 && !m.filterProjectIDs[t.ProjectID] {
		return false
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
		return board.StatusDone
	default:
		return current
	}
}

func (m *Model) previousStatus(current board.TicketStatus) board.TicketStatus {
	switch current {
	case board.StatusDone:
		return board.StatusInProgress
	case board.StatusInProgress:
		return board.StatusBacklog
	default:
		return current
	}
}

func (m *Model) notify(msg string) {
	m.notification = msg
	m.notifyTime = time.Now()
}

func (m *Model) saveTicket(ticket *board.Ticket) {
	if err := m.globalStore.Save(ticket); err != nil {
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

const gracefulShutdownTimeout = 3 * time.Second

// T2 of the integration plan removed maybeAutoStopCompletedPane.
// Ticket-done now flows CLI → daemon (TicketDoneReq) → SessionEvent
// broadcast; subscribed TUIs react via handleDaemonSessionEvent with
// the authoritative Expected=true signal. No per-TUI poll-driven kill
// path remains.

func (m *Model) Cleanup() {
	for _, pane := range m.panes {
		if pane.Running() {
			pane.StopGraceful(gracefulShutdownTimeout)
		}
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

			status := detector.DetectStatusWithPort(p.agentType, sessionID, p.worktreePath, p.agentPort, true, p.terminalContent)
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
