package ticketsvc

import (
	"errors"
	"testing"

	"github.com/techdufus/openkanban/internal/agent"
	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/daemon"
)

// fakeStore is the test double for the TicketStore consumer interface.
// Records Save calls so tests can assert call vs no-call (the
// vacuous-test guard the critic insisted on: a no-op LinkSession would
// trivially pass "ticket unchanged" but would fail "Save not called").
type fakeStore struct {
	tickets   map[board.TicketID]*board.Ticket
	saveCalls map[board.TicketID]int
}

func newFakeStore(tickets ...*board.Ticket) *fakeStore {
	s := &fakeStore{
		tickets:   make(map[board.TicketID]*board.Ticket),
		saveCalls: make(map[board.TicketID]int),
	}
	for _, t := range tickets {
		s.tickets[t.ID] = t
	}
	return s
}

func (s *fakeStore) FindByAgentSessionID(uuid string) []*board.Ticket {
	if uuid == "" {
		return nil
	}
	var out []*board.Ticket
	for _, t := range s.tickets {
		if t.AgentSessionID == uuid {
			out = append(out, t)
		}
	}
	return out
}

func (s *fakeStore) Save(t *board.Ticket) error {
	s.saveCalls[t.ID]++
	return nil
}

func TestLinkSession_HappyPath_MutatesNoSave(t *testing.T) {
	tk := board.NewTicket("T1", "p1")
	store := newFakeStore(tk)

	written, err := LinkSession(store, tk, "uuid-1", LinkOpts{})
	if err != nil {
		t.Fatalf("LinkSession: %v", err)
	}
	if !written {
		t.Errorf("written: got false, want true")
	}
	if tk.AgentSessionID != "uuid-1" {
		t.Errorf("AgentSessionID: got %q, want uuid-1", tk.AgentSessionID)
	}
	// LinkSession does NOT save the requesting ticket — caller does.
	// This pins the contract: 0 save calls in the happy path.
	if store.saveCalls[tk.ID] != 0 {
		t.Errorf("Save(requesting): got %d calls, want 0 (caller saves)", store.saveCalls[tk.ID])
	}
}

func TestLinkSession_Idempotent_SkipsWhenSameUUID(t *testing.T) {
	tk := board.NewTicket("T1", "p1")
	tk.AgentSessionID = "uuid-1"
	store := newFakeStore(tk)

	written, err := LinkSession(store, tk, "uuid-1", LinkOpts{})
	if err != nil {
		t.Fatalf("LinkSession: %v", err)
	}
	if written {
		t.Errorf("written: got true, want false (idempotent)")
	}
	if store.saveCalls[tk.ID] != 0 {
		t.Errorf("Save calls: got %d, want 0 (idempotent)", store.saveCalls[tk.ID])
	}
}

// TestLinkSession_ConflictPreservesTarget is the vacuous-test guard:
// a no-op LinkSession that always returns (false, nil) would pass
// "requester empty" but FAIL "target unchanged + no Save call." Both
// assertions must fire.
func TestLinkSession_ConflictPreservesTarget(t *testing.T) {
	target := board.NewTicket("Target", "p1")
	target.AgentSessionID = "uuid-shared"
	requester := board.NewTicket("Requester", "p1")
	store := newFakeStore(target, requester)

	written, err := LinkSession(store, requester, "uuid-shared", LinkOpts{})
	if written {
		t.Errorf("written: got true, want false")
	}
	var conflict *ErrSessionAlreadyLinked
	if !errors.As(err, &conflict) {
		t.Fatalf("err: got %v, want *ErrSessionAlreadyLinked", err)
	}
	if len(conflict.ConflictTicketIDs) != 1 || conflict.ConflictTicketIDs[0] != target.ID {
		t.Errorf("ConflictTicketIDs: got %v, want [%s]", conflict.ConflictTicketIDs, target.ID)
	}
	// (a) requester unchanged
	if requester.AgentSessionID != "" {
		t.Errorf("requester.AgentSessionID: got %q, want empty (claim refused)", requester.AgentSessionID)
	}
	// (b) target unchanged
	if target.AgentSessionID != "uuid-shared" {
		t.Errorf("target.AgentSessionID: got %q, want uuid-shared (untouched)", target.AgentSessionID)
	}
	// (c) NO Save called — the vacuous-test guard
	if store.saveCalls[requester.ID] != 0 {
		t.Errorf("Save(requester): got %d calls, want 0", store.saveCalls[requester.ID])
	}
	if store.saveCalls[target.ID] != 0 {
		t.Errorf("Save(target): got %d calls, want 0", store.saveCalls[target.ID])
	}
}

