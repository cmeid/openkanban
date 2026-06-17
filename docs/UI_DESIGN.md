# UI Design & TUI Wireframes

This document defines the visual design, component hierarchy, and styling for Agent Board's terminal user interface.

## Design Philosophy

- **Information density**: Show maximum useful info without clutter
- **Vim-native**: hjkl navigation, modal editing, command mode
- **Status at a glance**: Agent states visible via color/icons
- **Keyboard-first**: Every action has a keybinding

## Main Board View

### Wireframe: Default Layout

```
┌─ Agent Board ───────────────────────────────────────────────────────────────────────────────────────┐
│ myproject (~/projects/myproject)                                                     ? help  q quit │
├─────────────────────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                                     │
│  ┌─ Backlog (4) ──────┐  ┌─ In Progress (2/3) ┐  ┌─ In Review (1) ────┐  ┌─ Done (7) ─────────┐     │
│  │                    │  │                    │  │                    │  │                    │     │
│  │ ┌────────────────┐ │  │ ┌────────────────┐ │  │ ┌────────────────┐ │  │ ┌────────────────┐ │     │
│  │ │ #3 Add user    │ │  │ │▶#1 Auth system │ │  │ │ #10 OAuth flow │ │  │ │ #5 Setup CI/CD │ │     │
│  │ │ profile page   │ │  │ │ ● Working      │ │  │ │ awaiting Chris │ │  │ │ ✓ Completed    │ │     │
│  │ │ [frontend]     │ │  │ │ [backend,auth] │ │  │ │ [auth]         │ │  │ │ [devops]       │ │     │
│  │ └────────────────┘ │  │ └────────────────┘ │  │ └────────────────┘ │  │ └────────────────┘ │     │
│  │                    │  │                    │  │                    │  │                    │     │
│  │ ┌────────────────┐ │  │ ┌────────────────┐ │  │                    │  │ ┌────────────────┐ │     │
│  │ │ #4 API rate    │ │  │ │▶#2 Database    │ │  │                    │  │ │ #6 Add README  │ │     │
│  │ │ limiting       │ │  │ │ ○ Idle         │ │  │                    │  │ │ ✓ Completed    │ │     │
│  │ │ [backend]      │ │  │ │ [backend,db]   │ │  │                    │  │ │ [docs]         │ │     │
│  │ └────────────────┘ │  │ └────────────────┘ │  │                    │  │ └────────────────┘ │     │
│  │                    │  │                    │  │                    │  │                    │     │
│  │ ┌────────────────┐ │  │                    │  │                    │  │ ┌────────────────┐ │     │
│  │ │ #7 Write tests │ │  │                    │  │                    │  │ │ #8 Fix login   │ │     │
│  │ │                │ │  │                    │  │                    │  │ │ ✓ Completed    │ │     │
│  │ │ [testing]      │ │  │                    │  │                    │  │ │ [bugfix]       │ │     │
│  │ └────────────────┘ │  │                    │  │                    │  │ └────────────────┘ │     │
│  │                    │  │                    │  │                    │  │                    │     │
│  │ ┌────────────────┐ │  │                    │  │                    │  │        ...         │     │
│  │ │ #9 Refactor    │ │  │                    │  │                    │  │   (+4 more)        │     │
│  │ │ auth module    │ │  │                    │  │                    │  │                    │     │
│  │ │ [refactor]     │ │  │                    │  │                    │  │                    │     │
│  │ └────────────────┘ │  │                    │  │                    │  │                    │     │
│  │                    │  │                    │  │                    │  │                    │     │
│  └────────────────────┘  └────────────────────┘  └────────────────────┘  └────────────────────┘     │
│                                                                                                     │
├─────────────────────────────────────────────────────────────────────────────────────────────────────┤
│ NORMAL │ j/k: navigate │ h/l: columns │ Enter: attach │ n: new │ m: move                            │
└─────────────────────────────────────────────────────────────────────────────────────────────────────┘
```

### Wireframe: Ticket Selected (Highlighted)

```
  ┌─ In Progress (2/3) ┐
  │                    │
  │ ╔════════════════╗ │   ← Selected ticket has double border
  │ ║▶#1 Auth system ║ │
  │ ║ ● Working      ║ │
  │ ║ [backend,auth] ║ │
  │ ╚════════════════╝ │
  │                    │
  │ ┌────────────────┐ │
  │ │▶#2 Database    │ │
  │ │ ○ Idle         │ │
```

