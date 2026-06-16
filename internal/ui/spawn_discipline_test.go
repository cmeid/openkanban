package ui

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/daemonclient"
	"github.com/techdufus/openkanban/internal/project"
)

// spawnDisciplineStubAPI is a daemonAPI fake purpose-built for the
// client-side spawn-discipline tests. It records Owns / Spawn
// invocation counts so tests can prove that the Owns fast-path skipped
// Spawn (B4), and lets the test set canned Owns responses. Every other
// method on daemonAPI falls through to daemonAPINoop.
type spawnDisciplineStubAPI struct {
	daemonAPINoop

	ownsCalls  atomic.Int32
	spawnCalls atomic.Int32

	ownsResp daemon.OwnsResp
	ownsErr  error
}

func (s *spawnDisciplineStubAPI) Owns(_ context.Context, _ string) (daemon.OwnsResp, error) {
	s.ownsCalls.Add(1)
	return s.ownsResp, s.ownsErr
}
func (s *spawnDisciplineStubAPI) Spawn(_ context.Context, _ daemon.SpawnReq) (daemon.SpawnResp, error) {
	s.spawnCalls.Add(1)
	return daemon.SpawnResp{SessionID: "sid-from-spawn"}, nil
}

// makeDetachedPane constructs a PaneView in PaneViewDetached state with
// the given sessionID. info=nil keeps the constructor on the no-info
// branch — state stays Detached. The daemon client is nil because the
// spawnAgent test paths under test don't need a live daemon: the only
// daemon call is m.attachExisting, which returns nil when m.daemonClient
// is nil. Verifying the early-return is enough to pin the contract.
func makeDetachedPane(sessionID string) *daemonclient.PaneView {
	return daemonclient.NewPaneView(nil, "T-DETACH", sessionID, nil)
}

// TestSpawnAgent_DetachedPaneRecoversViaAttach pins B6: when the local
// PaneView is in PaneViewDetached state but still holds a SessionID, the
// spawnAgent dispatch should attempt re-attach via the existing pane
// instead of falling through to the spawn path (which would issue a
// fresh Spawn RPC, even after idempotent-Spawn made that harmless on
// the daemon side — wasted RPC + the local pane object is recreated).
//
// The assertion shape: mode flips to ModeAgentView, focusedPane is the
// ticket's ID, m.panes still holds the SAME pane (not a freshly
// constructed one), and no spawn-spawning state (ModeSpawning,
// spawningTicketID) gets set.
func TestSpawnAgent_DetachedPaneRecoversViaAttach(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	ticket := &board.Ticket{
		ID:        "T-DETACH-1",
		Title:     "detached recovery",
		ProjectID: proj.ID,
		Status:    board.StatusInProgress,
	}
	if err := globalStore.Add(ticket); err != nil {
		t.Fatalf("Add ticket: %v", err)
	}

	pane := daemonclient.NewPaneView(nil, string(ticket.ID), "sid-daemon-owned", nil)
	if got := pane.State(); got != daemonclient.PaneViewDetached {
		t.Fatalf("test fixture: pane.State() = %v, want PaneViewDetached", got)
	}

	m := &Model{
		globalStore:   globalStore,
		panes:         map[board.TicketID]*daemonclient.PaneView{ticket.ID: pane},
		columnTickets: [][]*board.Ticket{{}, {ticket}, {}, {}},
		columnOffsets: []int{0, 0, 0, 0},
		mode:          ModeNormal,
		activeColumn:  1,
		activeTicket:  0,
		width:         120,
		height:        40,
		config:        &config.Config{Agents: map[string]config.AgentConfig{}},
	}

	_, _ = m.spawnAgent()

	if m.mode != ModeAgentView {
		t.Errorf("mode = %v, want ModeAgentView (Detached branch must take the attach path, not fall through to spawn)", m.mode)
	}
	if m.focusedPane != ticket.ID {
		t.Errorf("focusedPane = %q, want %q", m.focusedPane, ticket.ID)
	}
	// Original pane is preserved — not replaced by a fresh spawn-path
	// PaneView. Identity check is the load-bearing assertion: a
	// fall-through spawn would m.panes[ticket.ID] = newlyConstructedPV
	// after the closure returns.
	if got, ok := m.panes[ticket.ID]; !ok || got != pane {
		t.Errorf("panes[%s] was replaced; want the original Detached pane", ticket.ID)
	}
	if m.spawningTicketID != "" {
		t.Errorf("spawningTicketID = %q, want \"\" (Detached attach must not enter ModeSpawning)",
			m.spawningTicketID)
	}
}

