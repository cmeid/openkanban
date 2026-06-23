# internal/ui

Bubbletea TUI layer. ~2500 lines across model.go + view.go.

## Overview

Elm-architecture implementation: Model holds all state, Update handles events via tea.Cmd, View renders.

## Modal System

| Mode | Purpose | Handler |
|------|---------|---------|
| `ModeNormal` | Board navigation | `handleNormalMode()` |
| `ModeCreateTicket` | New ticket form | `handleCreateTicketMode()` |
| `ModeEditTicket` | Edit existing | `handleEditTicketMode()` |
| `ModeAgentView` | Full-screen PTY | `handleAgentViewMode()` |
| `ModeSettings` | Config panel | `handleSettingsMode()` |
| `ModeFilter` | Search/filter | `handleFilterMode()` |
| `ModeSpawning` | Agent spawn in progress | Special case in `Update()` |
| `ModeShuttingDown` | Cleanup with spinner | Special case in `Update()` |
| `ModeConfirm` | Y/N dialog | `handleConfirm()` |

## Key Patterns

**Adding keybinding:**
```go
// In handleNormalMode()
case "x":
    return m.yourAction()
```

**New mode:**
1. Add to `Mode` const block
2. Create `handleYourMode(msg tea.KeyMsg)` 
3. Add case in `handleKey()` switch
4. Handle in `Update()` if needs special msg routing

**Async operation:**
```go
// Return tea.Cmd, never block
func (m *Model) doThing() (tea.Model, tea.Cmd) {
    return m, func() tea.Msg {
        result := expensiveOp()
        return thingDoneMsg{result}
    }
}
```

## Anti-Patterns

- **NEVER block in Update()** - Return tea.Cmd for I/O
- **NEVER mutate state in View()** - View is pure render
- **NEVER forget to refresh** - Call `refreshColumnTickets()` after ticket changes

## Form Fields

```go
const (
    formFieldTitle       = 0
    formFieldDescription = 1
    formFieldBranch      = 2
    formFieldLabels      = 3
    formFieldPriority    = 4
    formFieldWorktree    = 5
    formFieldBlockedBy   = 6
    formFieldProject     = 7
)
```

Navigate: `nextFormField()`, `prevFormField()`, focus with `focusCurrentField()`

## Key State

- `columnTickets [][]*board.Ticket` - Tickets grouped by column (filtered)
- `panes map[board.TicketID]*terminal.Pane` - Active PTY sessions
- `focusedPane board.TicketID` - Which pane has keyboard focus
- `filterProjectID string` - Current project filter
- `filterQuery string` - Text search filter
