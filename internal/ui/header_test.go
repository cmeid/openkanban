package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/x/ansi"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/daemonclient"
	"github.com/techdufus/openkanban/internal/project"
)

// TestRenderHeaderActivityChipClearsCorner pins the placement fix: the
// board header's working/waiting activity chip must sit clear of the
// top-right corner where macOS notification banners land. We assert the
// chip text is NOT in the rightmost columns (the banner zone) while the
// disposable "? help q quit" text keeps the corner. With the original
// flush-right "  " separator the chip lands ~col 104, inside the last 25
// columns, so reverting the renderHeader edit turns this test red.
func TestRenderHeaderActivityChipClearsCorner(t *testing.T) {
	const tid board.TicketID = "chip-1"

	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	ticket := &board.Ticket{
		ID:          tid,
		Title:       "Waiting agent",
		ProjectID:   "test",
		Status:      board.StatusInProgress,
		AgentStatus: board.AgentWaiting,
	}
	if err := globalStore.Add(ticket); err != nil {
		t.Fatalf("Add ticket: %v", err)
	}

	// info.Running=true flips the PaneView to Unattached so pane.Running()
	// is true without a real attach (see cycle_session_test.go).
	info := &daemon.SessionInfo{SessionID: "sid-" + string(tid), TicketID: string(tid), Running: true, Cols: 80, Rows: 24}

	m := &Model{
		globalStore: globalStore,
		panes:       map[board.TicketID]*daemonclient.PaneView{tid: daemonclient.NewPaneView(nil, string(tid), info.SessionID, info)},
		spinner:     spinner.New(spinner.WithSpinner(spinner.Dot)),
		colors:      newUIColors(config.DefaultConfig().GetTheme()),
		width:       120,
		height:      40,
		config:      &config.Config{Agents: map[string]config.AgentConfig{}},
	}

	out := ansi.Strip(m.renderHeader())

	var chipLine string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "waiting") {
			chipLine = line
			break
		}
	}
	if chipLine == "" {
		t.Fatalf("activity chip not rendered; header was:\n%s", out)
	}
	if !strings.Contains(chipLine, "? help") {
		t.Errorf("help text missing from header line: %q", chipLine)
	}

	// The chip must clear the notification banner zone (rightmost 25 cols).
	end := strings.Index(chipLine, "waiting") + len("waiting")
	if limit := m.width - 25; end > limit {
		t.Errorf("activity chip ends at col %d, want <= %d (clear of the top-right notification zone)\nline: %q", end, limit, chipLine)
	}
}

func TestAgentStatusGlyph(t *testing.T) {
	tests := []struct {
		name     string
		status   board.AgentStatus
		wantIcon string
		wantLbl  string
	}{
		{"working", board.AgentWorking, "●", "working"},
		{"waiting", board.AgentWaiting, "◐", "waiting"},
		{"idle", board.AgentIdle, "◆", "idle"},
		{"completed", board.AgentCompleted, "✓", "done"},
		{"error", board.AgentError, "✗", "error"},
		{"none -> empty", board.AgentNone, "", ""},
		{"unknown -> empty", board.AgentStatus("garbage"), "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			icon, label := agentStatusGlyph(tt.status)
			if icon != tt.wantIcon {
				t.Errorf("icon: got %q, want %q", icon, tt.wantIcon)
			}
			if label != tt.wantLbl {
				t.Errorf("label: got %q, want %q", label, tt.wantLbl)
			}
		})
	}
}

func TestPriorityGlyph(t *testing.T) {
	tests := []struct {
		priority int
		want     string
	}{
		{1, "⌃⌃"},
		{2, "⌃⎯"},
		{3, "⎯⎯"},
		{4, "⎯⌄"},
		{5, "⌄⌄"},
		{0, ""},
		{6, ""},
		{-1, ""},
	}
	for _, tt := range tests {
		got := priorityGlyph(tt.priority)
		if got != tt.want {
			t.Errorf("priorityGlyph(%d): got %q, want %q", tt.priority, got, tt.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		name string
		secs int
		want string
	}{
		{"0 seconds", 0, "0s"},
		{"45 seconds", 45, "45s"},
		{"1 minute", 60, "1m"},
		{"5 minutes", 5 * 60, "5m"},
		{"59 minutes", 59 * 60, "59m"},
		{"1 hour exact", 3600, "1h"},
		{"2h 15m", 2*3600 + 15*60, "2h15m"},
		{"3h 0m", 3 * 3600, "3h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := time.Duration(tt.secs) * time.Second
			got := formatDuration(d)
			if got != tt.want {
				t.Errorf("formatDuration(%v): got %q, want %q", d, got, tt.want)
			}
		})
	}
}
