package terminal

import (
	"fmt"
	"strings"
	"testing"

	xvt "github.com/charmbracelet/x/vt"
)

func lineToString(line []Glyph) string {
	if line == nil {
		return ""
	}
	runes := make([]rune, len(line))
	for i, g := range line {
		runes[i] = g.Char
	}
	return string(runes)
}


func TestPane_SnapshotScrollback_Empty(t *testing.T) {
	// Fresh pane with no started PTY: scrollback is nil.
	p := New("snap-empty", 80, 24, 1000)
	if got := p.SnapshotScrollback(); got != nil {
		t.Fatalf("fresh pane SnapshotScrollback: got %d rows, want nil", len(got))
	}
}

func TestPane_SnapshotScrollback_AfterScrollOff(t *testing.T) {
	// Drive real bytes through handleOutput so the snapshot reflects
	// what actually scrolled off the emulator grid — the same path the
	// daemon ships to attaching clients. Five lines on a 3-row grid
	// scroll the first two into history.
	p := New("snap-filled", 80, 3, 1000)
	p.vt = xvt.NewSafeEmulator(80, 3)
	p.handleOutput([]byte("line0\r\nline1\r\nline2\r\nline3\r\nline4"))

	rows := p.SnapshotScrollback()
	if len(rows) != 2 {
		t.Fatalf("SnapshotScrollback: got %d rows, want 2", len(rows))
	}
	for i, want := range []string{"line0", "line1"} {
		if got := strings.TrimRight(lineToString(rows[i]), " \x00"); got != want {
			t.Errorf("row %d: got %q, want %q", i, got, want)
		}
	}
}

// TestPane_SnapshotScrollback_MultiRowScrollOff is the regression guard
// for the detached-session scrollback bug: a single handleOutput write
// that scrolls many rows off the grid must put ALL of them into the
// shipped snapshot. The legacy CaptureTopRow/PushScrolledLine ring
// captured only the single pre-write row 0 per write, so a burst of
// output (e.g. a long agent message rendered while detached, then
// drained in one chunk on re-attach) lost all but one scrolled-off row —
// scroll-up after re-attach showed gaps. The emulator's own native
// scrollback wraps and tracks every row, so the snapshot reads from it.
func TestPane_SnapshotScrollback_MultiRowScrollOff(t *testing.T) {
	const rows = 4
	p := New("snap-burst", 20, rows, 1000)
	p.vt = xvt.NewSafeEmulator(20, rows)

	// 12 lines onto a 4-row grid in ONE write: L01..L08 scroll off,
	// L09..L12 remain on the grid. No trailing newline after L12.
	var b strings.Builder
	for i := 1; i <= 12; i++ {
		if i > 1 {
			b.WriteString("\r\n")
		}
		fmt.Fprintf(&b, "L%02d", i)
	}
	p.handleOutput([]byte(b.String()))

	got := p.SnapshotScrollback()
	if len(got) < 8 {
		t.Fatalf("SnapshotScrollback after 12-line single write: got %d rows, want >=8 (8 lines scrolled off the %d-row grid)", len(got), rows)
	}
	// The oldest 8 captured rows must be L01..L08 in order — proving no
	// scrolled-off row was dropped.
	for i := 0; i < 8; i++ {
		want := fmt.Sprintf("L%02d", i+1)
		if g := strings.TrimRight(lineToString(got[i]), " \x00"); g != want {
			t.Errorf("scrollback row %d: got %q, want %q", i, g, want)
		}
	}
}
