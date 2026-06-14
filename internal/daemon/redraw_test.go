package daemon

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	xvt "github.com/charmbracelet/x/vt"

	"github.com/techdufus/openkanban/internal/terminal"
)

// --- Fixtures ---
//
// Each fixture is a byte stream that is fed into a fresh
// xvt.SafeEmulator (80x24 unless otherwise noted) to set up a known
// emulator state. SerializeRedraw is then called on that emulator with
// the expected modal/title state for the fixture, and the resulting
// bytes are fed into a SECOND fresh emulator (also 80x24). The two
// emulators are then compared cell-for-cell.
//
// Generating fixtures as Go-literal byte slices keeps them easy to
// maintain — no separate testdata to wrangle.

type redrawFixture struct {
	name          string
	stream        []byte
	cursorVisible bool
	mouseEnabled  bool
	altScreen     bool
	title         string

	// wantContains is a list of substrings the emitted snapshot MUST
	// include. Used to assert mode/title escape sequences that the
	// destination emulator's getters don't expose. Each entry is a
	// human-readable label paired with the byte sequence.
	wantContains map[string]string

	// wantNotContains is the inverse: substrings that MUST NOT appear
	// (e.g. cursor-hide when cursor is visible).
	wantNotContains map[string]string
}

var redrawFixtures = []redrawFixture{
	{
		name:          "plain_sgr_80x24",
		stream:        []byte("hello \x1b[31mred\x1b[1;34mbb\x1b[0m\r\n\r\n\r\n\r\n"),
		cursorVisible: true,
	},
	{
		name:          "alt_screen_enter",
		stream:        []byte("primary\r\n\x1b[?1049hAFTER ALT\r\nLINE2"),
		cursorVisible: true,
		altScreen:     true,
		wantContains: map[string]string{
			"alt-screen enable": "\x1b[?1049h",
		},
	},
	{
		name:          "mouse_modes",
		stream:        []byte("\x1b[?1000h\x1b[?1006hclick"),
		cursorVisible: true,
		mouseEnabled:  true,
		wantContains: map[string]string{
			"mouse 1000 enable": "\x1b[?1000h",
			"mouse 1006 enable": "\x1b[?1006h",
		},
	},
	{
		name:          "cursor_hidden",
		stream:        []byte("\x1b[?25lhidden cursor"),
		cursorVisible: false,
		wantContains: map[string]string{
			"cursor hide": "\x1b[?25l",
		},
	},
	{
		name:          "title_set",
		stream:        []byte("\x1b]2;ticket title\x07hello"),
		cursorVisible: true,
		title:         "ticket title",
		wantContains: map[string]string{
			"OSC 2 prefix":      "\x1b]2;",
			"OSC 2 body":        "ticket title",
			"OSC 2 BEL trailer": "\x07",
		},
		wantNotContains: map[string]string{
			"cursor hide": "\x1b[?25l",
		},
	},
	{
		// Wide (CJK / emoji) glyphs occupy two terminal cells. The
		// snapshot must skip the continuation cell — ultraviolet stores
		// it as a zero-value Cell (Width=0) following each wide cell.
		// Emitting a space for the continuation in writeRow shifts every
		// glyph after the wide char one column right in the destination
		// emulator, surfacing as "garbled initial render on session
		// attach".
		name:          "wide_chars_round_trip",
		stream:        []byte("hello 中文 X 🙂 Y\r\n壱弐参\r\n"),
		cursorVisible: true,
	},
	{
		// Synthetic "claude-like" stream: enter alt screen, draw a
		// header line in bold, draw a status line, hide cursor, then
		// re-position the cursor mid-screen.
		name: "live_claude_capture",
		stream: bytes.Join([][]byte{
			[]byte("\x1b[?1049h"),
			[]byte("\x1b[2J"),
			[]byte("\x1b[H"),
			[]byte("\x1b[1;36mclaude > Working on task...\x1b[0m"),
			[]byte("\r\n"),
			[]byte("\x1b[33m  - read pane.go\x1b[0m"),
			[]byte("\r\n"),
			[]byte("\x1b[32m  - write redraw.go\x1b[0m"),
			[]byte("\r\n\r\n"),
			[]byte("\x1b[2;30mready for next instruction\x1b[0m"),
			[]byte("\x1b[10;5H"),
			[]byte("\x1b[?25l"),
		}, nil),
		cursorVisible: false,
		altScreen:     true,
		wantContains: map[string]string{
			"alt-screen enable": "\x1b[?1049h",
			"cursor hide":       "\x1b[?25l",
		},
	},
}

// feedEmulator returns a fresh 80x24 SafeEmulator with the byte stream
// applied. The emulator is small enough that we don't worry about
// scrollback; tests assert on the live grid only.
func feedEmulator(t *testing.T, stream []byte) *xvt.SafeEmulator {
	t.Helper()
	em := xvt.NewSafeEmulator(80, 24)
	if _, err := em.Write(stream); err != nil {
		t.Fatalf("emulator write: %v", err)
	}
	return em
}

