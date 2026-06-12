package project

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/board"
)

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return parsed
}

func ptrTime(t time.Time) *time.Time { return &t }

// fixtureTicket returns a ticket with every field populated to a
// distinguishable non-zero value, for round-trip testing.
func fixtureTicket(t *testing.T) *board.Ticket {
	t.Helper()
	created := mustParseTime(t, "2026-06-10T09:00:00Z")
	updated := mustParseTime(t, "2026-06-12T11:42:00Z")
	started := mustParseTime(t, "2026-06-11T10:00:00Z")
	spawned := mustParseTime(t, "2026-06-12T11:30:00Z")

	return &board.Ticket{
		ID:             "7f3a9b2c-1d8e-4a5b-9c3d-2f1e0a8b9c4d",
		ProjectID:      "proj-abc",
		Title:          "Wire fsnotify watcher",
		// Description is canonicalised to no-trailing-newline. Multi-line
		// internal newlines are preserved verbatim.
		Description:    "Multi-line\n\nbody preserved verbatim.\n\n- item 1\n- item 2",
		Status:         board.StatusInProgress,
		UseWorktree:    true,
		WorktreePath:   "/home/u/wt/task-fsnotify",
		BranchName:     "task/fsnotify",
		BaseBranch:     "main",
		AgentType:      "claude",
		AgentStatus:    board.AgentWorking,
		AgentSpawnedAt: ptrTime(spawned),
		AgentPort:      4097,
		AgentSessionID: "sess-42",
		CreatedAt:      created,
		UpdatedAt:      updated,
		StartedAt:      ptrTime(started),
		CompletedAt:    nil,
		Labels:         []string{"storage", "tui"},
		Priority:       2,
		Meta:           map[string]string{"epic": "hot-reload", "owner": "chris"},
		BlockedBy:      []board.TicketID{"a1b2c3d4-...."},
	}
}

func TestMarshalUnmarshalRoundTrip(t *testing.T) {
	orig := fixtureTicket(t)

	data, err := MarshalTicket(orig)
	if err != nil {
		t.Fatalf("MarshalTicket: %v", err)
	}

	got, err := UnmarshalTicket(data)
	if err != nil {
		t.Fatalf("UnmarshalTicket: %v\n--- payload ---\n%s", err, data)
	}

	if !reflect.DeepEqual(got, orig) {
		t.Errorf("round-trip mismatch\n got: %#v\nwant: %#v", got, orig)
	}
}

// TestMarshalEmptyCollectionsRoundTrip guards against the nil-vs-empty
// trap. NewTicket initialises Labels and Meta as non-nil empty
// containers; round-tripping must preserve that, so callers can rely
// on len()/range without nil checks.
func TestMarshalEmptyCollectionsRoundTrip(t *testing.T) {
	orig := board.NewTicket("Empty collections", "proj-test")
	// Lock timestamps so DeepEqual doesn't trip on time.Now().
	orig.CreatedAt = mustParseTime(t, "2026-06-12T00:00:00Z")
	orig.UpdatedAt = orig.CreatedAt
	// Post-load convention: BlockedBy is non-nil empty (matches Labels/Meta).
	// We normalise here so the pre/post comparison is fair; the on-disk
	// shape unconditionally produces []board.TicketID{} on Unmarshal.
	if orig.BlockedBy == nil {
		orig.BlockedBy = []board.TicketID{}
	}

	data, err := MarshalTicket(orig)
	if err != nil {
		t.Fatalf("MarshalTicket: %v", err)
	}

	got, err := UnmarshalTicket(data)
	if err != nil {
		t.Fatalf("UnmarshalTicket: %v", err)
	}

	if got.Labels == nil {
		t.Errorf("Labels became nil after round-trip; want non-nil empty slice")
	}
	if got.Meta == nil {
		t.Errorf("Meta became nil after round-trip; want non-nil empty map")
	}
	if !reflect.DeepEqual(got, orig) {
		t.Errorf("round-trip mismatch\n got: %#v\nwant: %#v", got, orig)
	}
}

func TestMarshalBodyContainsDescription(t *testing.T) {
	orig := fixtureTicket(t)

	data, err := MarshalTicket(orig)
	if err != nil {
		t.Fatalf("MarshalTicket: %v", err)
	}

	out := string(data)

	// Structural: starts with ---, has closing ---, body follows.
	if !strings.HasPrefix(out, "---\n") {
		t.Errorf("output should start with frontmatter delimiter; got first 20 bytes: %q", out[:min(20, len(out))])
	}
	if !strings.Contains(out, "\n---\n") {
		t.Error("output should contain closing frontmatter delimiter")
	}

	// Body content present verbatim.
	if !strings.Contains(out, orig.Description) {
		t.Error("description body should appear verbatim in output")
	}

	// Description must NOT appear in frontmatter section (else round-trip will double-attach).
	frontmatter := out[:strings.Index(out, "\n---\n")+5]
	if strings.Contains(frontmatter, "description:") {
		t.Error("description must not be emitted as a frontmatter field; it belongs in the body")
	}
}

func TestMarshalDeterministicFieldOrder(t *testing.T) {
	orig := fixtureTicket(t)

	a, err := MarshalTicket(orig)
	if err != nil {
		t.Fatalf("MarshalTicket a: %v", err)
	}
	b, err := MarshalTicket(orig)
	if err != nil {
		t.Fatalf("MarshalTicket b: %v", err)
	}

	if string(a) != string(b) {
		t.Errorf("output not deterministic across calls\n a:\n%s\n b:\n%s", a, b)
	}
}

func TestUnmarshalRejectsMissingFrontmatter(t *testing.T) {
	cases := map[string]string{
		"no-leading-fence":  "no fences here\nat all",
		"no-closing-fence":  "---\nid: abc\ntitle: x\n\nbody but never closed",
		"empty":             "",
		"only-leading":      "---\n",
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := UnmarshalTicket([]byte(payload))
			if err == nil {
				t.Errorf("expected error for %q payload, got nil", name)
			}
		})
	}
}

func TestTicketFilenameSlugAndUUID8(t *testing.T) {
	cases := []struct {
		name    string
		ticket  *board.Ticket
		want    string
	}{
		{
			name: "normal",
			ticket: &board.Ticket{
				ID:    "7f3a9b2c-1d8e-4a5b-9c3d-2f1e0a8b9c4d",
				Title: "Wire fsnotify watcher",
			},
			want: "wire-fsnotify-watcher-7f3a9b2c.md",
		},
		{
			name: "title with punctuation",
			ticket: &board.Ticket{
				ID:    "abc12345-...-",
				Title: "Fix bug: NULL pointer in foo()!",
			},
			want: "fix-bug-null-pointer-in-foo-abc12345.md",
		},
		{
			name: "empty title falls back to untitled",
			ticket: &board.Ticket{
				ID:    "deadbeef-...",
				Title: "",
			},
			want: "untitled-deadbeef.md",
		},
		{
			name: "short uuid",
			ticket: &board.Ticket{
				ID:    "short",
				Title: "Hello",
			},
			want: "hello-short.md",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TicketFilename(tc.ticket)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
