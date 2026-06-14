package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/daemonclient"
	"github.com/techdufus/openkanban/internal/project"
)

// TestAgentChromeHeight pins the contract used by handleAgentViewMouse
// to translate host-terminal mouse Y coords into pane-relative coords.
// Off-by-one bugs in selection / forwarded mouse events trace back to
// this mapping being wrong.
func TestAgentChromeHeight(t *testing.T) {
	cases := []struct {
		name    string
		hasDeps bool
		want    int
	}{
		{"no deps line, header only", false, 1},
		{"deps line present", true, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentChromeHeight(tc.hasDeps); got != tc.want {
				t.Errorf("agentChromeHeight(%v) = %d, want %d", tc.hasDeps, got, tc.want)
			}
		})
	}
}

// TestAgentPaneRowsInvariant pins the height-harmonization contract:
// for any reasonable host-terminal height, the pane rows allocated to
// the embedded session plus the chrome rows above it must exactly equal
// the host height. Off-by-one here causes claude's input line to anchor
// one row above (or below) the actual host bottom, producing the
// "wandering input" symptom from the ticket brief.
func TestAgentPaneRowsInvariant(t *testing.T) {
	heights := []int{4, 10, 24, 30, 50, 100, 200}
	for _, h := range heights {
		for _, hasDeps := range []bool{false, true} {
			rows := agentPaneRows(h, hasDeps)
			chrome := agentChromeHeight(hasDeps)
			if rows+chrome != h {
				t.Errorf("h=%d hasDeps=%v: pane(%d) + chrome(%d) = %d, want %d",
					h, hasDeps, rows, chrome, rows+chrome, h)
			}
		}
	}
}

// TestAgentPaneRowsFloorsToOne guards the minimum: even on terminals
// so short the chrome alone would consume the whole height, the pane
// must claim at least one row so the emulator has somewhere to draw.
// The chrome is the part that gets clipped by the host, not the pane.
func TestAgentPaneRowsFloorsToOne(t *testing.T) {
	cases := []struct {
		h       int
		hasDeps bool
	}{
		{0, false}, {1, false}, {1, true},
		{2, true}, {0, true},
		{-5, false},
	}
	for _, tc := range cases {
		if got := agentPaneRows(tc.h, tc.hasDeps); got < 1 {
			t.Errorf("agentPaneRows(%d, %v) = %d, want >= 1", tc.h, tc.hasDeps, got)
		}
	}
}