### Wireframe: Compact Mode (Small Terminal)

```
┌─ Agent Board ── myproject ──────────────────────────────────────────────┐
├─────────────────────────────────────────────────────────────────────────┤
│ Backlog(4)     In Progress(2)  In Review(1)    Done(7)                  │
│ ────────────   ───────────────  ────────────    ─────────               │
│ #3 Profile     ▶#1 Auth ●      #10 OAuth ✓     #5 CI/CD ✓               │
│ #4 Rate limit  ▶#2 DB ○                        #6 README ✓              │
│ #7 Tests                                       #8 Login ✓               │
│ #9 Refactor                                    (+4)                     │
├─────────────────────────────────────────────────────────────────────────┤
│ j/k:nav h/l:col Enter:attach n:new m:move ?:help                        │
└─────────────────────────────────────────────────────────────────────────┘
```

### Header Activity Chip

A persistent badge in the top-right of the header reports the live
session count, grouped by `AgentStatus` over panes whose
`PaneView.Running()` is true. The chip is **always rendered** — even
with zero sessions — so the header footprint doesn't shift as work
starts and ends.

| Condition (priority order) | Text                                | Background  | Foreground |
|----------------------------|-------------------------------------|-------------|------------|
| `waiting > 0`              | `◐ N waiting` (+ `, M working`)     | `secondary` | `base`     |
| `working > 0`              | `<spinner> N working`               | `warning`   | `base`     |
| `idle > 0`                 | `◆ N idle`                          | `primary`   | `base`     |
| all zero                   | `○ 0 sessions`                      | `surface`   | `muted`    |

The zero-state pairing (`bg=surface`, `fg=muted`) is the same subdued
treatment used by the column scroll-overflow indicators — it reads as
"inactive" rather than "alarming" and keeps the chip from competing
with active-state colors for attention.

Rendered by `renderHeader()` in `internal/ui/view.go`. The
`waiting` branch wins when both waiting and working are non-zero
because the user-attention semantics of "an agent is waiting on you"
outrank "an agent is making progress."

## Embedded Session View

When the user attaches into a running agent (Enter on an in-progress
ticket), the TUI enters `ModeAgentView` and the inner PTY takes over
the full window. A title bar at the top names the session and surfaces
state at a glance; the rest of the screen is the agent's terminal.

### Wireframe: Embedded Session

```
┌──────────────────────────────────────────────────────────────────────────────┐
│⎯⎯ Implement auth │ openkanban │ claude │ ● working │ ⏱ 2h15m   [1/3] Ctrl+g│  ← surface-tinted band
│⛓↑ Schema design                                                              │  ← optional deps row
│                                                                              │
│  $ claude --resume … --fork-session                                          │
│  ⏵ Adding JWT signing helper to internal/auth/token.go                       │
│  ...                                                                         │
│  (live PTY content)                                                          │
└──────────────────────────────────────────────────────────────────────────────┘
```

### Title Bar Segments (left to right)

