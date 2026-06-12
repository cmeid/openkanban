package ui

import "testing"

// TestColumnHeightMathFits verifies that for any terminal height tall enough
// to comfortably fit one full ticket card plus both overflow indicators, the
// math in visibleTicketCount stays under the board area. For smaller terminals
// the MaxHeight cap on the column style is the safety net — see
// TestVisibleTicketCountFloorsToOne.
//
// The bug this guards against: terminal scrolls up and clips the top of the
// board when the rendered column overflows m.height.
func TestColumnHeightMathFits(t *testing.T) {
	// minSafeHeight is the smallest height where the math alone is enough:
	// header chrome (7) + in-column chrome (4) + 1 worst-case ticket (8) +
	// both indicators (2) = 21. We add slack for safety.
	const minSafeHeight = 22

	for _, h := range []int{minSafeHeight, 24, 30, 50, 100, 200} {
		t.Run("height", func(t *testing.T) {
			m := &Model{height: h}
			visible := m.visibleTicketCount()
			if visible < 1 {
				t.Errorf("height=%d: visibleTicketCount() = %d, want >= 1", h, visible)
			}

			// Worst-case rendered column height: every visible ticket at full
			// 8 rows + both overflow indicators showing simultaneously.
			worstCaseBody := visible*ticketHeight + indicatorReserveRows
			columnTotalHeight := worstCaseBody + columnHeaderHeight + 1

			if columnTotalHeight > m.boardAreaHeight() {
				t.Errorf("height=%d: column rendered height %d exceeds board area %d (visible=%d)",
					h, columnTotalHeight, m.boardAreaHeight(), visible)
			}
		})
	}
}

// TestVisibleTicketCountFloorsToOne documents the floor-to-1 contract for
// terminals too small to fit even one full ticket: rather than returning 0
// (which would render an empty column), we return 1 and rely on the
// MaxHeight cap in renderColumn to clip the overflow.
func TestVisibleTicketCountFloorsToOne(t *testing.T) {
	for _, h := range []int{1, 5, 12, 20} {
		m := &Model{height: h}
		if got := m.visibleTicketCount(); got < 1 {
			t.Errorf("height=%d: visibleTicketCount() = %d, want >= 1", h, got)
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

// TestVisibleTicketCountReservesIndicatorRows guards the indicator-row
// reservation: visibleTicketCount must subtract indicatorReserveRows so the
// "▲ N more" / "▼ N more" rows do not push tickets off the column.
func TestVisibleTicketCountReservesIndicatorRows(t *testing.T) {
	// Pick a height where the reservation is observable: with reservation,
	// (h - 11 - 2) / 8 fits exactly N tickets; without reservation,
	// (h - 11) / 8 would fit N+1 and overflow when both indicators show.
	// h=13+8N for N=2 → h=29: columnContentHeight=18, (18-2)/8=2, but 18/8=2 → same.
	// h=13+8N+r where r >=2: pick h=31: cch=20, (20-2)/8=2, but 20/8=2 → same.
	// h=33: cch=22, (22-2)/8=2, 22/8=2 → same.
	// h=37: cch=26, (26-2)/8=3, 26/8=3 → same. The reservation only changes
	// the count when columnContentHeight % ticketHeight is in [0, indicatorReserveRows).
	// Pick h such that cch = 8N: h=11+8N. h=27 → cch=16, with reserve (14/8)=1,
	// without (16/8)=2. That's the diagnostic case.
	m := &Model{height: 27}
	if got := m.columnContentHeight(); got != 16 {
		t.Fatalf("columnContentHeight() = %d, want 16 (test premise broken)", got)
	}
	got := m.visibleTicketCount()
	want := 1 // (16 - 2) / 8 = 1
	if got != want {
		t.Errorf("visibleTicketCount() = %d, want %d (indicator reservation missing)", got, want)
	}
}