// cellsEqual compares the live grids of two emulators row-by-row,
// returning the first divergence as a human-readable message.
// Cursor positions are compared separately by the caller.
func cellsEqual(a, b *xvt.SafeEmulator) string {
	if a.Width() != b.Width() || a.Height() != b.Height() {
		return fmt.Sprintf("dim mismatch: a=%dx%d b=%dx%d",
			a.Width(), a.Height(), b.Width(), b.Height())
	}

	cols := a.Width()
	rows := a.Height()
	for row := 0; row < rows; row++ {
		for col := 0; col < cols; col++ {
			ga := terminal.CellToGlyph(a.CellAt(col, row))
			gb := terminal.CellToGlyph(b.CellAt(col, row))
			ca := ga.Char
			if ca == 0 {
				ca = ' '
			}
			cb := gb.Char
			if cb == 0 {
				cb = ' '
			}
			if ca != cb {
				return fmt.Sprintf("char mismatch at row=%d col=%d: a=%q b=%q\nrow %d a=%q\nrow %d b=%q",
					row, col, ca, cb, row, gridRow(a, row), row, gridRow(b, row))
			}
			// Style equality: same fields as Glyph.styleEqual.
			if ga.Bold != gb.Bold || ga.Italic != gb.Italic ||
				ga.Underline != gb.Underline || ga.Reverse != gb.Reverse ||
				ga.Blink != gb.Blink ||
				ga.FG != gb.FG || ga.BG != gb.BG {
				return fmt.Sprintf("style mismatch at row=%d col=%d:\n  a=%+v\n  b=%+v",
					row, col, ga, gb)
			}
		}
	}
	return ""
}

// gridRow returns the printable characters of one row, for diagnostics.
func gridRow(em *xvt.SafeEmulator, row int) string {
	var sb strings.Builder
	for col := 0; col < em.Width(); col++ {
		ch := terminal.CellToGlyph(em.CellAt(col, row)).Char
		if ch == 0 {
			ch = ' '
		}
		sb.WriteRune(ch)
	}
	return strings.TrimRight(sb.String(), " ")
}

func TestSerializeRedraw_RoundTrip(t *testing.T) {
	for _, fx := range redrawFixtures {
		fx := fx
		t.Run(fx.name, func(t *testing.T) {
			source := feedEmulator(t, fx.stream)
			snapshot := SerializeRedraw(source, fx.cursorVisible, fx.mouseEnabled, fx.altScreen, fx.title)
			if len(snapshot) == 0 {
				t.Fatalf("empty snapshot for non-empty source emulator")
			}

			// Wantcontains / wantnotcontains assertions on the
			// snapshot bytes — the destination SafeEmulator doesn't
			// expose cursor-visibility / mouse mode / title, so we
			// verify those by examining the emitted sequence.
			for label, seq := range fx.wantContains {
				if !bytes.Contains(snapshot, []byte(seq)) {
					t.Errorf("snapshot missing %s (bytes %q)", label, seq)
				}
			}
			for label, seq := range fx.wantNotContains {
				if bytes.Contains(snapshot, []byte(seq)) {
					t.Errorf("snapshot unexpectedly contains %s (bytes %q)", label, seq)
				}
			}

			// Round-trip: feed snapshot into a fresh emulator and
			// compare grids. We do NOT replay fx.stream into the
			// destination — only the snapshot bytes — so any state
			// the redraw fails to encode shows up as a divergence.
			dest := xvt.NewSafeEmulator(80, 24)
			if _, err := dest.Write(snapshot); err != nil {
				t.Fatalf("destination emulator write: %v", err)
			}

			if msg := cellsEqual(source, dest); msg != "" {
				t.Errorf("round-trip cells diverged: %s", msg)
			}

			// Cursor position should round-trip too.
			srcCur := source.CursorPosition()
			dstCur := dest.CursorPosition()
			if srcCur.X != dstCur.X || srcCur.Y != dstCur.Y {
				t.Errorf("cursor position mismatch: src=(%d,%d) dst=(%d,%d)",
					srcCur.X, srcCur.Y, dstCur.X, dstCur.Y)
			}

			// IsAltScreen survives round-trip (SafeEmulator exposes
			// this directly — the other modes do not).
			if got := dest.IsAltScreen(); got != fx.altScreen {
				t.Errorf("dest IsAltScreen=%v, want %v", got, fx.altScreen)
			}
		})
	}
}

// TestSerializeRedraw_NilEmulator verifies the function fails closed
// rather than panicking on a nil or zero-sized emulator.
func TestSerializeRedraw_NilEmulator(t *testing.T) {
	if out := SerializeRedraw(nil, true, false, false, ""); out != nil {
		t.Errorf("nil vt: want nil, got %d bytes", len(out))
	}
}

// TestSerializeRedraw_StartsWithRIS asserts the very first bytes are
// a reset, so a destination emulator with arbitrary prior state lands
// on a clean baseline.
func TestSerializeRedraw_StartsWithRIS(t *testing.T) {
	em := feedEmulator(t, []byte("hello"))
	snap := SerializeRedraw(em, true, false, false, "")
	if len(snap) < 2 {
		t.Fatalf("snapshot too short: %d bytes", len(snap))
	}
	if snap[0] != 0x1b || snap[1] != 'c' {
		t.Errorf("snapshot does not start with RIS: got %q", snap[:min(8, len(snap))])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
