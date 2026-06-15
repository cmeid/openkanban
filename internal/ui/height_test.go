package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/lipgloss"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/daemonclient"
	"github.com/techdufus/openkanban/internal/project"
)

// newHeightTestModel wires a minimum-viable Model sufficient to call
// renderTicket / renderColumn without panicking. Mirrors the pattern in
// priority_sort_test.go:makePrioritySortModel; broken out so the render-
// time height tests below don't drag in priority-sort plumbing.
func newHeightTestModel(t *testing.T, width, height int) (*Model, *project.Project) {
	t.Helper()
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	cols := board.DefaultColumns()
	m := &Model{
		globalStore:    globalStore,
		panes:          map[board.TicketID]*daemonclient.PaneView{},
		daemonAttached: map[board.TicketID]int{},
		columns:        cols,
		columnTickets:  make([][]*board.Ticket, len(cols)),
		columnOffsets:  make([]int, len(cols)),
		spinner:        sp,
		width:          width,
		height:         height,
		config:         &config.Config{Agents: map[string]config.AgentConfig{}},
		colors:         newUIColors(config.DefaultConfig().GetTheme()),
	}
	return m, proj
}

func makeTicket(proj *project.Project, title, desc string, labels []string) *board.Ticket {
	return &board.Ticket{
		ID:          board.NewTicketID(),
		Title:       title,
		Description: desc,
		Status:      board.StatusBacklog,
		Priority:    3,
		Labels:      labels,
		AgentStatus: board.AgentIdle,
		ProjectID:   proj.ID,
	}
}

// TestRenderTicketShortTitleHeight pins the baseline card height for a
// ticket whose title fits in a single row: 8 rows (2 border + title +
// header line + desc + status + labels + 1 bottom margin).
func TestRenderTicketShortTitleHeight(t *testing.T) {
	m, proj := newHeightTestModel(t, 120, 40)
	tk := makeTicket(proj, "short title", "a brief description", []string{"bug"})

	view := m.renderTicket(tk, false, false, 36, m.colors.primary)

	if got := lipgloss.Height(view); got != 8 {
		t.Errorf("short-title card height = %d, want 8\n---\n%s\n---", got, view)
	}
}

// TestRenderTicketLongTitleHeight pins the height for a ticket whose
// title is long enough to wrap to 2 rows: 9 rows (baseline + 1 extra
// title row). Verifies that the width on titleStyle matches the card's
// effective content width after cardStyle.Padding(0,1) — without the
// width fix the outer Padding re-wraps the already-wrapped title and
// the card balloons to 10-11 rows.
func TestRenderTicketLongTitleHeight(t *testing.T) {
	m, proj := newHeightTestModel(t, 120, 40)
	tk := makeTicket(proj, strings.Repeat("verylongword ", 12), "a brief description", []string{"bug"})

	view := m.renderTicket(tk, false, false, 36, m.colors.primary)

	if got := lipgloss.Height(view); got != 9 {
		t.Errorf("long-title card height = %d, want 9\n---\n%s\n---", got, view)
	}
}

