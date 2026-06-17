# Data Model & Persistence

This document defines the data structures, persistence strategies, and state management for OpenKanban.

## Core Data Structures

### Ticket

The fundamental unit of work. Each ticket represents a task with an associated git worktree and agent session.

```go
type TicketID string // UUID v4

type TicketStatus string

const (
    StatusBacklog    TicketStatus = "backlog"
    StatusInProgress TicketStatus = "in_progress"
    StatusDone       TicketStatus = "done"
    StatusArchived   TicketStatus = "archived"
)

type AgentStatus string

const (
    AgentIdle      AgentStatus = "idle"      // Session exists, no activity
    AgentWorking   AgentStatus = "working"   // Active output detected
    AgentWaiting   AgentStatus = "waiting"   // Waiting for user input
    AgentCompleted AgentStatus = "completed" // Agent reported done
    AgentError     AgentStatus = "error"     // Agent crashed/errored
    AgentNone      AgentStatus = "none"      // No session spawned
)

type Ticket struct {
    ID          TicketID     `json:"id"`
    ProjectID   string       `json:"project_id"`
    Title       string       `json:"title"`
    Description string       `json:"description,omitempty"`
    Status      TicketStatus `json:"status"`

    // Git integration
    UseWorktree  bool   `json:"use_worktree"`
    WorktreePath string `json:"worktree_path,omitempty"`
    BranchName   string `json:"branch_name,omitempty"`
    BaseBranch   string `json:"base_branch,omitempty"` // e.g., "main"

    // Agent integration (embedded PTY terminals, not tmux)
    AgentType      string      `json:"agent_type,omitempty"` // "claude", "opencode", "aider", "gemini", "codex"
    AgentStatus    AgentStatus `json:"agent_status"`
    AgentSpawnedAt *time.Time  `json:"agent_spawned_at,omitempty"`
    AgentPort      int         `json:"agent_port,omitempty"`        // Per-ticket opencode port
    AgentSessionID string      `json:"agent_session_id,omitempty"`  // Claude/opencode session UUID once known

    // Metadata
    CreatedAt   time.Time  `json:"created_at"`
    UpdatedAt   time.Time  `json:"updated_at"`
    StartedAt   *time.Time `json:"started_at,omitempty"`   // When moved to in_progress
    CompletedAt *time.Time `json:"completed_at,omitempty"` // When moved to done

    // User-defined
    Labels    []string          `json:"labels,omitempty"`
    Priority  int               `json:"priority,omitempty"` // 1=highest, 5=lowest
    Meta      map[string]string `json:"meta,omitempty"`     // Custom key-value pairs
    BlockedBy []TicketID        `json:"blocked_by,omitempty"` // Informational; no enforcement
}
```

