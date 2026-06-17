package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/project"
	"github.com/techdufus/openkanban/internal/ticketsvc"
)

// linkSessionTestEnv assembles a registry + globalStore for the
// uniqueness tests. Returns the registry so the test can reload the
// globalStore after persisting tickets.
type linkSessionTestEnv struct {
	registry  *project.ProjectRegistry
	gs        *project.GlobalTicketStore
	configDir string
}

func newLinkSessionTestEnv(t *testing.T, projectID string) *linkSessionTestEnv {
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
	if err := os.MkdirAll(filepath.Join(configHome, "openkanban", "tickets", projectID), 0o755); err != nil {
		t.Fatalf("mkdir project tickets: %v", err)
	}
	gs, err := project.LoadGlobalTicketStore(registry)
	if err != nil {
		t.Fatalf("LoadGlobalTicketStore: %v", err)
	}
	return &linkSessionTestEnv{registry: registry, gs: gs, configDir: configHome}
}

func (e *linkSessionTestEnv) reload(t *testing.T) {
	t.Helper()
	gs, err := project.LoadGlobalTicketStore(e.registry)
	if err != nil {
		t.Fatalf("reload LoadGlobalTicketStore: %v", err)
	}
	e.gs = gs
}

// fakeSessionFile creates a Claude-shaped session JSONL so
// applySessionFlags's SessionPath check passes without exercising real
// Claude.
func fakeSessionFile(t *testing.T, uuid string) {
	t.Helper()
	home := os.Getenv("HOME")
	dir := filepath.Join(home, ".claude", "projects", "fake")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, uuid+".jsonl"), []byte(`{}`+"\n"), 0o644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}
}

// TestTicketNew_Session_RefusesDuplicate is the load-bearing red-before-
// green test for Task 2. Against main, applySessionFlags writes
// AgentSessionID unconditionally — two tickets with the same --session
// UUID end up linked to the same session (the bug). Post-fix,
// ticketsvc.LinkSession refuses the second creation with
// *ticketsvc.ErrSessionAlreadyLinked unless --force is passed.
//
// Pre-fix proof: replacing the LinkSession call in applySessionFlags
// with the direct `ticket.AgentSessionID = uuid` write makes this test
// FAIL — both tickets end up linked.
func TestTicketNew_Session_RefusesDuplicate(t *testing.T) {
	uuid := "55555555-5555-4555-8555-555555555555"
	env := newLinkSessionTestEnv(t, "proj-1")
	fakeSessionFile(t, uuid)

	// First ticket claims the UUID.
	first := board.NewTicket("first", "proj-1")
	t.Cleanup(resetTicketNewFlags)
	resetTicketNewFlags()
	ticketNewSession = uuid

	if err := applySessionFlags(first, env.gs); err != nil {
		t.Fatalf("first applySessionFlags: %v", err)
	}
	if first.AgentSessionID != uuid {
		t.Fatalf("first.AgentSessionID: got %q, want %q", first.AgentSessionID, uuid)
	}

	// Persist first ticket, then reload globalStore so the uniqueness
	// scan in the second creation sees the claim.
	if err := saveTicketToProject(env.gs, "proj-1", first); err != nil {
		t.Fatalf("save first ticket: %v", err)
	}
	env.reload(t)

	// Second ticket tries the same UUID — REFUSED.
	second := board.NewTicket("second", "proj-1")
	resetTicketNewFlags()
	ticketNewSession = uuid

	err := applySessionFlags(second, env.gs)
	if err == nil {
		t.Fatalf("second applySessionFlags: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "already linked") {
		t.Errorf("error message: %q does not mention 'already linked'", err.Error())
	}
	var conflict *ticketsvc.ErrSessionAlreadyLinked
	if !errors.As(err, &conflict) {
		t.Errorf("err type: got %T, want *ticketsvc.ErrSessionAlreadyLinked", err)
	}
	if second.AgentSessionID != "" {
		t.Errorf("second.AgentSessionID after refuse: got %q, want empty", second.AgentSessionID)
	}
}

// TestTicketNew_Session_Force_StealsLink proves the --force semantics:
// claim a uuid already linked elsewhere, clearing the other ticket's
// link.
func TestTicketNew_Session_Force_StealsLink(t *testing.T) {
	uuid := "66666666-6666-4666-8666-666666666666"
	env := newLinkSessionTestEnv(t, "proj-1")
	fakeSessionFile(t, uuid)

	first := board.NewTicket("first", "proj-1")
	t.Cleanup(resetTicketNewFlags)
	resetTicketNewFlags()
	ticketNewSession = uuid
	if err := applySessionFlags(first, env.gs); err != nil {
		t.Fatalf("first applySessionFlags: %v", err)
	}
	if err := saveTicketToProject(env.gs, "proj-1", first); err != nil {
		t.Fatalf("save first: %v", err)
	}
	env.reload(t)

	// Second --session --force CLAIMS, clearing first.
	second := board.NewTicket("second", "proj-1")
	resetTicketNewFlags()
	ticketNewSession = uuid
	ticketNewForce = true

	if err := applySessionFlags(second, env.gs); err != nil {
		t.Fatalf("second applySessionFlags: %v", err)
	}
	if second.AgentSessionID != uuid {
		t.Errorf("second.AgentSessionID: got %q, want %q", second.AgentSessionID, uuid)
	}
	// First's link should be cleared. Reload globalStore + read the
	// first ticket from disk to verify the Force-cleared persistence.
	env.reload(t)
	reloaded, err := env.gs.Get(first.ID)
	if err != nil {
		t.Fatalf("reload first ticket: %v", err)
	}
	if reloaded.AgentSessionID != "" {
		t.Errorf("first.AgentSessionID after Force (from disk): got %q, want empty", reloaded.AgentSessionID)
	}
}

// saveTicketToProject is a test helper that writes a ticket to the
// project's TicketStore so a subsequent LoadGlobalTicketStore picks it
// up.
func saveTicketToProject(gs *project.GlobalTicketStore, projectID string, tk *board.Ticket) error {
	store := gs.GetStoreForTicket(tk)
	if store == nil {
		return errors.New("project not registered in global store")
	}
	store.Add(tk)
	return store.SaveTicket(tk)
}