| Segment              | Source                       | Notes                                                              |
|----------------------|------------------------------|--------------------------------------------------------------------|
| Priority glyph       | `ticket.Priority` (1–5)      | Same convention as the card view (P1=err, P5=muted).               |
| Bold title           | `ticket.Title`               | The lead element — no breadcrumb.                                  |
| Project badge        | `project.Name`               | Suppressed when the project is single.                             |
| Agent type badge     | `ticket.AgentType`           | "claude", etc.                                                     |
| Status pill          | `ticket.AgentStatus`         | `● working` / `◐ waiting` / `◆ idle` / `✓ done` / `✗ error`.       |
| ⏱ Duration           | `time.Since(AgentSpawnedAt)` | Session age. Suppressed when status is `done` (pill carries it).   |
| `[n/m]` pane indicator | `Running()` panes          | Position of the focused pane among all running panes.              |
| Scroll indicator     | `pane.ViewportOffset()`      | `↑offset/total` in warning color when scrolled back.               |
| `Ctrl+g` hint        | `m.autoAttach`               | Reads `Ctrl+g board` normally, or `next waiter (Auto)` when Auto mode is on (toggle: board `a`). Ctrl+g leaves the session — to the board, or the oldest waiting session under Auto. Not the only intercepted key: `Ctrl+]`/`Ctrl+\` cycle sessions; Enter retries a failed attach.|

### Time Semantics

The `⏱` duration is **elapsed time since the agent was first spawned**
for this ticket. `AgentSpawnedAt` is set once on first spawn and
persists on the ticket's YAML frontmatter, so the counter survives TUI
and daemon restarts — it only resets on a force-fresh kill. There is
no agent last-activity timestamp; building an "idle for X" display
would require a new field written from the daemon's `started` /
`attached` push handlers.

### Status Pill Colors

| Status      | Glyph | Color     |
|-------------|-------|-----------|
| `working`   | ●     | success   |
| `waiting`   | ◐     | warning   |
| `idle`      | ◆     | muted     |
| `completed` | ✓     | success   |
| `error`     | ✗     | err       |
| `none`      | —     | (no pill) |

The whole header row (plus the optional deps row) renders with
`m.colors.surface` as background so the chrome reads as a distinct
band over the embedded PTY content.

## Component Hierarchy

```
App
├── Header
│   ├── Logo/Title
│   ├── BoardName
│   ├── RepoPath
│   └── QuickHelp (? q)
│
├── BoardView
│   ├── Column (repeated)
│   │   ├── ColumnHeader
│   │   │   ├── Title
│   │   │   ├── Count
│   │   │   └── WIPIndicator
│   │   │
│   │   └── TicketList
│   │       └── TicketCard (repeated)
│   │           ├── ID
│   │           ├── Title
│   │           ├── AgentIndicator
│   │           ├── StatusBadge
│   │           └── Labels
│   │
│   └── ScrollIndicator
│
├── StatusBar
│   ├── Mode
│   ├── KeyHints
│   └── Notifications
│
└── Overlays (conditional)
    ├── HelpModal
    ├── TicketDetailModal
    ├── NewTicketForm
    ├── CommandPalette
    └── ConfirmDialog
```

## Ticket Card States

### Agent Status Indicators

| Status | Icon | Color | Description |
|--------|------|-------|-------------|
| None | (none) | Gray | No agent spawned |
| Idle | ○ | Blue | Session exists, no activity |
| Working | ● | Yellow (animated) | Active processing |
| Waiting | ◐ | Magenta | Awaiting user input |
| Completed | ✓ | Green | Agent finished |
| Error | ✗ | Red | Agent crashed |

### TUI Viewing Indicator

Orthogonal to agent status — signals whether any TUI client is
currently in agent-view mode on this ticket. Distinct from the
daemon's "attached" semantic (PTY-stream ownership, which typically
spans the whole session lifetime): viewing is the narrower "user is
focused on it right now" signal, flipping on Enter / off on Esc.

| State        | Icon   | Color | Description                                                |
|--------------|--------|-------|------------------------------------------------------------|
| Not viewed   | (none) | —     | No TUI has this session open in agent view                 |
| Being viewed | ◉      | info  | One or more TUIs are in `ModeAgentView` on this session    |

Rendered in the card's header badge row, after the agent-status badge.
Driven by `SetViewing` RPC calls every TUI makes on agent-view
enter/leave; the daemon broadcasts `viewing` / `unviewing`
`SessionEvent`s and each subscribed TUI maintains its own counter. A
TUI is most useful when looking at a sibling instance — your own
window can't show its own viewing state while you're in agent view on
that very session. See
[AGENT_INTEGRATION.md → TUI viewing signal](AGENT_INTEGRATION.md#tui-viewing-signal-distinct-from-attach)
for the full RPC flow.

### Column Position Badge

A muted `N/total` prefix sits at the head of each card's header row,
where `N` is the card's 1-indexed position in its column and `total`
is the column's card count. Makes the cursor's location — and how
many cards sit above and below — visible at a glance without having
to scroll.

| Element | Value         | Foreground |
|---------|---------------|------------|
| Format  | `%d/%d`       | `muted`    |
| Index   | `i+1` (1-based) | —        |
| Total   | `len(tickets)` (per column) | — |

Rendered first in `renderTicket`'s `headerParts` so priority, project,
deps, and session badges follow it. Muted styling keeps it from
competing with the indicators that change state — it's a static
position cue, not a status signal.

### Card Variations

```
Standard (backlog):          With Agent (in progress):     Completed:
┌────────────────┐           ┌────────────────┐           ┌────────────────┐
│ #3 Add profile │           │▶#1 Auth system │           │ #5 Setup CI/CD │
│                │           │ ● Working      │           │ ✓ Completed    │
│ [frontend]     │           │ [backend,auth] │           │ [devops]       │
└────────────────┘           └────────────────┘           └────────────────┘