The `json:` tags reflect the legacy single-file JSON layout. The
current on-disk shape is YAML frontmatter inside a Markdown file (see
[Per-Ticket File Format](#per-ticket-file-format) below) — the
frontmatter field names match the JSON tags 1:1.

### Project

A Project represents a registered git repository. Each git repo is one Project.

```go
type Project struct {
    ID          string          `json:"id"`
    Name        string          `json:"name"`
    RepoPath    string          `json:"repo_path"`    // Absolute path to git repo root
    WorktreeDir string          `json:"worktree_dir"` // Where worktrees go (default: {repo}-worktrees)
    CreatedAt   time.Time       `json:"created_at"`
    UpdatedAt   time.Time       `json:"updated_at"`
    Settings    ProjectSettings `json:"settings"`
}

type ProjectSettings struct {
    AutoSpawnAgent   bool   `json:"auto_spawn_agent"`
    AutoCreateBranch bool   `json:"auto_create_branch"`
    BranchPrefix     string `json:"branch_prefix,omitempty"`
    BranchNaming     string `json:"branch_naming,omitempty"`   // "template" | "ai" | "prompt"
    BranchTemplate   string `json:"branch_template,omitempty"` // e.g., "{prefix}{slug}"
    SlugMaxLength    int    `json:"slug_max_length,omitempty"` // default: 40
}
```

### Column

Columns define the board layout and map to ticket statuses.

```go
type Column struct {
    ID     string       `json:"id"`
    Name   string       `json:"name"`
    Status TicketStatus `json:"status"` // Maps to ticket status
    Color  string       `json:"color"`  // Hex color for column header
    Limit  int          `json:"limit"`  // WIP limit (0 = unlimited)
}
```

### Application State

Runtime state for the TUI application.

```go
type AppState struct {
    // Current board
    Board *Board
    
    // UI state
    ActiveColumn int        // Currently selected column index
    ActiveTicket int        // Currently selected ticket index within column
    Mode         UIMode     // Normal, Insert, Command, Help
    
    // Filtering/search
    FilterLabels []string
    SearchQuery  string
    
    // Cached views
    ColumnTickets [][]TicketID // Tickets per column (filtered/sorted)
    
    // Agent monitoring
    AgentStatuses map[TicketID]AgentStatus // Real-time status cache
    LastPoll      time.Time
}

type UIMode string

const (
    ModeNormal  UIMode = "normal"
    ModeInsert  UIMode = "insert"  // Editing ticket
    ModeCommand UIMode = "command" // : command mode
    ModeHelp    UIMode = "help"    // Help overlay
    ModeConfirm UIMode = "confirm" // Confirmation dialog
)
```

## Persistence Strategy

### File-Based Storage

OpenKanban uses a multi-file layout with one Markdown file per ticket:

```
~/.config/openkanban/
├── config.json                       # Global configuration
├── projects.json                     # Project registry
├── watch-errors.log                  # Reload failures (parse / validation)
└── tickets/
    ├── <project_id>/                 # One directory per registered project
    │   ├── <slug>-<uuid8>.md         # One file per ticket
    │   └── ...
    ├── <project_id>.json.migrated    # Rollback artifact after migration (optional)
    └── archived/
        └── <project_id>_<ts>/        # Whole project dirs land here when removed
```

### Per-Ticket File Format

Each ticket lives in its own `.md` file. The filename is `<slug>-<uuid8>.md`
where `slug` is a `Slugify(title, 40)` of the title and `uuid8` is the first
8 characters of the ticket's UUID. **Filename is cosmetic — identity comes
from the `id` field in the frontmatter.** A title edit writes the new file
then removes the old; if interrupted, the next load picks the
newer-by-mtime and deletes the stale duplicate.

```markdown
---
id: 7f3a9b2c-1d8e-4a5b-9c3d-2f1e0a8b9c4d
project_id: proj-abc
title: Wire fsnotify watcher
status: in_progress
priority: 2
labels:
  - storage
  - tui
created_at: 2026-06-12T10:00:00Z
updated_at: 2026-06-12T11:42:00Z
started_at: 2026-06-12T11:00:00Z
completed_at: null
use_worktree: true
worktree_path: /Users/cmeid/wt/task-fsnotify
branch_name: task/fsnotify
base_branch: main
agent_type: claude
blocked_by: []
meta: {}
# runtime fields — overwritten on agent spawn; on TUI startup, reset
# unless openkanbankd still owns a session for this ticket (in which
# case the on-disk values are believed and the daemon's reality reconciles
# them via subscribed events).
agent_status: working
agent_spawned_at: 2026-06-12T11:30:00Z
agent_port: 4097
agent_session_id: sess-42
---

Multi-line description goes here. Markdown formatting is preserved
verbatim — this body becomes the ticket's Description field on load.
```

The frontmatter is parsed with `gopkg.in/yaml.v3`. Enum values
(`status`, `agent_status`, `agent_type`) are validated against allowlists
at load time. Missing optional fields default to sensible values: empty
`status` → `backlog`, empty `agent_status` → `none`, zero timestamps →
`time.Now()`, missing `priority` → `3`. Hard-required: `id` (non-empty)
and `title` (non-empty after trim).

### Project Registry Format

Stored in `~/.config/openkanban/projects.json`. Unchanged from prior
versions:

```json
{
  "projects": {
    "proj-uuid-1": {
      "id": "proj-uuid-1",
      "name": "My Project",
      "repo_path": "/home/user/projects/myproject",
      "worktree_dir": "/home/user/projects/myproject-worktrees",
      "created_at": "2025-01-15T10:00:00Z",
      "updated_at": "2025-01-16T14:30:00Z",
      "settings": {
        "auto_spawn_agent": true,
        "auto_create_branch": true,
        "branch_prefix": "task/",
        "branch_naming": "template",
        "branch_template": "{prefix}{slug}",
        "slug_max_length": 40
      }
    }
  }
}
```

### Migration from Legacy JSON

Earlier versions stored tickets as a single file per project at
`tickets/<project_id>.json`. On first load, `LoadTicketStore` invokes
`MigrateProjectToPerTicket` which:

1. Reads and parses the legacy JSON
2. Stages each ticket as a `.md` in `tickets/<project_id>.migrating/`
3. Validates each staged file round-trips with matching id, title, and status
4. Atomically renames the staging dir to `tickets/<project_id>/`
5. Renames the legacy JSON to `<project_id>.json.migrated` (rollback artifact)

The migration is idempotent (re-running is a no-op when complete) and
orphan-safe (a `.migrating/` dir from a prior interrupted run is cleaned
before retry).

**Stale-snapshot recovery**: if a legacy JSON ever reappears after a
successful migration (e.g. an old binary launched in another shell and
wrote a stale snapshot of its in-memory store), the migration code
detects that the JSON is a strict subset of the current per-ticket dir
state (every ticket present with `updated_at` no older than the JSON's
record) and renames it to `<project_id>.json.stale-<unix-timestamp>`
rather than refusing to start.

**Rollback**: `mv <project_id>.json.migrated <project_id>.json && rm -rf <project_id>/`
and reinstall the older binary.

### In-Memory Stores

The `internal/project` package exposes two store types:

```go
// One project's tickets. Also caches each ticket's on-disk path so
// SaveTicket can atomically rewrite a single file without scanning.
type TicketStore struct {
    ProjectID string
    Tickets   map[board.TicketID]*board.Ticket
    UpdatedAt time.Time
    // paths (private) maps id → absolute .md path
}

func (s *TicketStore) SaveTicket(t *board.Ticket) error     // atomic tmp+rename per file
func (s *TicketStore) Delete(id board.TicketID) error       // map + on-disk file
func (s *TicketStore) DeleteTicketFile(id board.TicketID) error  // file only
func (s *TicketStore) SaveAll() error                       // fan out across in-memory map

// All projects, aggregated. Wraps a TicketStore per project.
type GlobalTicketStore struct { /* ... */ }

func (g *GlobalTicketStore) Save(ticket *board.Ticket) error                       // delegates to SaveTicket
func (g *GlobalTicketStore) ReloadTicket(projectID, path string) error             // file-watcher entrypoint
func (g *GlobalTicketStore) MoveProject(id board.TicketID, newProjectID string) error
func (g *GlobalTicketStore) RemoveProject(id string) error                         // archives whole dir
```

`SaveTicket` writes a single file via tmp+rename. **No other ticket's file
is touched** — the load-bearing property behind hot-reload safety. A
parent agent session saving ticket A cannot clobber a child session's
edits to ticket B; cross-ticket conflicts are impossible by construction.

### File-Watcher Integration

The `internal/watch` package runs an `fsnotify.Watcher` rooted at the
config dir plus each project's tickets subdir. Events are debounced
~100ms per path, classified by parent directory + basename, and pushed
into the Bubble Tea event loop as `ui.FsChangedMsg` via `program.Send`.
On macOS, fsnotify uses kqueue. Watching a directory does NOT bound
the fd count — fsnotify's kqueue backend opens one fd per file inside
each watched dir so `EVFILT_VNODE` can deliver Write events (kqueue's
vnode filter only works on individual fds). fd footprint therefore
scales with ticket count regardless of whether we `Add()` the dir or
each file. FSEvents is the only real fix if `ulimit -n` ever bites.
See `internal/watch/watcher.go` package doc for the per-fd accounting.

Editor swap files (`.tmp`, `.swp`, `~`, leading-dot, vim's `4913`) are
filtered out at the classifier.

**Self-write suppression**: the TUI's reload handler records
`(path, mtime, size, deadline)` after each `SaveTicket` call and
checks incoming events against that window (5 second TTL). This
prevents fsnotify echoes of the TUI's own writes from triggering
redundant reloads.

When a reload fails (malformed YAML, invalid enum value, etc.) the
prior in-memory ticket is kept and the failure is appended to
`~/.config/openkanban/watch-errors.log` as
`<RFC3339 timestamp>\t<path>\t<error>`. The user also sees a transient
TUI notification.

## State Transitions

### Ticket Lifecycle

```
                    ┌─────────────┐
                    │   Created   │
                    └──────┬──────┘
                           │
                           ▼
┌──────────────────────────────────────────────────────────┐
│                      BACKLOG                              │
│  - No worktree                                           │
│  - No agent session                                      │
│  - agent_status = "none"                                 │
└──────────────────────┬───────────────────────────────────┘
                       │ Move to In Progress
                       │ (triggers: create worktree, spawn agent)
                       ▼
┌──────────────────────────────────────────────────────────┐
│                   IN PROGRESS                             │
│  - Worktree created at {worktree_dir}/{branch_name}      │
│  - Branch created: {branch_prefix}{slug}                 │
│  - Embedded PTY terminal with agent process              │
│  - agent_status cycles: idle → working → waiting → ...   │
└──────────────────────┬───────────────────────────────────┘
                       │ Move to Done
                       │ (triggers: optional cleanup prompt)
                       ▼
┌──────────────────────────────────────────────────────────┐
│                       DONE                                │
│  - Worktree can be kept or removed                       │
│  - Agent session terminated                              │
│  - agent_status = "completed" or "none"                  │
│  - Branch ready for PR                                   │
└──────────────────────┬───────────────────────────────────┘
                       │ Archive (optional)
                       ▼
┌──────────────────────────────────────────────────────────┐
│                     ARCHIVED                              │
│  - Hidden from default view                              │
│  - Worktree removed                                      │
│  - Historical record preserved                           │
└──────────────────────────────────────────────────────────┘
```

### Agent Status Transitions

```
     spawn agent
         │
         ▼
      ┌──────┐
      │ idle │◄─────────────────────┐
      └──┬───┘                      │
         │ activity detected        │ no activity (30s)
         ▼                          │
    ┌─────────┐                     │
    │ working │─────────────────────┘
    └────┬────┘
         │ waiting for input
         ▼
    ┌─────────┐
    │ waiting │ (prompt detected)
    └────┬────┘
         │ user responds OR
         │ agent continues
         ▼
    ┌─────────────┐
    │ working/idle│
    └─────────────┘

    Error states:
    - Process exits unexpectedly → "error"
    - Status file says "done" → "completed"
    - PTY process killed → "none"
```

## Global Configuration

```go
type Config struct {
    Defaults BoardSettings          `json:"defaults"`
    Agents   map[string]AgentConfig `json:"agents"`
    UI       UIConfig               `json:"ui"`
    Cleanup  CleanupSettings        `json:"cleanup"`
    Behavior BehaviorSettings       `json:"behavior"`
    Opencode OpencodeSettings       `json:"opencode"`
    Keys     map[string]string      `json:"keys,omitempty"`
}

type BoardSettings struct {
    DefaultAgent     string `json:"default_agent"`
    WorktreeBase     string `json:"worktree_base"`
    AutoSpawnAgent   bool   `json:"auto_spawn_agent"`
    AutoCreateBranch bool   `json:"auto_create_branch"`
    BranchPrefix     string `json:"branch_prefix"`
    BranchNaming     string `json:"branch_naming"`   // "template" | "ai" | "prompt"
    BranchTemplate   string `json:"branch_template"` // e.g., "{prefix}{slug}"
    SlugMaxLength    int    `json:"slug_max_length"` // default: 40
    InitPrompt       string `json:"init_prompt"`
}

type AgentConfig struct {
    Command    string            `json:"command"`
    Args       []string          `json:"args"`
    Env        map[string]string `json:"env"`
    StatusFile string            `json:"status_file"`
    InitPrompt string            `json:"init_prompt"`
}

type UIConfig struct {
    Theme           string `json:"theme"`
    ShowAgentStatus bool   `json:"show_agent_status"`
    RefreshInterval int    `json:"refresh_interval"`
    ColumnWidth     int    `json:"column_width"`
    TicketHeight    int    `json:"ticket_height"`
    SidebarVisible  bool   `json:"sidebar_visible"`
}

type CleanupSettings struct {
    DeleteWorktree       bool `json:"delete_worktree"`
    DeleteBranch         bool `json:"delete_branch"`
    ForceWorktreeRemoval bool `json:"force_worktree_removal"`
}

type BehaviorSettings struct {
    ConfirmQuitWithAgents bool `json:"confirm_quit_with_agents"`
}

type OpencodeSettings struct {
    ServerEnabled  bool `json:"server_enabled"`
    ServerPort     int  `json:"server_port"`
    PollInterval   int  `json:"poll_interval"`
    StartupTimeout int  `json:"startup_timeout"`
}
```

### Default Configuration File

```json
{
  "defaults": {
    "default_agent": "opencode",
    "worktree_base": "",
    "auto_spawn_agent": true,
    "auto_create_branch": true,
    "branch_prefix": "task/",
    "branch_naming": "template",
    "branch_template": "{prefix}{slug}",
    "slug_max_length": 40
  },
  "agents": {
    "claude": {
      "command": "claude",
      "args": ["--dangerously-skip-permissions"],
      "env": {},
      "status_file": ".claude/status.json"
    },
    "opencode": {
      "command": "opencode",
      "args": [],
      "env": {},
      "status_file": ".opencode/status.json"
    },
    "aider": {
      "command": "aider",
      "args": ["--yes"],
      "env": {},
      "status_file": ""
    }
  },
  "ui": {
    "theme": "catppuccin-mocha",
    "show_agent_status": true,
    "refresh_interval": 5,
    "column_width": 40,
    "ticket_height": 4,
    "sidebar_visible": true
  },
  "cleanup": {
    "delete_worktree": true,
    "delete_branch": false,
    "force_worktree_removal": false
  },
  "behavior": {
    "confirm_quit_with_agents": true
  },
  "opencode": {
    "server_enabled": true,
    "server_port": 4096,
    "poll_interval": 1,
    "startup_timeout": 10
  }
}
```

## File Paths

| Purpose | Path | Notes |
|---------|------|-------|
| Global config | `~/.config/openkanban/config.json` | User preferences |
| Project registry | `~/.config/openkanban/projects.json` | All registered projects |
| Per-ticket file | `~/.config/openkanban/tickets/<project_id>/<slug>-<uuid8>.md` | One Markdown file per ticket |
| Legacy rollback | `~/.config/openkanban/tickets/<project_id>.json.migrated` | Pre-migration JSON, preserved for rollback |
| Stale snapshot | `~/.config/openkanban/tickets/<project_id>.json.stale-<ts>` | A legacy JSON renamed aside after stale-snapshot recovery |
| Archived projects | `~/.config/openkanban/tickets/archived/<project_id>_<ts>/` | Whole project dirs after RemoveProject |
| Watch errors | `~/.config/openkanban/watch-errors.log` | Reload failures (parse / validation) |
| Worktrees | `{repo}-worktrees/` | Default sibling to repo |
| Status cache | `~/.cache/openkanban-status/` | Agent status files |

## Concurrency Considerations

1. **No cross-ticket conflicts by construction**: per-ticket files mean
   different writers touching different tickets never share a write
   target. A parent agent saving ticket A and a child agent editing
   ticket B's file in `$EDITOR` cannot collide.
2. **Atomic writes**: every persistence path writes to `<dest>.tmp` and
   then `os.Rename`s onto the destination. No partial-write window.
3. **Same-ticket simultaneous writes** still race at the file level
   (last writer wins) but this is a much smaller surface than the
   legacy whole-file rewrites. The TUI is single-threaded via Bubble
   Tea's event loop; conflicts only arise across processes.
4. **File locking scope**: Per-ticket `.md` files do not use `flock` —
   external writers (and openkanban itself) coordinate via the
   tmp+rename pattern in (2) instead, which most editors already do.
   The one exception is the daemon's pidfile at
   `~/.cache/openkanban/daemon.pid`, which uses
   `flock(LOCK_EX|LOCK_NB)` to enforce single-instance: a second
   `openkanban daemon` invocation observes the lock held and exits
   non-zero. The lock is on the daemon's *runtime state*, not on
   ticket content; ticket files are still lock-free.
5. **Watcher loop is single-goroutine**: the fsnotify event channel is
   consumed by one goroutine in `internal/app/app.go` that calls
   `program.Send`. The model's `Update` handler then mutates state in
   place. No additional locking needed.
6. **PTY operations**: terminal panes managed per-ticket with mutex
   protection in `internal/terminal`.

```go
// internal/project/tickets.go — SaveTicket pattern
func (s *TicketStore) SaveTicket(t *board.Ticket) error {
    dir := s.ticketDir()
    if err := os.MkdirAll(dir, 0o755); err != nil { return err }

    data, err := MarshalTicket(t)
    if err != nil { return err }

    newPath := filepath.Join(dir, TicketFilename(t))
    tmpPath := newPath + ".tmp"
    if err := os.WriteFile(tmpPath, data, 0o644); err != nil { return err }
    if err := os.Rename(tmpPath, newPath); err != nil { return err }

    // Title-edit rename: if the cached path differs from the new
    // path, remove the old file. Frontmatter id is canonical
    // identity; orphans on interruption are reconciled on next load.
    if oldPath, hadOld := s.paths[t.ID]; hadOld && oldPath != newPath {
        _ = os.Remove(oldPath)
    }
    s.paths[t.ID] = newPath
    s.Tickets[t.ID] = t
    return nil
}
```