func TestLinkSession_BestEffort_SilentNoOpOnConflict(t *testing.T) {
	target := board.NewTicket("Target", "p1")
	target.AgentSessionID = "uuid-shared"
	requester := board.NewTicket("Requester", "p1")
	store := newFakeStore(target, requester)

	written, err := LinkSession(store, requester, "uuid-shared", LinkOpts{BestEffort: true})
	if err != nil {
		t.Fatalf("LinkSession BestEffort returned error: %v", err)
	}
	if written {
		t.Errorf("written: got true, want false")
	}
	if requester.AgentSessionID != "" {
		t.Errorf("requester.AgentSessionID: got %q, want empty", requester.AgentSessionID)
	}
	if store.saveCalls[requester.ID] != 0 {
		t.Errorf("Save(requester): want 0, got %d", store.saveCalls[requester.ID])
	}
}

func TestLinkSession_Force_ClearsConflictsAndClaims(t *testing.T) {
	target := board.NewTicket("Target", "p1")
	target.AgentSessionID = "uuid-shared"
	requester := board.NewTicket("Requester", "p1")
	store := newFakeStore(target, requester)

	written, err := LinkSession(store, requester, "uuid-shared", LinkOpts{Force: true})
	if err != nil {
		t.Fatalf("LinkSession Force: %v", err)
	}
	if !written {
		t.Errorf("written: got false, want true")
	}
	if target.AgentSessionID != "" {
		t.Errorf("target.AgentSessionID after Force: got %q, want empty", target.AgentSessionID)
	}
	if requester.AgentSessionID != "uuid-shared" {
		t.Errorf("requester.AgentSessionID: got %q, want uuid-shared", requester.AgentSessionID)
	}
	if store.saveCalls[target.ID] != 1 {
		t.Errorf("Save(target): want 1 (Force cleared), got %d", store.saveCalls[target.ID])
	}
	// Requesting save is the caller's job (CLI's store.SaveTicket).
	if store.saveCalls[requester.ID] != 0 {
		t.Errorf("Save(requester): want 0 (caller saves), got %d", store.saveCalls[requester.ID])
	}
}

func TestLinkSession_BestEffortAndForce_Rejected(t *testing.T) {
	tk := board.NewTicket("T1", "p1")
	store := newFakeStore(tk)
	_, err := LinkSession(store, tk, "uuid-1", LinkOpts{BestEffort: true, Force: true})
	if err == nil {
		t.Errorf("expected error for mutually-exclusive opts, got nil")
	}
}

func TestLinkSession_EmptyUUID_NoOp(t *testing.T) {
	tk := board.NewTicket("T1", "p1")
	store := newFakeStore(tk)
	written, err := LinkSession(store, tk, "", LinkOpts{})
	if err != nil {
		t.Fatalf("LinkSession empty uuid: %v", err)
	}
	if written {
		t.Errorf("written: got true on empty uuid")
	}
}

func TestLinkSession_MultipleConflicts(t *testing.T) {
	// Storage tolerates duplicates by policy — exercise that
	// LinkSession lists all conflicting tickets, not just the first.
	a := board.NewTicket("A", "p1")
	a.AgentSessionID = "uuid-shared"
	b := board.NewTicket("B", "p1")
	b.AgentSessionID = "uuid-shared"
	requester := board.NewTicket("R", "p1")
	store := newFakeStore(a, b, requester)

	_, err := LinkSession(store, requester, "uuid-shared", LinkOpts{})
	var conflict *ErrSessionAlreadyLinked
	if !errors.As(err, &conflict) {
		t.Fatalf("err: got %v, want *ErrSessionAlreadyLinked", err)
	}
	if len(conflict.ConflictTicketIDs) != 2 {
		t.Errorf("ConflictTicketIDs: got %d, want 2", len(conflict.ConflictTicketIDs))
	}
}

// ---- GateAttach tests ----

func TestGateAttach_NotHeld_AllowsAttach(t *testing.T) {
	probe := func(uuid string) (agent.SessionHolder, *daemon.OwnsResp, error) {
		return agent.SessionHolder{}, nil, nil
	}
	if err := GateAttach(probe, "uuid-1", ""); err != nil {
		t.Errorf("GateAttach: got %v, want nil", err)
	}
}

func TestGateAttach_LsofHolder_Refuses(t *testing.T) {
	probe := func(uuid string) (agent.SessionHolder, *daemon.OwnsResp, error) {
		return agent.SessionHolder{PID: 12345, Path: "/tmp/x.jsonl"}, nil, nil
	}
	err := GateAttach(probe, "uuid-1", "")
	var inUse *ErrSessionInUse
	if !errors.As(err, &inUse) {
		t.Fatalf("err: got %v, want *ErrSessionInUse", err)
	}
	if inUse.HolderPID != 12345 {
		t.Errorf("HolderPID: got %d, want 12345", inUse.HolderPID)
	}
}

func TestGateAttach_DaemonOwnsForUs_Allows(t *testing.T) {
	probe := func(uuid string) (agent.SessionHolder, *daemon.OwnsResp, error) {
		return agent.SessionHolder{}, &daemon.OwnsResp{
			Owned:           true,
			SessionID:       "ours-123",
			OwnedByTicketID: "TICK-A",
		}, nil
	}
	if err := GateAttach(probe, "uuid-1", "TICK-A"); err != nil {
		t.Errorf("GateAttach idempotent re-attach: got %v, want nil", err)
	}
}