Selected:                    Error state:                  High priority:
╔════════════════╗           ┌────────────────┐           ┌────────────────┐
║ #3 Add profile ║           │▶#4 API rate    │           │!#7 Critical    │
║                ║           │ ✗ Error        │           │                │
║ [frontend]     ║           │ [backend]      │           │ [urgent]       │
╚════════════════╝           └────────────────┘           └────────────────┘
```

## Modal Dialogs

### New Ticket Form

```
┌─ New Ticket ──────────────────────────────────────────┐
│                                                        │
│  Title:                                                │
│  ┌──────────────────────────────────────────────────┐ │
│  │ Implement user authentication                    │ │
│  └──────────────────────────────────────────────────┘ │
│                                                        │
│  Description:                                          │
│  ┌──────────────────────────────────────────────────┐ │
│  │ Add JWT-based auth to the API with:              │ │
│  │ - Login/logout endpoints                         │ │
│  │ - Token refresh                                  │ │
│  │ - Role-based permissions                         │ │
│  └──────────────────────────────────────────────────┘ │
│                                                        │
│  Labels: [backend] [auth] [+]                          │
│                                                        │
│  Priority: ● High  ○ Medium  ○ Low                     │
│                                                        │
│  Agent: [claude ▼]  ☑ Auto-spawn when started         │
│                                                        │
│  ┌─────────┐  ┌──────────┐                            │
│  │ Create  │  │  Cancel  │                            │
│  └─────────┘  └──────────┘                            │
│                                                        │
│  Tab: next field │ Shift+Tab: prev │ Enter: submit    │
└────────────────────────────────────────────────────────┘
```

### Ticket Detail View

```
┌─ Ticket #1 ─────────────────────────────────────────────────────────────────┐
│                                                                              │
│  Auth system implementation                                     [In Progress]│
│  ═══════════════════════════════════════════════════════════════════════════│
│                                                                              │
│  Description:                                                                │
│  Add JWT-based authentication to the API including login, logout,           │
│  token refresh, and role-based permissions.                                 │
│                                                                              │
│  ───────────────────────────────────────────────────────────────────────────│
│                                                                              │
│  Agent: claude           Status: ● Working                                  │
│  Branch: agent/auth-1    Session: ab-ticket-1                               │
│  Worktree: ~/projects/myproject-worktrees/auth-1                           │
│                                                                              │
│  ───────────────────────────────────────────────────────────────────────────│
│                                                                              │
│  Labels: [backend] [auth] [security]                                        │
│  Priority: High                                                              │
│  Created: 2025-01-15 10:30                                                  │
│  Started: 2025-01-16 09:00 (5h 30m ago)                                     │
│                                                                              │
│  ───────────────────────────────────────────────────────────────────────────│
│                                                                              │
│  [Enter] Attach to session  [e] Edit  [m] Move  [d] Delete  [Esc] Close    │
│                                                                              │
└──────────────────────────────────────────────────────────────────────────────┘
```

### Help Modal

```
┌─ Keyboard Shortcuts ─────────────────────────────────────────────────────────┐
│                                                                              │
│  Navigation                      Actions                                     │
│  ──────────────────────────────  ──────────────────────────────              │
│  h/l    Move between columns     n           New ticket                      │
│  j/k    Move between tickets     e           Edit ticket                     │
│  g/G    First / last ticket      d           Delete ticket (confirm)         │
│                                  Space       Move forward                    │
│                                  -/Bksp      Move backward                   │
│                                  K/J         Raise / lower priority          │
│                                                                              │
│  Sidebar                         Agent                                       │
│  ──────────────────────────────  ──────────────────────────────              │
│  [      Toggle sidebar           s/Enter     Open agent                      │
│  Tab    Focus sidebar            S           Stop agent                      │
│  h/l    Enter / exit sidebar     Ctrl+g      Exit (Auto: jump)               │
│  j/k    Navigate projects        Ctrl+]/\    Next / prev session             │
│  a      Add project              C-Space     Promote + bg agent              │
│  d      Delete project                                                       │
│  o      Toggle open only                                                     │
│                                                                              │
│  View                            System                                      │
│  ──────────────────────────────  ──────────────────────────────              │
│  /      Search / filter          O           Settings                        │
│  o      Cycle sort order         ?           Toggle help                     │
│  w      Toggle session filter    Esc         Cancel / back                   │
│  W      Show working (all proj)  Ctrl+R      Restart (binary updated)        │
│  a      Auto mode: Ctrl+g jumps  q           Quit                            │
│         to oldest waiting sess.                                              │
│                                                                              │
│  Ticket form                     Settings & dialogs                          │
│  ──────────────────────────────  ──────────────────────────────              │
│  Tab    Next field               j/k         Navigate items                  │
│  S-Tab  Previous field           Ent/Spc     Edit / toggle                   │
│  Ctrl+S Save                     Esc/q       Close                           │
│  1-5    Set priority             y/n         Confirm dialog                  │
│  Esc    Cancel form                                                          │
│                                                                              │
│                                                      Press any key to close  │
└──────────────────────────────────────────────────────────────────────────────┘
```

### Confirmation Dialog

```
                    ┌─ Confirm ─────────────────────────┐
                    │                                    │
                    │  Delete ticket #3?                 │
                    │                                    │
                    │  This will also:                   │
                    │  • Kill tmux session ab-ticket-3   │
                    │  • Remove worktree (optional)      │
                    │                                    │
                    │  ☐ Also delete git worktree        │
                    │  ☐ Also delete branch              │
                    │                                    │
                    │    [y] Yes    [n] No    [Esc]     │
                    │                                    │
                    └────────────────────────────────────┘
