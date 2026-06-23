package ui

import (
	"regexp"
	"strconv"
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

// TestRenderHeaderActivityChipCountsAllOpenSessions pins the fix for the chip
// undercounting open sessions. The chip total must equal the number of open
// sessions (panes), with EVERY status bucketed in the breakdown — regardless of
// (a) a stale pane.Running()==false on an unattached pane and (b) statuses the
// old switch didn't credit (error/none/stuck/completed). Reverting either part
// of the renderHeader fix turns this red.
func TestRenderHeaderActivityChipCountsAllOpenSessions(t *testing.T) {
	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	cases := []struct {
		id      string
		status  board.AgentStatus
		running bool // pane.Running(); false simulates a stale unattached pane
	}{
		{"w1", board.AgentWorking, true},
		{"w2", board.AgentWorking, false}, // stale Running — previously dropped
		{"wait1", board.AgentWaiting, true},
		{"idle1", board.AgentIdle, true},
		{"err1", board.AgentError, true},      // previously uncredited by the switch
		{"stuck1", board.AgentStuck, true},    // previously uncredited by the switch
		{"done1", board.AgentCompleted, true}, // previously uncredited by the switch
		{"none1", board.AgentNone, true},      // spawned, no status yet -> "starting"
	}

	panes := map[board.TicketID]*daemonclient.PaneView{}
	for _, c := range cases {
		tid := board.TicketID(c.id)
		ticket := &board.Ticket{
			ID: tid, Title: c.id, ProjectID: "test",
			Status: board.StatusInProgress, AgentStatus: c.status,
		}
		if err := globalStore.Add(ticket); err != nil {
			t.Fatalf("Add %s: %v", c.id, err)
		}
		info := &daemon.SessionInfo{SessionID: "sid-" + c.id, TicketID: c.id, Running: c.running, Cols: 80, Rows: 24}
		panes[tid] = daemonclient.NewPaneView(nil, c.id, info.SessionID, info)
	}

	m := &Model{
		globalStore: globalStore,
		panes:       panes,
		spinner:     spinner.New(spinner.WithSpinner(spinner.Dot)),
		colors:      newUIColors(config.DefaultConfig().GetTheme()),
		width:       200,
		height:      40,
		config:      &config.Config{Agents: map[string]config.AgentConfig{}},
	}

	out := ansi.Strip(m.renderHeader())

	// Total equals open-session count (8 panes), independent of status / stale Running.
	if !strings.Contains(out, "8 sessions") {
		t.Errorf("chip total: want %q (one per open pane); header was:\n%s", "8 sessions", out)
	}
	// Every non-zero bucket surfaces — including stuck/done, the switch branches
	// the original fixture never exercised.
	for _, want := range []string{"2 working", "1 waiting", "1 idle", "1 error", "1 stuck", "1 done", "1 starting"} {
		if !strings.Contains(out, want) {
			t.Errorf("breakdown missing %q; header was:\n%s", want, out)
		}
	}

	// Structural invariant: the breakdown counts must SUM to the announced total —
	// pins "every status buckets, nothing double-counts or leaks" rather than just
	// the specific statuses in this fixture. Parse the chip line.
	var chipLine string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "sessions") {
			chipLine = line
			break
		}
	}
	if chipLine == "" {
		t.Fatalf("chip line not found; header was:\n%s", out)
	}
	total := mustAtoi(t, regexp.MustCompile(`(\d+) sessions`).FindStringSubmatch(chipLine), "total")
	sum := 0
	for _, m := range regexp.MustCompile(`(\d+) (?:working|waiting|idle|starting|stuck|error|done)`).FindAllStringSubmatch(chipLine, -1) {
		sum += mustAtoi(t, m, "bucket")
	}
	if sum != total {
		t.Errorf("breakdown sum %d != announced total %d; chip line: %q", sum, total, chipLine)
	}
	if total != len(cases) {
		t.Errorf("announced total %d != open panes %d; chip line: %q", total, len(cases), chipLine)
	}
}

func mustAtoi(t *testing.T, match []string, what string) int {
	t.Helper()
	if len(match) < 2 {
		t.Fatalf("could not parse %s from chip", what)
	}
	n, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatalf("parse %s %q: %v", what, match[1], err)
	}
	return n
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
		{"subagents", board.AgentSubagents, "⊟", "sub-agents"},
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

// TestRenderHeaderSubagentsChip pins the header activity chip for the
// sub-agents status: a session awaiting background sub-agents shows the calm
// "⊟ N sub-agents" chip (NOT the orange "waiting"), and a genuine needs-you
// "waiting" session still wins the single chip when both are present.
func TestRenderHeaderSubagentsChip(t *testing.T) {
	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	panes := map[board.TicketID]*daemonclient.PaneView{}
	add := func(id board.TicketID, st board.AgentStatus) {
		ticket := &board.Ticket{ID: id, Title: string(id), ProjectID: "test", Status: board.StatusInProgress, AgentStatus: st}
		if err := globalStore.Add(ticket); err != nil {
			t.Fatalf("Add ticket %s: %v", id, err)
		}
		info := &daemon.SessionInfo{SessionID: "sid-" + string(id), TicketID: string(id), Running: true, Cols: 80, Rows: 24}
		panes[id] = daemonclient.NewPaneView(nil, string(id), info.SessionID, info)
	}

	newM := func() *Model {
		return &Model{
			globalStore: globalStore,
			panes:       panes,
			spinner:     spinner.New(spinner.WithSpinner(spinner.Dot)),
			colors:      newUIColors(config.DefaultConfig().GetTheme()),
			width:       120,
			height:      40,
			config:      &config.Config{Agents: map[string]config.AgentConfig{}},
		}
	}

	// Only a sub-agents session → calm "⊟" leading icon + a "sub-agents"
	// breakdown bucket, and crucially NOT classified as "waiting".
	add("sub-1", board.AgentSubagents)
	out := ansi.Strip(newM().renderHeader())
	if !strings.Contains(out, "⊟") {
		t.Errorf("header missing sub-agents ⊟ icon; got:\n%s", out)
	}
	if !strings.Contains(out, "1 sub-agents") {
		t.Errorf("header missing sub-agents breakdown bucket; got:\n%s", out)
	}
	if strings.Contains(out, "waiting") {
		t.Errorf("sub-agents session must not render as waiting; got:\n%s", out)
	}

	// Add a genuine needs-you waiting session → waiting wins the leading
	// icon (◐), but the sub-agents session must still be bucketed in the
	// breakdown (not dropped — the chip total must account for every pane).
	add("wait-1", board.AgentWaiting)
	out = ansi.Strip(newM().renderHeader())
	if !strings.Contains(out, "◐") {
		t.Errorf("waiting must win the leading icon when present; got:\n%s", out)
	}
	if !strings.Contains(out, "1 waiting") || !strings.Contains(out, "1 sub-agents") {
		t.Errorf("breakdown must list both waiting and sub-agents; got:\n%s", out)
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
