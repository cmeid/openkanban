# Internal Packages

## Imports

```go
// 1. Standard library
"fmt"
"sync"

// 2. Third-party
tea "github.com/charmbracelet/bubbletea"
"github.com/charmbracelet/lipgloss"

// 3. Internal
"github.com/techdufus/openkanban/internal/board"
```

## Error Handling

- Custom error types: `type BoardError struct { Message string }`
- Sentinel errors: `var ErrTicketNotFound = &BoardError{...}`
- Wrap with context: `fmt.Errorf("failed to X: %w", err)`
- Graceful degradation - return defaults when safe

## Naming

- Type aliases for IDs: `type TicketID string`
- String constants: `const StatusBacklog TicketStatus = "backlog"`
- Constructor functions: `NewTicket()`, `NewPane()`
- Touch pattern: `t.Touch()` updates `UpdatedAt`
- SetAgentStatus pattern: `t.SetAgentStatus(as) bool` returns true on real change; mirrors `SetStatus` for the agent dimension and stamps `StatusChangedAt`. Never assign `t.AgentStatus =` directly outside `internal/board`.

## Testing

- Table-driven with subtests
- `wantXxx` or `expected` fields
- Include values in errors: `t.Errorf("got %v, want %v", ...)`

```go
tests := []struct {
    name     string
    input    string
    expected int
}{...}

for _, tt := range tests {
    t.Run(tt.name, func(t *testing.T) {...})
}
```

### Config isolation is mandatory

Any test that persists a project, ticket, filter, config, or status MUST isolate the config home — call `testutil.NewTestEnv(t)` or `t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())`. `config.GuardHomeWrite` PANICS if a test writes under the developer's real `~/.config/openkanban` or `~/.cache/openkanban*`. Knobs: `OPENKANBAN_CONFIG_DIR`, `XDG_CONFIG_HOME`, `OPENKANBAN_STATUS_DIR`. The `withFakeHome` pattern (fake `$HOME` + emptied overrides) also works. The guard is a no-op in production (`testing.Testing()`-gated).

## JSON Config

- Tags with omitempty: `json:"field,omitempty"`
- Pointer for nullable time: `*time.Time`
- Merge with defaults pattern for missing fields

## Anti-Patterns

- Don't use `any` - use concrete types or interfaces
- Don't ignore errors silently - at minimum log them
- Don't use init() for complex logic
- Don't export fields that should be private