```

## Color Scheme (Themeable)

OpenKanban uses semantic color names that map to different hues per theme. This allows themes to use their own color palettes while maintaining consistent meaning.

### Theme Color Structure

```go
type ThemeColors struct {
    // Backgrounds
    Base    string // Main background
    Surface string // Elevated surfaces (cards, panels)
    Overlay string // Highest elevation (modals, dropdowns)

    // Text
    Text    string // Primary text
    Subtext string // Secondary text
    Muted   string // Disabled/placeholder text

    // Semantic accents
    Primary   string // Main accent (focus, selection, backlog column)
    Secondary string // Secondary accent (in-review column, special highlights)
    Success   string // Positive states (done column, confirmations)
    Warning   string // Caution states (in-progress column)
    Error     string // Errors, destructive actions
    Info      string // Informational elements
}
```

### Semantic Color Usage

| Purpose | Color | Usage |
|---------|-------|-------|
| Backlog column | `Primary` | Column header, selected ticket border |
| In Progress column | `Warning` | Column header, working agent indicator |
| In Review column | `Secondary` | Column header, awaiting-human-review state |
| Done column | `Success` | Column header, completed status |
| Agent idle | `Primary` | Idle session indicator |
| Agent working | `Warning` | Active processing (animated) |
| Agent waiting | `Secondary` | Awaiting user input |
| Agent error | `Error` | Crashed/failed state |
| High priority | `Error` | Critical/urgent tickets |
| Links/info | `Info` | Informational elements |

### Example: Catppuccin Mocha

```go
ThemeColors{
    Base:      "#1e1e2e",
    Surface:   "#313244",
    Overlay:   "#45475a",
    Text:      "#cdd6f4",
    Subtext:   "#bac2de",
    Muted:     "#6c7086",
    Primary:   "#89b4fa",  // Blue - backlog, selection
    Secondary: "#cba6f7",  // Mauve - special accents
    Success:   "#a6e3a1",  // Green - done, success
    Warning:   "#f9e2af",  // Yellow - in-progress
    Error:     "#f38ba8",  // Red - errors
    Info:      "#94e2d5",  // Teal - informational
}
```

### Example: Gruvbox Dark (Different Hues, Same Semantics)

```go
ThemeColors{
    Base:      "#282828",
    Surface:   "#3c3836",
    Overlay:   "#504945",
    Text:      "#ebdbb2",
    Subtext:   "#d5c4a1",
    Muted:     "#928374",
    Primary:   "#83a598",  // Aqua - backlog, selection
    Secondary: "#d3869b",  // Purple - special accents
    Success:   "#b8bb26",  // Green - done, success
    Warning:   "#fabd2f",  // Yellow - in-progress
    Error:     "#fb4934",  // Red - errors
    Info:      "#8ec07c",  // Green - informational
}
```

## Lipgloss Style Definitions

Colors are accessed via `m.colors` which is derived from the active theme:

```go
// Column colors are derived from status
func (m *Model) columnColor(status board.TicketStatus) lipgloss.Color {
    switch status {
    case board.StatusBacklog:
        return m.colors.primary
    case board.StatusInProgress:
        return m.colors.warning
    case board.StatusDone:
        return m.colors.success
    default:
        return m.colors.muted
    }
}

