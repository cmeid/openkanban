package cmd

import (
	"strings"
	"testing"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/project"
)

// resolveTicketFixture builds an in-memory store + registry (no disk) for
// exercising resolveTicket's matching tiers.
func resolveTicketFixture(t *testing.T) (*project.TicketStore, *project.ProjectRegistry, *project.Project) {
	t.Helper()
	proj := project.NewProject("alpha", t.TempDir())
	reg := &project.ProjectRegistry{Projects: map[string]*project.Project{proj.ID: proj}}
	store := project.NewTicketStore(proj.ID, proj.RepoPath)
	return store, reg, proj
}

func TestResolveTicket_ExactAndPrefix(t *testing.T) {
	store, reg, proj := resolveTicketFixture(t)
	tk := board.NewTicket("Fix the thing", proj.ID)
	store.Add(tk)

	// Exact id.
	got, err := resolveTicket(store, reg, string(tk.ID))
	if err != nil || got.ID != tk.ID {
		t.Fatalf("exact id: got %v, err %v", got, err)
	}

	// Unique id prefix (>=4) — also the form the filename short-hash takes.
	got, err = resolveTicket(store, reg, string(tk.ID)[:8])
	if err != nil || got.ID != tk.ID {
		t.Fatalf("id prefix: got %v, err %v", got, err)
	}
}

func TestResolveTicket_TitleSlug(t *testing.T) {
	store, reg, proj := resolveTicketFixture(t)
	tk := board.NewTicket("Fix the thing", proj.ID)
	store.Add(tk)

	got, err := resolveTicket(store, reg, "fix-the-thing")
	if err != nil {
		t.Fatalf("slug: err %v", err)
	}
	if got.ID != tk.ID {
		t.Errorf("slug resolved to %s, want %s", got.ID, tk.ID)
	}
}

func TestResolveTicket_AmbiguousPrefix(t *testing.T) {
	store, reg, proj := resolveTicketFixture(t)
	// Two tickets sharing a contrived id prefix would be ideal, but ids
	// are random UUIDs. Instead force ambiguity via the slug tier: two
	// tickets with the same title -> same slug.
	a := board.NewTicket("Same Title", proj.ID)
	b := board.NewTicket("Same Title", proj.ID)
	store.Add(a)
	store.Add(b)

	_, err := resolveTicket(store, reg, "same-title")
	if err == nil {
		t.Fatal("expected ambiguity error, got nil")
	}
	if !strings.Contains(err.Error(), "matches 2 tickets") {
		t.Errorf("error %q does not report the ambiguity", err.Error())
	}
}

func TestResolveTicket_ProjectIDHint(t *testing.T) {
	store, reg, proj := resolveTicketFixture(t)
	store.Add(board.NewTicket("a ticket", proj.ID))
	ticketDeleteProject = proj.ID // referenced by the not-found message
	t.Cleanup(func() { ticketDeleteProject = "" })

	// Passing the PROJECT id (the footgun) yields a corrective hint.
	_, err := resolveTicket(store, reg, proj.ID)
	if err == nil {
		t.Fatal("expected project-id hint error, got nil")
	}
	if !strings.Contains(err.Error(), "is a project id") || !strings.Contains(err.Error(), "ticket list") {
		t.Errorf("error %q is not the project-id hint", err.Error())
	}

	// A project-id prefix triggers the prefix variant.
	_, err = resolveTicket(store, reg, proj.ID[:6])
	if err == nil {
		t.Fatal("expected project-id-prefix hint, got nil")
	}
	if !strings.Contains(err.Error(), "project id prefix") {
		t.Errorf("error %q is not the project-id-prefix hint", err.Error())
	}
}

func TestResolveTicket_NotFound(t *testing.T) {
	store, reg, proj := resolveTicketFixture(t)
	store.Add(board.NewTicket("a ticket", proj.ID))
	ticketDeleteProject = proj.ID
	t.Cleanup(func() { ticketDeleteProject = "" })

	_, err := resolveTicket(store, reg, "zzzz-nonexistent")
	if err == nil {
		t.Fatal("expected not-found error, got nil")
	}
	if !strings.Contains(err.Error(), "no ticket matches") {
		t.Errorf("error %q is not the not-found message", err.Error())
	}
}
