package daemonclient

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/techdufus/openkanban/internal/daemon"
)

// TestPaneView_Attach_AlreadyAttachedReturnsSentinel proves that a plain
// Attach probe of a session already held by another client surfaces as
// the ErrAlreadyAttached sentinel (the signal the UI branches on to warn
// before taking over), and that a generic failure does NOT match it.
// Traverses the real daemon rejection (session.go AttemptAttach →
// attach.go writeError "already_attached"), not a hand-rolled wire fake.
func TestPaneView_Attach_AlreadyAttachedReturnsSentinel(t *testing.T) {
	_ = startTestDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c1, err := New(ctx)
	if err != nil {
		t.Fatalf("New c1: %v", err)
	}
	defer c1.Close()

	sid, info := spawnSessionForTest(t, c1)
	defer c1.Kill(context.Background(), sid, 0)

	pv1 := NewPaneView(c1, "T-PV-AA1", sid, info)
	pv1.SetSize(80, 24)
	defer pv1.Close()
	if err := pv1.Attach(ctx); err != nil {
		t.Fatalf("pv1 Attach: %v", err)
	}

	// A second client attaches the same session → the daemon rejects with
	// "already_attached", which must surface as ErrAlreadyAttached.
	c2, err := New(ctx)
	if err != nil {
		t.Fatalf("New c2: %v", err)
	}
	defer c2.Close()

	pv2 := NewPaneView(c2, "T-PV-AA1", sid, info)
	pv2.SetSize(80, 24)
	defer pv2.Close()
	err = pv2.Attach(ctx)
	if err == nil {
		t.Fatalf("pv2 Attach: got nil, want ErrAlreadyAttached")
	}
	if !errors.Is(err, ErrAlreadyAttached) {
		t.Fatalf("pv2 Attach: got %v, want errors.Is ErrAlreadyAttached", err)
	}

	// Negative control: a generic failure (unknown session) must NOT be
	// mistaken for the already-attached sentinel.
	pvBad := NewPaneView(c2, "T-PV-AA-BAD", "no-such-session", nil)
	defer pvBad.Close()
	badErr := pvBad.Attach(ctx)
	if badErr == nil {
		t.Fatalf("pvBad Attach: got nil, want a generic error")
	}
	if errors.Is(badErr, ErrAlreadyAttached) {
		t.Errorf("pvBad Attach: matched ErrAlreadyAttached, want generic error (%v)", badErr)
	}

	// The probe must not have disturbed pv1 — that's the whole point of
	// probing before warning.
	if pv1.State() != PaneViewAttached {
		t.Errorf("pv1 State after probe: got %s want attached", pv1.State())
	}
}

// TestPaneView_Peek_DoesNotAttach proves Peek fills the local emulator
// with a one-shot snapshot WITHOUT becoming the attached client and
// WITHOUT disturbing an existing attacher. Covers B2 (daemon ships the
// snapshot, AttachedClient unchanged) and B3 (client stays Unattached
// but renders the content).
func TestPaneView_Peek_DoesNotAttach(t *testing.T) {
	_ = startTestDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c1, err := New(ctx)
	if err != nil {
		t.Fatalf("New c1: %v", err)
	}
	defer c1.Close()

	sid, info := spawnSessionForTest(t, c1)
	defer c1.Kill(context.Background(), sid, 0)

	pv1 := NewPaneView(c1, "T-PEEK", sid, info)
	pv1.SetSize(80, 24)
	defer pv1.Close()
	if err := pv1.Attach(ctx); err != nil {
		t.Fatalf("pv1 Attach: %v", err)
	}

	// Type a marker so the session's snapshot has identifiable content
	// (cat echoes it back).
	pv1.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'Z'}})
	if !waitForContent(t, pv1, "Z") {
		t.Fatalf("pv1 did not echo marker within deadline; content=%q", pv1.GetContent())
	}

	// Record who holds the attachment before the peek. (PaneView.Attach
	// dials its own conn, so this is the attach conn's client id, not
	// c1's control-conn id — what matters is it stays the same.)
	attachedBefore := attachedClientFor(t, c1, ctx, sid)
	if attachedBefore == 0 {
		t.Fatalf("AttachedClient before peek: got 0, want pv1's attach conn")
	}

	// Peeker on a second client.
	c2, err := New(ctx)
	if err != nil {
		t.Fatalf("New c2: %v", err)
	}
	defer c2.Close()

	pv2 := NewPaneView(c2, "T-PEEK", sid, info)
	pv2.SetSize(80, 24)
	defer pv2.Close()
	if err := pv2.Peek(ctx); err != nil {
		t.Fatalf("Peek: %v", err)
	}

	if pv2.State() != PaneViewUnattached {
		t.Fatalf("Peek state: got %s want unattached", pv2.State())
	}
	if !strings.Contains(pv2.GetContent(), "Z") {
		t.Errorf("Peek content: %q does not contain marker Z", pv2.GetContent())
	}

	// The peer must still own the attachment — Peek changed nothing
	// daemon-side: same attacher, still non-zero.
	attachedAfter := attachedClientFor(t, c1, ctx, sid)
	if attachedAfter != attachedBefore {
		t.Errorf("AttachedClient changed across peek: before=%d after=%d (peek must not touch attachment)",
			attachedBefore, attachedAfter)
	}
	if pv1.State() != PaneViewAttached {
		t.Errorf("pv1 State after peek: got %s want attached", pv1.State())
	}
}