// TestSpawnAgent_DetachedPaneEmptySessionID_FallsThrough pins the
// complementary half of B6: a Detached pane WITH NO SessionID has
// nothing for attachExisting to target — there is no daemon-side
// session to re-attach to — so the spawn path is the correct fallback.
// The test fixture parks the ticket in StatusBacklog so the spawn
// path's in-progress gate fires the expected notify ("Press Space to
// move to In Progress first") — observably distinct from the
// attach branch (which would set ModeAgentView).
func TestSpawnAgent_DetachedPaneEmptySessionID_FallsThrough(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	ticket := &board.Ticket{
		ID:        "T-DETACH-2",
		Title:     "empty-sid falls through",
		ProjectID: proj.ID,
		Status:    board.StatusBacklog, // gate fires once we fall through
	}
	if err := globalStore.Add(ticket); err != nil {
		t.Fatalf("Add ticket: %v", err)
	}

	pane := makeDetachedPane("") // sessionID="" → no attach target
	if got := pane.State(); got != daemonclient.PaneViewDetached {
		t.Fatalf("test fixture: pane.State() = %v, want PaneViewDetached", got)
	}

	m := &Model{
		globalStore:   globalStore,
		panes:         map[board.TicketID]*daemonclient.PaneView{ticket.ID: pane},
		columnTickets: [][]*board.Ticket{{ticket}, {}, {}, {}},
		columnOffsets: []int{0, 0, 0, 0},
		mode:          ModeNormal,
		activeColumn:  0,
		activeTicket:  0,
		width:         120,
		height:        40,
		config:        &config.Config{Agents: map[string]config.AgentConfig{}},
	}

	_, _ = m.spawnAgent()

	// Fall-through hit the in-progress gate, so the notification is the
	// expected one and the mode never advanced to ModeAgentView.
	if m.mode != ModeNormal {
		t.Errorf("mode = %v, want ModeNormal (fall-through to spawn must hit the in-progress gate, not enter agent view)", m.mode)
	}
	if m.notification != "Press Space to move to In Progress first" {
		t.Errorf("notification = %q, want \"Press Space to move to In Progress first\"", m.notification)
	}
}

