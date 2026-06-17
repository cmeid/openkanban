package ui

import (
	"testing"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/project"
)

// backfillTestEnv builds a real GlobalTicketStore with a registered
// project + saved ticket(s), so backfillAgentSession's LinkSession call
// can actually scan and Save.
func backfillTestEnv(t *testing.T, projectID string, seed ...*board.Ticket) *project.GlobalTicketStore {
	t.Helper()
	registry := &project.ProjectRegistry{Projects: make(map[string]*project.Project)}
	proj := &project.Project{
		ID:       projectID,
		Name:     "test",
		RepoPath: t.TempDir(),
	}
	if err := registry.Add(proj); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("HOME", configHome)
	gs, err := project.LoadGlobalTicketStore(registry)
	if err != nil {
		t.Fatalf("LoadGlobalTicketStore: %v", err)
	}
	for _, tk := range seed {
		store := gs.GetStoreForTicket(tk)
		if store == nil {
			t.Fatalf("project %s not in store", tk.ProjectID)
		}
		store.Add(tk)
		if err := store.SaveTicket(tk); err != nil {
			t.Fatalf("seed SaveTicket: %v", err)
		}
	}
	// Re-load so the seeded tickets land in allTickets (uniqueness
	// scan iterates that map, not the per-project store directly).
	gs, err = project.LoadGlobalTicketStore(registry)
	if err != nil {
		t.Fatalf("reload LoadGlobalTicketStore: %v", err)
	}
	return gs
}

// recordingPurger captures every call so the test can assert call vs
// no-call — the load-bearing vacuous-test guard per the plan.
type recordingPurger struct {
	calls []string // list of uuids passed
}

func (r *recordingPurger) purge(historyPath, uuid string, prefixes ...string) error {
	r.calls = append(r.calls, uuid)
	return nil
}

// TestBackfillAgentSession_HappyPath_ClaimsAndPurges is the
// "fresh-spawn" baseline: the ticket has no AgentSessionID, the JSONL
// is discovered, LinkSession succeeds, save fires, purge fires.
func TestBackfillAgentSession_HappyPath_ClaimsAndPurges(t *testing.T) {
	tk := board.NewTicket("T1", "proj-1")
	gs := backfillTestEnv(t, "proj-1", tk)
	purger := &recordingPurger{}

	id := backfillAgentSession(
		gs, tk.ID, "claude", "/tmp/wt", "/home/test",
		func(string) string { return "" },                   // opencode
		func(string) string { return "uuid-1" },             // claude
		purger.purge,
	)

	if id != "uuid-1" {
		t.Errorf("returned id: got %q, want uuid-1", id)
	}
	reloaded, _ := gs.Get(tk.ID)
	if reloaded.AgentSessionID != "uuid-1" {
		t.Errorf("ticket.AgentSessionID: got %q, want uuid-1", reloaded.AgentSessionID)
	}
	if len(purger.calls) != 1 || purger.calls[0] != "uuid-1" {
		t.Errorf("purger.calls: got %v, want [uuid-1]", purger.calls)
	}
}

// TestBackfillAgentSession_Conflict_SilentNoOp is the load-bearing
// vacuous-test guard from the plan. Pre-fix (the original direct
// `ticket.AgentSessionID = apiSessionID` + unconditional purge call),
// this test FAILS because PurgeClaudePrimingHistory is called even
// when the claim conflicts. Post-fix (LinkSession BestEffort gate),
// the test asserts THREE things — any of them being absent would
// allow a no-op LinkSession to pass vacuously:
//
//   1. Requesting ticket's AgentSessionID stays empty (claim refused).
//   2. Conflict-target ticket's AgentSessionID is unchanged.
//   3. Purger was NOT called. (This is what catches a no-op LinkSession
//      that would otherwise pass #1 and #2 trivially.)
func TestBackfillAgentSession_Conflict_SilentNoOp(t *testing.T) {
	target := board.NewTicket("Target", "proj-1")
	target.AgentSessionID = "uuid-shared"
	requester := board.NewTicket("Requester", "proj-1")
	gs := backfillTestEnv(t, "proj-1", target, requester)
	purger := &recordingPurger{}

	id := backfillAgentSession(
		gs, requester.ID, "claude", "/tmp/wt", "/home/test",
		func(string) string { return "" },
		func(string) string { return "uuid-shared" },
		purger.purge,
	)

	if id != "" {
		t.Errorf("returned id on conflict: got %q, want empty", id)
	}
	// (1) requester remains empty
	reloadedR, _ := gs.Get(requester.ID)
	if reloadedR.AgentSessionID != "" {
		t.Errorf("requester.AgentSessionID: got %q, want empty (claim refused)", reloadedR.AgentSessionID)
	}
	// (2) target unchanged
	reloadedT, _ := gs.Get(target.ID)
	if reloadedT.AgentSessionID != "uuid-shared" {
		t.Errorf("target.AgentSessionID: got %q, want uuid-shared (unchanged)", reloadedT.AgentSessionID)
	}
	// (3) purger NOT called — the vacuous-test guard
	if len(purger.calls) != 0 {
		t.Errorf("purger.calls on conflict: got %v, want empty (no purge when claim refused)", purger.calls)
	}
}

// TestBackfillAgentSession_Opencode_NoPurge confirms purge is
// claude-specific. Opencode discovery should claim but never purge
// (no priming history concept for opencode).
func TestBackfillAgentSession_Opencode_NoPurge(t *testing.T) {
	tk := board.NewTicket("T1", "proj-1")
	gs := backfillTestEnv(t, "proj-1", tk)
	purger := &recordingPurger{}

	id := backfillAgentSession(
		gs, tk.ID, "opencode", "/tmp/wt", "/home/test",
		func(string) string { return "uuid-oc" },
		func(string) string { return "" },
		purger.purge,
	)

	if id != "uuid-oc" {
		t.Errorf("returned id: got %q, want uuid-oc", id)
	}
	if len(purger.calls) != 0 {
		t.Errorf("purger.calls for opencode: got %v, want empty", purger.calls)
	}
}

// TestBackfillAgentSession_NilStore_NoOp guards the early-return.
func TestBackfillAgentSession_NilStore_NoOp(t *testing.T) {
	purger := &recordingPurger{}
	id := backfillAgentSession(
		nil, "tid", "claude", "/tmp/wt", "/home/x",
		func(string) string { return "" },
		func(string) string { return "uuid" },
		purger.purge,
	)
	if id != "" {
		t.Errorf("got %q, want empty", id)
	}
	if len(purger.calls) != 0 {
		t.Errorf("purger.calls: got %v, want empty", purger.calls)
	}
}

// TestBackfillAgentSession_NoJSONL_NoOp guards the find-empty path.
func TestBackfillAgentSession_NoJSONL_NoOp(t *testing.T) {
	tk := board.NewTicket("T1", "proj-1")
	gs := backfillTestEnv(t, "proj-1", tk)
	purger := &recordingPurger{}

	id := backfillAgentSession(
		gs, tk.ID, "claude", "/tmp/wt", "/home/test",
		func(string) string { return "" },
		func(string) string { return "" }, // find returns empty
		purger.purge,
	)
	if id != "" {
		t.Errorf("got %q, want empty", id)
	}
	if len(purger.calls) != 0 {
		t.Errorf("purger.calls: got %v, want empty", purger.calls)
	}
}