// attachedClientFor returns the AttachedClient id the daemon reports for
// sid, or 0 if not found / nobody attached.
func attachedClientFor(t *testing.T, c *Client, ctx context.Context, sid string) uint16 {
	t.Helper()
	list, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, s := range list.Sessions {
		if s.SessionID == sid {
			return s.AttachedClient
		}
	}
	return 0
}

// TestPaneView_FreshEmulatorResetsScrollback is the deterministic
// (no-daemon) guard for the scrollback-doubling regression: a snapshot
// replay into a vt a prior Peek already populated must start from an
// empty ring. It drives the same primitives the Peek/attach replay use
// — initEmulatorLocked + applySnapshotChunk + freshEmulatorLocked — so
// it fails if freshEmulatorLocked stops tearing the prior emulator down.
func TestPaneView_FreshEmulatorResetsScrollback(t *testing.T) {
	pv := NewPaneView(nil, "T-SB", "sid-x", &daemon.SessionInfo{SessionID: "sid-x", Running: true})
	defer pv.Close()
	pv.SetSize(80, 24)

	pv.mu.Lock()
	pv.initEmulatorLocked()
	pv.mu.Unlock()

	// 40 rows into a 24-row grid → ~16 scroll off into the ring. This is
	// the \r\n-terminated history shape the daemon snapshot ships.
	var b strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "line-%02d\r\n", i)
	}
	payload := []byte(b.String())

	pv.applySnapshotChunk(payload)
	before := pv.ScrollbackLen()
	if before == 0 {
		t.Fatalf("precondition: expected non-empty scrollback after 40 rows, got 0")
	}

	// A second replay through freshEmulatorLocked must reset the ring
	// first, then land the same number of rows — not double them.
	pv.mu.Lock()
	pv.freshEmulatorLocked()
	pv.mu.Unlock()
	if got := pv.ScrollbackLen(); got != 0 {
		t.Fatalf("freshEmulatorLocked did not reset scrollback: got %d want 0", got)
	}
	pv.applySnapshotChunk(payload)
	if got := pv.ScrollbackLen(); got != before {
		t.Errorf("replay after fresh emulator: got %d want %d (snapshot doubled the ring)", got, before)
	}
}

// TestPaneView_PeekOnlyClose_DoesNotHang pins the decision NOT to tear
// down a Peek-only emulator from detach()/Close(): teardownEmulatorLocked
// wakes the drain goroutine via an InputPipe sentinel write that blocks
// on a snapshot-only vt (no live attach loop draining it) — the
// pre-existing teardown-hang. Close() on such a pane must therefore
// return promptly rather than wedge. If a future change reintroduces the
// teardown on the conn==nil path, this test hangs and the -timeout fires.
func TestPaneView_PeekOnlyClose_DoesNotHang(t *testing.T) {
	pv := NewPaneView(nil, "T-NOHANG", "sid-x", &daemon.SessionInfo{SessionID: "sid-x", Running: true})
	pv.SetSize(80, 24)

	// Peek-only state: emulator alive with applied snapshot, no conn.
	pv.mu.Lock()
	pv.initEmulatorLocked()
	pv.mu.Unlock()
	pv.applySnapshotChunk([]byte("hello world\r\n"))

	done := make(chan struct{})
	go func() {
		_ = pv.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close() hung on a peek-only pane — teardown reintroduced on the conn==nil path?")
	}
}

// waitForContent polls pv.View()/GetContent until it contains want or a
// short deadline elapses.
func waitForContent(t *testing.T, pv *PaneView, want string) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(pv.GetContent(), want) || strings.Contains(pv.View(), want) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