// TestRetryAttach_SucceedsAfterTransientFailures pins B7: when the
// post-Spawn attach fails on the first try (transient daemon-side
// contention, brief socket hiccup), the retry loop keeps trying with
// backoff. We assert that the function returns nil once a call
// succeeds, and that the attach func was invoked exactly as many
// times as it took.
func TestRetryAttach_SucceedsAfterTransientFailures(t *testing.T) {
	var calls atomic.Int32
	attach := func(_ context.Context) error {
		n := calls.Add(1)
		if n < 3 {
			return errors.New("transient attach failure")
		}
		return nil
	}

	start := time.Now()
	err := retryAttach(attach)
	elapsed := time.Since(start)

	if err != nil {
		t.Errorf("retryAttach err = %v, want nil (third call should succeed)", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("attach call count = %d, want 3", got)
	}
	// Backoff schedule for retries 1 and 2 is 200ms + 400ms = 600ms
	// total sleep. Allow a generous lower bound so the assertion is
	// stable on a busy CI host but still catches a "didn't sleep at
	// all" regression.
	if elapsed < 500*time.Millisecond {
		t.Errorf("elapsed = %v, want at least ~500ms (backoff schedule should have fired)", elapsed)
	}
}

// TestRetryAttach_GivesUpAfterCap pins the upper bound: when attach
// fails on every attempt, retryAttach returns the final error and
// caps the call count at 1 (initial) + spawnAttachMaxRetries.
func TestRetryAttach_GivesUpAfterCap(t *testing.T) {
	var calls atomic.Int32
	wantErr := errors.New("persistent attach failure")
	attach := func(_ context.Context) error {
		calls.Add(1)
		return wantErr
	}

	err := retryAttach(attach)

	if err == nil {
		t.Errorf("retryAttach err = nil, want non-nil after exhausting retries")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("retryAttach err = %v, want = %v", err, wantErr)
	}
	want := int32(1 + spawnAttachMaxRetries)
	if got := calls.Load(); got != want {
		t.Errorf("attach call count = %d, want %d (1 initial + %d retries)",
			got, want, spawnAttachMaxRetries)
	}
}

// TestPrepareSpawnWith_OwnsFastPathSkipsSpawn pins B4: when the ticket
// has an AgentSessionID and the daemon's Owns probe reports Owned=true,
// prepareSpawnWith MUST NOT issue a Spawn RPC. The fast path constructs
// a PaneView aimed at the existing daemon session and returns
// spawnReadyMsg directly. Idempotent Spawn (PR #34) makes a duplicate
// Spawn harmless, but symmetric with shouldCleanupDeadSession's
// Owns-first pattern: confirm ownership, skip the fork.
//
// Test mechanics:
//   - m.daemon is the stub; its Owns returns Owned=true.
//   - m.daemonClient is nil. If the fast path WERE NOT taken, the
//     closure would either issue Spawn (which would record a call on
//     the stub) or fall into the "daemon unreachable" branch
//     (returning spawnErrorMsg). Both are observable.
//   - The fast path's pv.Attach() will fail (no daemon to dial). Per
//     B7's notice contract, the returned spawnReadyMsg carries the
//     "Attached to existing session but stream failed" diagnostic so
//     we can distinguish it from the regular post-Spawn path's notice.
func TestPrepareSpawnWith_OwnsFastPathSkipsSpawn(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	// Use a tempdir socket path so even if pv.Attach() inside the fast
	// path tries to dial it bails fast (and never forks a real daemon
	// from a unit test).
	t.Setenv("OPENKANBAN_DAEMON_SOCK", t.TempDir()+"/missing.sock")
	t.Setenv("OPENKANBAN_DAEMON_BINARY", "/usr/bin/true")

	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	const wantUUID = "f1e2d3c4-b5a6-7890-1234-567890abcdef"
	const wantDaemonSID = "sid-already-owned"
	ticket := &board.Ticket{
		ID:             "T-OWNS-FAST",
		Title:          "fast-path target",
		ProjectID:      proj.ID,
		Status:         board.StatusInProgress,
		AgentSessionID: wantUUID,
	}
	if err := globalStore.Add(ticket); err != nil {
		t.Fatalf("Add ticket: %v", err)
	}

	stub := &spawnDisciplineStubAPI{
		ownsResp: daemon.OwnsResp{
			Owned:     true,
			SessionID: wantDaemonSID,
		},
	}

	m := &Model{
		globalStore:  globalStore,
		daemon:       stub,
		panes:        map[board.TicketID]*daemonclient.PaneView{},
		daemonClient: nil, // FORCE fast-path: any non-fast-path branch needs daemonClient
		width:        120,
		height:       40,
		config:       &config.Config{Behavior: config.BehaviorSettings{}, Agents: map[string]config.AgentConfig{}},
		worktreeMgrs: nil, // also nil — the fast path runs BEFORE the mgr check
	}

	agentCfg := config.AgentConfig{Command: "claude"}
	cmd := m.prepareSpawnWith(ticket, proj, agentCfg, spawnPlan{})
	if cmd == nil {
		t.Fatal("prepareSpawnWith returned nil cmd")
	}
	msg := cmd()

	// (a) Owns was queried with the ticket's AgentSessionID.
	if got := stub.ownsCalls.Load(); got != 1 {
		t.Errorf("Owns calls = %d, want 1", got)
	}
	// (b) Spawn was NOT issued.
	if got := stub.spawnCalls.Load(); got != 0 {
		t.Errorf("Spawn calls = %d, want 0 (Owns fast-path must skip Spawn)", got)
	}
	// (c) The closure returned spawnReadyMsg, not spawnErrorMsg
	// (which is what the non-fast-path "daemon unreachable" branch
	// returns when daemonClient is nil).
	ready, ok := msg.(spawnReadyMsg)
	if !ok {
		t.Fatalf("msg type = %T, want spawnReadyMsg; msg = %#v", msg, msg)
	}
	// (d) The PaneView points at the daemon-reported SessionID.
	if got := ready.pane.SessionID(); got != wantDaemonSID {
		t.Errorf("pane.SessionID() = %q, want %q (must reuse Owns' daemon-side session)",
			got, wantDaemonSID)
	}
	// (e) The fast path's attach-failure notice is set (we couldn't
	// dial — no real daemon). Distinct from the regular spawn-path
	// notice, so the user can tell what failed.
	const wantNotice = "Attached to existing session but stream failed — press Enter to retry"
	if ready.notice != wantNotice {
		t.Errorf("notice = %q, want %q", ready.notice, wantNotice)
	}
}

// TestPrepareSpawnWith_OwnsFastPath_NotOwned_FallsThrough complements
// the fast-path test: when Owns reports Owned=false the closure must
// continue to the regular spawn-path branches. We assert it doesn't
// short-circuit on the fast path AND that the next blocking branch
// (mgr==nil in this fixture) is reached.
func TestPrepareSpawnWith_OwnsFastPath_NotOwned_FallsThrough(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	ticket := &board.Ticket{
		ID:             "T-OWNS-NO",
		Title:          "fast-path miss",
		ProjectID:      proj.ID,
		Status:         board.StatusInProgress,
		AgentSessionID: "some-uuid",
	}
	if err := globalStore.Add(ticket); err != nil {
		t.Fatalf("Add ticket: %v", err)
	}

	stub := &spawnDisciplineStubAPI{
		ownsResp: daemon.OwnsResp{Owned: false},
	}

	m := &Model{
		globalStore:  globalStore,
		daemon:       stub,
		panes:        map[board.TicketID]*daemonclient.PaneView{},
		daemonClient: nil,
		width:        120,
		height:       40,
		config:       &config.Config{Behavior: config.BehaviorSettings{}, Agents: map[string]config.AgentConfig{}},
		worktreeMgrs: nil,
	}

	cmd := m.prepareSpawnWith(ticket, proj, config.AgentConfig{Command: "claude"}, spawnPlan{})
	if cmd == nil {
		t.Fatal("prepareSpawnWith returned nil cmd")
	}
	msg := cmd()

	if got := stub.ownsCalls.Load(); got != 1 {
		t.Errorf("Owns calls = %d, want 1 (fast path probes even on miss)", got)
	}
	if got := stub.spawnCalls.Load(); got != 0 {
		t.Errorf("Spawn calls = %d, want 0 (mgr==nil short-circuits before reaching Spawn)", got)
	}
	// The closure should have reached the mgr nil-check below the
	// fast-path branch — and returned spawnErrorMsg.
	errMsg, ok := msg.(spawnErrorMsg)
	if !ok {
		t.Fatalf("msg type = %T, want spawnErrorMsg; msg = %#v", msg, msg)
	}
	if errMsg.err != "worktree manager not found" {
		t.Errorf("err = %q, want \"worktree manager not found\" (fall-through must hit mgr check)",
			errMsg.err)
	}
}

// TestPrepareSpawnWith_OwnsFastPath_NoAgentSessionID_Skips pins that
// the Owns probe is only made when there's a UUID to probe with. A
// ticket with empty AgentSessionID has nothing to fast-path on, so
// Owns must NOT be called — that would waste an RPC against a query
// (SessionUUID="") the daemon would treat as Owned=false anyway.
func TestPrepareSpawnWith_OwnsFastPath_NoAgentSessionID_Skips(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	ticket := &board.Ticket{
		ID:        "T-OWNS-NIL-UUID",
		Title:     "no UUID",
		ProjectID: proj.ID,
		Status:    board.StatusInProgress,
		// AgentSessionID intentionally empty
	}
	if err := globalStore.Add(ticket); err != nil {
		t.Fatalf("Add ticket: %v", err)
	}

	stub := &spawnDisciplineStubAPI{
		// Even if Owns is somehow called and returns Owned=true, we
		// shouldn't see this used — the gate above MUST keep Owns
		// from firing.
		ownsResp: daemon.OwnsResp{Owned: true, SessionID: "shouldnt-be-used"},
	}

	m := &Model{
		globalStore:  globalStore,
		daemon:       stub,
		panes:        map[board.TicketID]*daemonclient.PaneView{},
		daemonClient: nil,
		width:        120,
		height:       40,
		config:       &config.Config{Behavior: config.BehaviorSettings{}, Agents: map[string]config.AgentConfig{}},
		worktreeMgrs: nil,
	}

	cmd := m.prepareSpawnWith(ticket, proj, config.AgentConfig{Command: "claude"}, spawnPlan{})
	if cmd == nil {
		t.Fatal("prepareSpawnWith returned nil cmd")
	}
	_ = cmd()

	if got := stub.ownsCalls.Load(); got != 0 {
		t.Errorf("Owns calls = %d, want 0 (no AgentSessionID -> no Owns probe)", got)
	}
}

// TestSessionNameFor mirrors the priority used inside prepareSpawnWith
// (AgentSessionID > branchName > ticket.ID) so the fast-path and
// regular-path PaneViews agree on identity. Pinned as a unit test so
// future refactors of either branch can't drift apart silently.
func TestSessionNameFor(t *testing.T) {
	tests := []struct {
		name       string
		ticket     *board.Ticket
		branchName string
		want       string
	}{
		{
			name:       "uuid present wins",
			ticket:     &board.Ticket{ID: "T-1", AgentSessionID: "uuid-1"},
			branchName: "feat/x",
			want:       "uuid-1",
		},
		{
			name:       "branch name when no uuid",
			ticket:     &board.Ticket{ID: "T-2"},
			branchName: "feat/y",
			want:       "feat/y",
		},
		{
			name:       "ticket id when neither set",
			ticket:     &board.Ticket{ID: "T-3"},
			branchName: "",
			want:       "T-3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sessionNameFor(tt.ticket, tt.branchName); got != tt.want {
				t.Errorf("sessionNameFor = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestSpawnAgent_DetachedAttachFailure_SurfacesError pins the B6
// silent-swallow fix: when the Detached arm of spawnAgent kicks off an
// attachExisting and the resulting attach errors out, the user MUST
// see the failure as a notification — not get parked silently in
// ModeAgentView with a dead pane.
//
// Test mechanics: we don't drive a real daemon-backed attachExisting
// (that would require standing up a daemonclient with a controllable
// failure mode). The change under test is in Update's spawnErrorMsg
// handler — it used to gate notification on
// `msg.ticketID == m.spawningTicketID` AND only fire inside
// `m.mode == ModeSpawning`. The Detached arm sets m.mode = ModeAgentView
// before kicking off attachExisting, and never enters ModeSpawning, so
// before the fix the spawnErrorMsg arrived in a mode that had no
// handler at all. Now the handler is at the top-level switch with the
// gate dropped, so a spawnErrorMsg fired into a Model in ModeAgentView
// (mimicking the post-Detached-arm state) MUST set the notification.
func TestSpawnAgent_DetachedAttachFailure_SurfacesError(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	ticket := &board.Ticket{
		ID:        "T-DETACH-ERR",
		Title:     "detached attach failure",
		ProjectID: proj.ID,
		Status:    board.StatusInProgress,
	}
	if err := globalStore.Add(ticket); err != nil {
		t.Fatalf("Add ticket: %v", err)
	}

	pane := daemonclient.NewPaneView(nil, string(ticket.ID), "sid-daemon", nil)

	m := &Model{
		globalStore:   globalStore,
		panes:         map[board.TicketID]*daemonclient.PaneView{ticket.ID: pane},
		columnTickets: [][]*board.Ticket{{}, {ticket}, {}, {}},
		columnOffsets: []int{0, 0, 0, 0},
		// Post-Detached-arm state: spawnAgent set mode to ModeAgentView,
		// did NOT touch spawningTicketID/spawningAgent, then kicked off
		// the attach. We're modeling the moment the attach errored back.
		mode:             ModeAgentView,
		focusedPane:      ticket.ID,
		activeColumn:     1,
		activeTicket:     0,
		width:            120,
		height:           40,
		config:           &config.Config{Agents: map[string]config.AgentConfig{}},
		spawningTicketID: "", // deliberately empty — the pre-fix gate
		spawningAgent:    "",
	}

	// Construct the message attachExisting would produce on Attach error
	// (see model.go's attachExisting: "attach failed: " + err.Error()).
	errMsg := spawnErrorMsg{
		ticketID: ticket.ID,
		err:      "attach failed: dial tcp: connection refused",
	}

	updated, _ := m.Update(errMsg)
	got, ok := updated.(*Model)
	if !ok {
		t.Fatalf("Update returned %T, want *Model", updated)
	}

	if got.notification == "" {
		t.Errorf("notification = \"\", want a non-empty attach-failure toast (pre-fix bug: gate dropped the message)")
	}
	if got.notification != errMsg.err {
		t.Errorf("notification = %q, want %q", got.notification, errMsg.err)
	}
	// We did NOT enter ModeSpawning, so the spawning-bookkeeping branch
	// should not have fired. Verify mode wasn't clobbered back to
	// ModeNormal by accident.
	if got.mode != ModeAgentView {
		t.Errorf("mode = %v, want ModeAgentView (handler must not flip mode when not mid-spawn)", got.mode)
	}
}

// TestSpawnErrorMsg_DuringSpawningStillClearsState complements the
// gate-drop test by pinning the ModeSpawning branch: when an error
// arrives mid-spawn for the spawning ticket, the handler clears the
// ModeSpawning bookkeeping (mode → ModeNormal, spawningTicketID/Agent
// → "") AND fires the toast. This keeps the existing spawn-path UX
// intact while broadening the handler to cover the attach-only callers.
func TestSpawnErrorMsg_DuringSpawningStillClearsState(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	ticket := &board.Ticket{
		ID:        "T-SPAWNING",
		Title:     "spawn failure mid-spawn",
		ProjectID: proj.ID,
		Status:    board.StatusInProgress,
	}
	if err := globalStore.Add(ticket); err != nil {
		t.Fatalf("Add ticket: %v", err)
	}

	m := &Model{
		globalStore:      globalStore,
		panes:            map[board.TicketID]*daemonclient.PaneView{},
		columnTickets:    [][]*board.Ticket{{}, {ticket}, {}, {}},
		columnOffsets:    []int{0, 0, 0, 0},
		mode:             ModeSpawning,
		spawningTicketID: ticket.ID,
		spawningAgent:    "claude",
		width:            120,
		height:           40,
		config:           &config.Config{Agents: map[string]config.AgentConfig{}},
	}

	errMsg := spawnErrorMsg{
		ticketID: ticket.ID,
		err:      "spawn failed: daemon refused",
	}
	updated, _ := m.Update(errMsg)
	got := updated.(*Model)

	if got.mode != ModeNormal {
		t.Errorf("mode = %v, want ModeNormal (mid-spawn error should drop us back)", got.mode)
	}
	if got.spawningTicketID != "" {
		t.Errorf("spawningTicketID = %q, want \"\"", got.spawningTicketID)
	}
	if got.spawningAgent != "" {
		t.Errorf("spawningAgent = %q, want \"\"", got.spawningAgent)
	}
	if got.notification != errMsg.err {
		t.Errorf("notification = %q, want %q", got.notification, errMsg.err)
	}
}

// (compile-time check) tea.Cmd assignability — make sure no import
// gets dropped if we churn the test contents above.
var _ = func() tea.Cmd { return nil }