// Agent status colors
func (m *Model) agentColor(status board.AgentStatus) lipgloss.Color {
    switch status {
    case board.AgentIdle:
        return m.colors.primary
    case board.AgentWorking:
        return m.colors.warning
    case board.AgentWaiting:
        return m.colors.secondary
    case board.AgentCompleted:
        return m.colors.success
    case board.AgentError:
        return m.colors.err
    default:
        return m.colors.muted
    }
}
```

### Style Examples

```go
// Column header uses semantic column color
headerStyle := lipgloss.NewStyle().
    Foreground(m.columnColor(col.Status)).
    Bold(true)

// Selected ticket border matches column color
ticketBorder := lipgloss.NewStyle().
    Border(lipgloss.DoubleBorder()).
    BorderForeground(m.columnColor(col.Status))

// Modal uses primary accent
modalStyle := lipgloss.NewStyle().
    Border(lipgloss.RoundedBorder()).
    BorderForeground(m.colors.primary).
    Background(m.colors.surface)

// Error states use error color
errorStyle := lipgloss.NewStyle().
    Foreground(m.colors.err).
    Bold(true)

// Success states use success color
successStyle := lipgloss.NewStyle().
    Foreground(m.colors.success)
```

## Responsive Behavior

### Terminal Size Breakpoints

| Width | Layout | Adjustments |
|-------|--------|-------------|
| <80 | Compact | Single column, abbreviated cards |
| 80-120 | Standard | 3 columns, full cards |
| 120-160 | Wide | 3 columns + expanded cards with description |
| >160 | Ultra-wide | Optional 4th column (Archive) or wider cards |

### Height Adjustments

| Height | Behavior |
|--------|----------|
| <20 | Hide descriptions, minimal chrome |
| 20-40 | Standard layout |
| >40 | More visible tickets, expanded details |

### Resize Handler

```go
func (m Model) handleResize(width, height int) Model {
    m.width = width
    m.height = height
    
    // Calculate column widths
    if width < 80 {
        m.columnWidth = width - 4
        m.visibleColumns = 1
    } else if width < 120 {
        m.columnWidth = (width - 8) / 3
        m.visibleColumns = 3
    } else {
        m.columnWidth = 40
        m.visibleColumns = 3
    }
    
    // Calculate visible tickets per column
    cardHeight := 4 // title + status + labels + margin
    headerHeight := 4 // app header + column header
    footerHeight := 2 // status bar
    m.ticketsPerColumn = (height - headerHeight - footerHeight) / cardHeight
    
    return m
}
```

## Animation Considerations

### Agent Working Animation

Since terminal blink support is inconsistent, implement via polling:

```go
type agentAnimationMsg time.Time

func tickAgentAnimation() tea.Cmd {
    return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg {
        return agentAnimationMsg(t)
    })
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
    switch msg := msg.(type) {
    case agentAnimationMsg:
        m.animationFrame = (m.animationFrame + 1) % 4
        return m, tickAgentAnimation()
    }
    // ...
}

// Render working indicator with animation
func (m Model) renderAgentWorking() string {
    frames := []string{"●", "◐", "○", "◑"}
    return AgentWorking.Render(frames[m.animationFrame])
}
```

### Smooth Scrolling

Use viewport component from Bubbles:

```go
import "github.com/charmbracelet/bubbles/viewport"

type Column struct {
    viewport viewport.Model
    tickets  []Ticket
}
```

## Accessibility Considerations

1. **High contrast mode**: Alternative color scheme with higher contrast ratios
2. **No color-only indicators**: Icons accompany all status colors
3. **Screen reader hints**: Status bar announces current position
4. **Configurable refresh rate**: Reduce animations for sensitive users

```go
// High contrast alternative
var highContrastColors = struct {
    Background lipgloss.Color
    Text       lipgloss.Color
    Selected   lipgloss.Color
    // ...
}{
    Background: lipgloss.Color("#000000"),
    Text:       lipgloss.Color("#ffffff"),
    Selected:   lipgloss.Color("#00ff00"),
}
```