func TestGateAttach_DaemonOwnsForOther_Refuses(t *testing.T) {
	probe := func(uuid string) (agent.SessionHolder, *daemon.OwnsResp, error) {
		return agent.SessionHolder{}, &daemon.OwnsResp{
			Owned:           true,
			SessionID:       "theirs-456",
			OwnedByTicketID: "TICK-B",
		}, nil
	}
	err := GateAttach(probe, "uuid-1", "TICK-A")
	var inUse *ErrSessionInUse
	if !errors.As(err, &inUse) {
		t.Fatalf("err: got %v, want *ErrSessionInUse", err)
	}
	if inUse.DaemonSessionID != "theirs-456" {
		t.Errorf("DaemonSessionID: got %q, want theirs-456", inUse.DaemonSessionID)
	}
	if inUse.DaemonOwnedByTicketID != "TICK-B" {
		t.Errorf("DaemonOwnedByTicketID: got %q, want TICK-B", inUse.DaemonOwnedByTicketID)
	}
}

func TestGateAttach_DaemonOwnsEmptyTicketID_AllowsForOldDaemonCompat(t *testing.T) {
	// Wire compat: a pre-OwnedByTicketID daemon returns Owned=true with
	// the field empty. GateAttach must NOT refuse — fall through to
	// "treat as ours" to preserve the existing fast-path attach behavior
	// for users mid-upgrade.
	probe := func(uuid string) (agent.SessionHolder, *daemon.OwnsResp, error) {
		return agent.SessionHolder{}, &daemon.OwnsResp{
			Owned:     true,
			SessionID: "legacy-sess",
			// OwnedByTicketID intentionally empty — old daemon.
		}, nil
	}
	if err := GateAttach(probe, "uuid-1", "TICK-A"); err != nil {
		t.Errorf("old-daemon compat: got %v, want nil", err)
	}
}

func TestGateAttach_DaemonConflict_Refuses(t *testing.T) {
	// Daemon multi-match: refuse even if one of the matches happens to
	// be ours. The 1:1 invariant has fractured; we surface it.
	probe := func(uuid string) (agent.SessionHolder, *daemon.OwnsResp, error) {
		return agent.SessionHolder{}, &daemon.OwnsResp{
			Owned:              true,
			SessionID:          "first",
			OwnedByTicketID:    "TICK-A", // matches our requesting ticket
			Conflict:           true,
			ConflictSessionIDs: []string{"first", "second"},
		}, nil
	}
	err := GateAttach(probe, "uuid-1", "TICK-A")
	var inUse *ErrSessionInUse
	if !errors.As(err, &inUse) {
		t.Fatalf("err: got %v, want *ErrSessionInUse on conflict even when first match is ours", err)
	}
	if len(inUse.ConflictSessionIDs) != 2 {
		t.Errorf("ConflictSessionIDs: got %v, want 2 entries", inUse.ConflictSessionIDs)
	}
}

func TestGateAttach_LsofTrumpsDaemon(t *testing.T) {
	// If lsof shows a holder AND daemon owns for us, lsof wins
	// (external process is the harder refuse).
	probe := func(uuid string) (agent.SessionHolder, *daemon.OwnsResp, error) {
		return agent.SessionHolder{PID: 99, Path: "/tmp/x"}, &daemon.OwnsResp{
			Owned:           true,
			SessionID:       "ours",
			OwnedByTicketID: "TICK-A",
		}, nil
	}
	err := GateAttach(probe, "uuid-1", "TICK-A")
	var inUse *ErrSessionInUse
	if !errors.As(err, &inUse) {
		t.Fatalf("err: got %v, want *ErrSessionInUse (lsof should refuse even when daemon owns for us)", err)
	}
	if inUse.HolderPID != 99 {
		t.Errorf("HolderPID: got %d, want 99", inUse.HolderPID)
	}
}

func TestGateAttach_NilProbe_NoOp(t *testing.T) {
	if err := GateAttach(nil, "uuid-1", "TICK-A"); err != nil {
		t.Errorf("nil probe: got %v, want nil", err)
	}
}

func TestGateAttach_EmptyUUID_NoOp(t *testing.T) {
	probe := func(uuid string) (agent.SessionHolder, *daemon.OwnsResp, error) {
		t.Fatal("probe should not be called for empty uuid")
		return agent.SessionHolder{}, nil, nil
	}
	if err := GateAttach(probe, "", "TICK-A"); err != nil {
		t.Errorf("empty uuid: got %v, want nil", err)
	}
}

func TestGateAttach_ProbeError_Wrapped(t *testing.T) {
	want := errors.New("lsof exploded")
	probe := func(uuid string) (agent.SessionHolder, *daemon.OwnsResp, error) {
		return agent.SessionHolder{}, nil, want
	}
	err := GateAttach(probe, "uuid-1", "TICK-A")
	if !errors.Is(err, want) {
		t.Errorf("probe error not propagated: got %v", err)
	}
}
