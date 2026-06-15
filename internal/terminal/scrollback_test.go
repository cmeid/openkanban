package terminal

import (
	"testing"
)

func makeTestLine(s string) []Glyph {
	glyphs := make([]Glyph, len(s))
	for i, ch := range s {
		glyphs[i] = Glyph{Char: ch, Width: 1}
	}
	return glyphs
}

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

func TestScrollbackBuffer_Basic(t *testing.T) {
	sb := NewScrollbackBuffer(5)

	if sb.Len() != 0 {
		t.Errorf("new buffer should be empty, got len=%d", sb.Len())
	}

	if sb.Capacity() != 5 {
		t.Errorf("capacity should be 5, got %d", sb.Capacity())
	}

	// Push some lines
	sb.Push(makeTestLine("line1"))
	sb.Push(makeTestLine("line2"))
	sb.Push(makeTestLine("line3"))

	if sb.Len() != 3 {
		t.Errorf("expected len=3, got %d", sb.Len())
	}

	// Get lines
	if s := lineToString(sb.Get(0)); s != "line1" {
		t.Errorf("Get(0) expected 'line1', got '%s'", s)
	}
	if s := lineToString(sb.Get(1)); s != "line2" {
		t.Errorf("Get(1) expected 'line2', got '%s'", s)
	}
	if s := lineToString(sb.Get(2)); s != "line3" {
		t.Errorf("Get(2) expected 'line3', got '%s'", s)
	}
}

func TestScrollbackBuffer_Wraparound(t *testing.T) {
	sb := NewScrollbackBuffer(3)

	// Fill buffer
	sb.Push(makeTestLine("line1"))
	sb.Push(makeTestLine("line2"))
	sb.Push(makeTestLine("line3"))

	if sb.Len() != 3 {
		t.Errorf("expected len=3, got %d", sb.Len())
	}

	// Overflow - should evict oldest
	sb.Push(makeTestLine("line4"))

	if sb.Len() != 3 {
		t.Errorf("after overflow, expected len=3, got %d", sb.Len())
	}

	// Verify oldest line is now line2
	if s := lineToString(sb.Get(0)); s != "line2" {
		t.Errorf("after overflow, Get(0) expected 'line2', got '%s'", s)
	}
	if s := lineToString(sb.Get(1)); s != "line3" {
		t.Errorf("after overflow, Get(1) expected 'line3', got '%s'", s)
	}
	if s := lineToString(sb.Get(2)); s != "line4" {
		t.Errorf("after overflow, Get(2) expected 'line4', got '%s'", s)
	}

	// Push more
	sb.Push(makeTestLine("line5"))
	sb.Push(makeTestLine("line6"))

	if s := lineToString(sb.Get(0)); s != "line4" {
		t.Errorf("Get(0) expected 'line4', got '%s'", s)
	}
	if s := lineToString(sb.Get(2)); s != "line6" {
		t.Errorf("Get(2) expected 'line6', got '%s'", s)
	}
}

func TestScrollbackBuffer_GetRange(t *testing.T) {
	sb := NewScrollbackBuffer(10)

	for i := 1; i <= 5; i++ {
		sb.Push(makeTestLine("line" + string(rune('0'+i))))
	}

	// Get range [1, 4)
	lines := sb.GetRange(1, 4)
	if len(lines) != 3 {
		t.Errorf("expected 3 lines, got %d", len(lines))
	}

	expected := []string{"line2", "line3", "line4"}
	for i, exp := range expected {
		if s := lineToString(lines[i]); s != exp {
			t.Errorf("GetRange[%d] expected '%s', got '%s'", i, exp, s)
		}
	}
}

func TestScrollbackBuffer_GetOutOfBounds(t *testing.T) {
	sb := NewScrollbackBuffer(5)
	sb.Push(makeTestLine("line1"))

	if sb.Get(-1) != nil {
		t.Error("Get(-1) should return nil")
	}
	if sb.Get(1) != nil {
		t.Error("Get(1) should return nil when only 1 line exists")
	}
	if sb.Get(100) != nil {
		t.Error("Get(100) should return nil")
	}
}

func TestScrollbackBuffer_Clear(t *testing.T) {
	sb := NewScrollbackBuffer(5)
	sb.Push(makeTestLine("line1"))
	sb.Push(makeTestLine("line2"))

	sb.Clear()

	if sb.Len() != 0 {
		t.Errorf("after Clear, expected len=0, got %d", sb.Len())
	}
	if sb.Get(0) != nil {
		t.Error("after Clear, Get(0) should return nil")
	}
}

func TestScrollbackBuffer_DefaultCapacity(t *testing.T) {
	sb := NewScrollbackBuffer(0)
	if sb.Capacity() != 10000 {
		t.Errorf("expected default capacity 10000, got %d", sb.Capacity())
	}

	sb2 := NewScrollbackBuffer(-5)
	if sb2.Capacity() != 10000 {
		t.Errorf("expected default capacity 10000 for negative, got %d", sb2.Capacity())
	}
}

func TestPane_SnapshotScrollback_Empty(t *testing.T) {
	// Fresh pane with no started PTY: scrollback is nil.
	p := New("snap-empty", 80, 24, 1000)
	if got := p.SnapshotScrollback(); got != nil {
		t.Fatalf("fresh pane SnapshotScrollback: got %d rows, want nil", len(got))
	}
}

func TestPane_SnapshotScrollback_AfterScrollOff(t *testing.T) {
	// Simulate scrollback population by directly constructing the
	// buffer; this exercises SnapshotScrollback without depending on
	// the full PTY/vt write path.
	p := New("snap-filled", 80, 24, 1000)
	p.scrollback = NewScrollbackBuffer(1000)
	for i := 0; i < 5; i++ {
		p.scrollback.Push(makeTestLine("line" + string(rune('0'+i))))
	}

	rows := p.SnapshotScrollback()
	if len(rows) != 5 {
		t.Fatalf("SnapshotScrollback: got %d rows, want 5", len(rows))
	}
	for i, want := range []string{"line0", "line1", "line2", "line3", "line4"} {
		if got := lineToString(rows[i]); got != want {
			t.Errorf("row %d: got %q, want %q", i, got, want)
		}
	}
}