// TestRenderColumnPacksMixedHeights renders a column containing a mix of
// short-title and long-title tickets and asserts:
//
//  1. The rendered column height never exceeds boardAreaHeight().
//  2. The per-ticket heights cache (m.columnTicketHeights) is populated
//     with one entry per ticket in the column.
func TestRenderColumnPacksMixedHeights(t *testing.T) {
	m, proj := newHeightTestModel(t, 120, 40)

	tickets := []*board.Ticket{
		makeTicket(proj, "short A", "d", []string{"x"}),
		makeTicket(proj, strings.Repeat("longword ", 10), "d", []string{"x"}),
		makeTicket(proj, "short B", "d", []string{"x"}),
		makeTicket(proj, strings.Repeat("longword ", 10), "d", []string{"x"}),
		makeTicket(proj, "short C", "d", []string{"x"}),
		makeTicket(proj, "short D", "d", []string{"x"}),
		makeTicket(proj, "short E", "d", []string{"x"}),
		makeTicket(proj, "short F", "d", []string{"x"}),
	}
	for _, tk := range tickets {
		if err := m.globalStore.Add(tk); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	m.refreshColumnTickets()

	colWidth := 40
	rendered := m.renderColumn(0, m.columns[0], m.columnTickets[0], true, false, false, colWidth, false, 0)

	if got, max := lipgloss.Height(rendered), m.boardAreaHeight(); got > max {
		t.Errorf("rendered column height = %d, want <= boardAreaHeight=%d", got, max)
	}

	if len(m.columnTicketHeights[0]) != len(m.columnTickets[0]) {
		t.Errorf("columnTicketHeights[0] has %d entries, want %d",
			len(m.columnTicketHeights[0]), len(m.columnTickets[0]))
	}
	// Sanity: every cached height should be > 0 (a rendered card always
	// has at least a top + bottom border row).
	for i, h := range m.columnTicketHeights[0] {
		if h <= 0 {
			t.Errorf("columnTicketHeights[0][%d] = %d, want > 0", i, h)
		}
	}
}

// TestBoardAreaHeightMatchesColumnContentHeight ensures the two helpers
// remain consistent: columnContentHeight is boardAreaHeight minus the
// in-column chrome (header rows + bottom border).
func TestBoardAreaHeightMatchesColumnContentHeight(t *testing.T) {
	for _, h := range []int{20, 24, 30, 50, 100} {
		m := &Model{height: h}
		got := m.columnContentHeight()
		want := m.boardAreaHeight() - columnHeaderHeight - 1
		if got != want {
			t.Errorf("height=%d: columnContentHeight()=%d, want %d", h, got, want)
		}
	}
}

// TestSidebarMatchesBoardArea verifies that when the sidebar is visible,
// the composed View() output fits within m.height. Previously, the
// sidebar sized itself to m.height - headerHeight() - 1 (statusBar
// only), while boardAreaHeight() reserves an additional 2 rows for the
// newlines bracketing the status bar. JoinHorizontal(Top, sidebar,
// board) then produced a row 2 lines taller than the board area,
// pushing total View() output past m.height.
//
// Asserts on the composed View() height rather than the sidebar string
// in isolation: lipgloss.Style.Height(n) is a *minimum*, not an exact
// target (see lipgloss/align.go:62-83), so the in-isolation height of
// the sidebar after Height() is not a reliable invariant. The actual
// invariant we care about is that the full View() fits the terminal.
func TestSidebarMatchesBoardArea(t *testing.T) {
	m, proj := newHeightTestModel(t, 120, 30)
	m.sidebarVisible = true

	// Populate at least one ticket so View() renders the board content
	// rather than the empty-state placeholder.
	if err := m.globalStore.Add(makeTicket(proj, "a ticket", "desc", []string{"bug"})); err != nil {
		t.Fatalf("Add: %v", err)
	}
	m.refreshColumnTickets()

	view := m.View()
	if got := lipgloss.Height(view); got > m.height {
		t.Errorf("View() height = %d, want <= m.height=%d\n---\n%s\n---", got, m.height, view)
	}
}

// TestColumnHeaderLineSingleRow verifies that a deliberately long column
// name does not wrap the column's header line to 2 rows at narrow widths.
// Without the headerLine clamp in renderColumn, "▸ 📋 In Progress Tickets
// Awaiting Review (1)" wraps at minColumnWidth (effective content width
// = width - 2 = 18) and breaks the columnHeaderHeight = 3 invariant
// (top border + header + blank separator).
//
// The assertion compares the long-name column's rendered height to a
// short-name baseline rendered with the same number of tickets at the
// same width; if the long header wraps, the long-name column is exactly
// one row taller. With the clamp applied they match.
func TestColumnHeaderLineSingleRow(t *testing.T) {
	m, proj := newHeightTestModel(t, 120, 40)

	// One ticket per column so we exercise the normal (non-empty-state)
	// rendering path that places headerLine above the joined tickets.
	tk := makeTicket(proj, "short", "d", []string{"x"})
	if err := m.globalStore.Add(tk); err != nil {
		t.Fatalf("Add: %v", err)
	}
	m.refreshColumnTickets()

	shortCol := m.columns[0]
	shortCol.Name = "Todo"
	longCol := m.columns[0]
	longCol.Name = "In Progress Tickets Awaiting Review"

	short := m.renderColumn(0, shortCol, m.columnTickets[0], false, false, false, minColumnWidth, true, 0)
	long := m.renderColumn(0, longCol, m.columnTickets[0], false, false, false, minColumnWidth, true, 0)

	if got, want := lipgloss.Height(long), lipgloss.Height(short); got != want {
		t.Errorf("long-name column height = %d, short-name column height = %d; "+
			"headers should both occupy 1 row\n---long---\n%s\n---short---\n%s\n---",
			got, want, long, short)
	}
}

// TestCardLineClampStaysSingleLine pins the rendering pattern used by
// renderTicket to clamp every non-title card line (description, badge
// rows) to exactly one row. Without this, long descriptions wrap and
// push the card past its measured height.
func TestCardLineClampStaysSingleLine(t *testing.T) {
	const width = 20
	cases := []struct {
		name  string
		input string
	}{
		{"short fits", "hello"},
		{"exact width", strings.Repeat("a", width)},
		{"long wraps without clamp", strings.Repeat("a", width*4)},
		{"unicode long", strings.Repeat("日本語", width)},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := lipgloss.NewStyle().Width(width).MaxHeight(1).Render(tc.input)
			if got := lipgloss.Height(out); got != 1 {
				t.Errorf("Width(%d).MaxHeight(1).Render(%q) height = %d, want 1", width, tc.input, got)
			}
		})
	}
}