// chromeTestModel builds a minimal Model with enough wiring for the
// agent-view chrome to render. Returns the model and the ticket whose
// pane is focused; tests mutate the ticket (title, badges, deps) before
// calling renderAgentChrome.
func chromeTestModel(t *testing.T, width int) (*Model, *board.Ticket) {
	t.Helper()
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	proj := &project.Project{ID: "test-proj", Name: "manifold-frontend", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	ticket := &board.Ticket{
		ID:        "ticket-1",
		Title:     "do the thing",
		ProjectID: proj.ID,
		AgentType: "claude",
		Status:    board.StatusInProgress,
	}
	if err := globalStore.Add(ticket); err != nil {
		t.Fatalf("Add ticket: %v", err)
	}

	pv := daemonclient.NewPaneView(nil, string(ticket.ID), "session-1", nil)

	m := &Model{
		width:       width,
		height:      24,
		colors:      newUIColors(config.GetTheme("", nil)),
		globalStore: globalStore,
		panes:       map[board.TicketID]*daemonclient.PaneView{ticket.ID: pv},
		focusedPane: ticket.ID,
		mode:        ModeAgentView,
	}
	return m, ticket
}

// TestRenderAgentChromeRowCount pins the contract that the chrome
// rendered above the pane is always exactly agentChromeHeight rows,
// regardless of how exotic the input is. Each chrome row ends with \n
// (separating it from the next row, or from pane.View()); counting
// \n in the chrome string gives the row count.
//
// This is the structural invariant that makes the height-harmonization
// math work: if chrome ever silently grows to 2 or 3 rows (because the
// header wrapped, or the deps line wrapped), the pane is mispositioned
// and claude's input wanders.
func TestRenderAgentChromeRowCount(t *testing.T) {
	cases := []struct {
		name    string
		width   int
		title   string
		deps    []*board.Ticket
		blocks  []*board.Ticket
		wantRow int
	}{
		{
			name:    "short title, no deps",
			width:   80,
			title:   "do the thing",
			wantRow: 1,
		},
		{
			name:    "very long title forces header to clip rather than wrap",
			width:   60,
			title:   strings.Repeat("really long title ", 6),
			wantRow: 1,
		},
		{
			name:    "narrow terminal",
			width:   20,
			title:   "moderate title here",
			wantRow: 1,
		},
		{
			name:    "title plus single dep",
			width:   80,
			title:   "do the thing",
			deps:    []*board.Ticket{{ID: "dep-1", Title: "blocking ticket", ProjectID: "test-proj"}},
			wantRow: 2,
		},
		{
			name:  "many long deps must clip, not wrap",
			width: 50,
			title: "do the thing",
			deps: []*board.Ticket{
				{ID: "dep-1", Title: strings.Repeat("blocker one ", 3), ProjectID: "test-proj"},
				{ID: "dep-2", Title: strings.Repeat("blocker two ", 3), ProjectID: "test-proj"},
				{ID: "dep-3", Title: strings.Repeat("blocker three ", 3), ProjectID: "test-proj"},
			},
			blocks: []*board.Ticket{
				{ID: "blk-1", Title: strings.Repeat("blocked one ", 3), ProjectID: "test-proj"},
			},
			wantRow: 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, ticket := chromeTestModel(t, tc.width)
			ticket.Title = tc.title

			for _, dep := range tc.deps {
				if err := m.globalStore.Add(dep); err != nil {
					t.Fatalf("Add dep: %v", err)
				}
				ticket.BlockedBy = append(ticket.BlockedBy, dep.ID)
			}
			for _, blk := range tc.blocks {
				blk.BlockedBy = append(blk.BlockedBy, ticket.ID)
				if err := m.globalStore.Add(blk); err != nil {
					t.Fatalf("Add blk: %v", err)
				}
			}

			pane := m.panes[ticket.ID]
			chrome := m.renderAgentChrome(pane)

			gotRows := strings.Count(chrome, "\n")
			if gotRows != tc.wantRow {
				t.Errorf("chrome row count = %d, want %d\nchrome:\n%s",
					gotRows, tc.wantRow, chrome)
			}

			// Each row's display width must fit in m.width so the host
			// terminal doesn't wrap it.
			for i, row := range strings.Split(strings.TrimRight(chrome, "\n"), "\n") {
				if w := lipgloss.Width(row); w > tc.width {
					t.Errorf("chrome row %d: width %d > %d\nrow: %q",
						i, w, tc.width, row)
				}
			}
		})
	}
}

// TestWindowResizeSizesPaneByChrome verifies that when the host
// terminal reports a new size, the focused pane is sized so that
// chrome + pane == host height — the core height-harmonization
// contract. Previously the code used a hardcoded `m.height - 2`,
// which over-reserved 1 row for ticket with no deps and produced a
// blank gap at the bottom of the terminal where claude's input row
// was supposed to be.
func TestWindowResizeSizesPaneByChrome(t *testing.T) {
	cases := []struct {
		name        string
		hasDeps     bool
		wantPaneRow int // for termHeight=30
	}{
		{"no deps → 1 chrome row → pane = 29", false, 29},
		{"has deps → 2 chrome rows → pane = 28", true, 28},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, ticket := chromeTestModel(t, 80)
			if tc.hasDeps {
				blocker := &board.Ticket{ID: "blocker", Title: "blocker", ProjectID: ticket.ProjectID}
				if err := m.globalStore.Add(blocker); err != nil {
					t.Fatalf("Add blocker: %v", err)
				}
				ticket.BlockedBy = []board.TicketID{blocker.ID}
			}

			_, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 30})

			pane := m.panes[ticket.ID]
			gotCols, gotRows := pane.Size()
			if gotCols != 80 {
				t.Errorf("pane cols = %d, want 80", gotCols)
			}
			if gotRows != tc.wantPaneRow {
				t.Errorf("pane rows = %d, want %d (chrome=%d)",
					gotRows, tc.wantPaneRow, agentChromeHeight(tc.hasDeps))
			}
		})
	}
}
