package agent

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/techdufus/openkanban/internal/board"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBranchSlug(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "task prefix simple", input: "task/foo", want: "foo"},
		{name: "task prefix with dashes", input: "task/foo-bar-baz", want: "foo-bar-baz"},
		{name: "task prefix only", input: "task/", want: ""},
		{name: "empty", input: "", want: ""},
		{name: "feature prefix unchanged", input: "feature/foo", want: "feature/foo"},
		{name: "bare name unchanged", input: "foo", want: "foo"},
		{name: "double task only strips once", input: "task/task/foo", want: "task/foo"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BranchSlug(tt.input)
			if got != tt.want {
				t.Errorf("BranchSlug(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestMergeTicketBrief(t *testing.T) {
	type pre struct {
		// relative to worktreePath; if empty, no pre-existing file is written
		relPath string
		content string
	}

	tests := []struct {
		name         string
		ticket       *board.Ticket
		useWorktree  bool // false => pass "" for worktreePath
		pre          *pre
		wantPath     string
		wantHasBrief bool
		wantErr      bool
		// post-conditions evaluated against the written file (or absence)
		check func(t *testing.T, worktreePath string)
	}{
		{
			name:         "nil ticket",
			ticket:       nil,
			useWorktree:  true,
			wantPath:     "",
			wantHasBrief: false,
			check: func(t *testing.T, worktreePath string) {
				entries, _ := os.ReadDir(filepath.Join(worktreePath, "tickets"))
				if len(entries) != 0 {
					t.Errorf("expected no files written, found %d", len(entries))
				}
			},
		},
		{
			name:         "empty worktree path",
			ticket:       &board.Ticket{BranchName: "task/foo", Description: "x"},
			useWorktree:  false,
			wantPath:     "",
			wantHasBrief: false,
		},
		{
			name:         "branch task/foo blank desc absent file",
			ticket:       &board.Ticket{BranchName: "task/foo", Description: ""},
			useWorktree:  true,
			wantPath:     "",
			wantHasBrief: false,
			check: func(t *testing.T, worktreePath string) {
				if _, err := os.Stat(filepath.Join(worktreePath, "tickets", "foo.md")); !os.IsNotExist(err) {
					t.Errorf("expected file not created, got err=%v", err)
				}
			},
		},
		{
			name: "branch task/foo desc set absent file creates brief",
			ticket: &board.Ticket{
				Title:       "Foo Title",
				BranchName:  "task/foo",
				Description: "implement foo",
			},
			useWorktree:  true,
			wantPath:     "tickets/foo.md",
			wantHasBrief: true,
			check: func(t *testing.T, worktreePath string) {
				b, err := os.ReadFile(filepath.Join(worktreePath, "tickets", "foo.md"))
				if err != nil {
					t.Fatalf("expected brief file, got err=%v", err)
				}
				s := string(b)
				wantSubs := []string{"# Foo Title", briefBlockStart, "implement foo", briefBlockEnd}
				for _, w := range wantSubs {
					if !strings.Contains(s, w) {
						t.Errorf("missing %q in:\n%s", w, s)
					}
				}
			},
		},
		{
			name: "branch task/foo blank desc preexisting file unchanged",
			ticket: &board.Ticket{
				Title:       "Foo",
				BranchName:  "task/foo",
				Description: "",
			},
			useWorktree: true,
			pre: &pre{
				relPath: "tickets/foo.md",
				content: "## Brief\nstuff\n",
			},
			wantPath:     "tickets/foo.md",
			wantHasBrief: true,
			check: func(t *testing.T, worktreePath string) {
				b, err := os.ReadFile(filepath.Join(worktreePath, "tickets", "foo.md"))
				if err != nil {
					t.Fatal(err)
				}
				if string(b) != "## Brief\nstuff\n" {
					t.Errorf("file was modified: %q", string(b))
				}
			},
		},
		{
			name: "preexisting without block appends",
			ticket: &board.Ticket{
				BranchName:  "task/foo",
				Description: "new notes",
			},
			useWorktree: true,
			pre: &pre{
				relPath: "tickets/foo.md",
				content: "# Existing\n\nbody text\n",
			},
			wantPath:     "tickets/foo.md",
			wantHasBrief: true,
			check: func(t *testing.T, worktreePath string) {
				b, err := os.ReadFile(filepath.Join(worktreePath, "tickets", "foo.md"))
				if err != nil {
					t.Fatal(err)
				}
				s := string(b)
				if !strings.HasPrefix(s, "# Existing\n\nbody text\n") {
					t.Errorf("original content not preserved at start: %q", s)
				}
				for _, w := range []string{briefBlockStart, "new notes", briefBlockEnd} {
					if !strings.Contains(s, w) {
						t.Errorf("missing %q in:\n%s", w, s)
					}
				}
			},
		},
		{
			name: "preexisting block replaced",
			ticket: &board.Ticket{
				BranchName:  "task/foo",
				Description: "new notes",
			},
			useWorktree: true,
			pre: &pre{
				relPath: "tickets/foo.md",
				content: "# Header\n\n" + briefBlockStart + "\n" + briefBlockTitle + "\n\nold notes\n" + briefBlockEnd + "\n",
			},
			wantPath:     "tickets/foo.md",
			wantHasBrief: true,
			check: func(t *testing.T, worktreePath string) {
				b, err := os.ReadFile(filepath.Join(worktreePath, "tickets", "foo.md"))
				if err != nil {
					t.Fatal(err)
				}
				s := string(b)
				if strings.Contains(s, "old notes") {
					t.Errorf("old notes still present:\n%s", s)
				}
				if !strings.Contains(s, "new notes") {
					t.Errorf("new notes not present:\n%s", s)
				}
				if !strings.Contains(s, "# Header") {
					t.Errorf("header lost:\n%s", s)
				}
			},
		},
		{
			name: "whitespace-only desc absent file no write",
			ticket: &board.Ticket{
				BranchName:  "task/foo",
				Description: "   \n\t\n  ",
			},
			useWorktree:  true,
			wantPath:     "",
			wantHasBrief: false,
			check: func(t *testing.T, worktreePath string) {
				if _, err := os.Stat(filepath.Join(worktreePath, "tickets", "foo.md")); !os.IsNotExist(err) {
					t.Errorf("expected no file, got err=%v", err)
				}
			},
		},
		{
			name: "task slash no slug",
			ticket: &board.Ticket{
				BranchName:  "task/",
				Description: "x",
			},
			useWorktree:  true,
			wantPath:     "",
			wantHasBrief: false,
		},
		{
			name: "feature branch as-is",
			ticket: &board.Ticket{
				BranchName:  "feature/foo",
				Description: "x",
			},
			useWorktree:  true,
			wantPath:     "tickets/feature/foo.md",
			wantHasBrief: true,
			check: func(t *testing.T, worktreePath string) {
				if _, err := os.Stat(filepath.Join(worktreePath, "tickets", "feature", "foo.md")); err != nil {
					t.Errorf("expected nested brief file, got err=%v", err)
				}
			},
		},
		{
			name: "missing parent dir created",
			ticket: &board.Ticket{
				BranchName:  "task/foo",
				Description: "hello",
			},
			useWorktree:  true,
			wantPath:     "tickets/foo.md",
			wantHasBrief: true,
			check: func(t *testing.T, worktreePath string) {
				info, err := os.Stat(filepath.Join(worktreePath, "tickets"))
				if err != nil {
					t.Fatalf("expected tickets dir created: %v", err)
				}
				if !info.IsDir() {
					t.Errorf("tickets is not a dir")
				}
			},
		},
		{
			name: "mid-file block preserves header and footer",
			ticket: &board.Ticket{
				BranchName:  "task/foo",
				Description: "NEW",
			},
			useWorktree: true,
			pre: &pre{
				relPath: "tickets/foo.md",
				content: "## Header\nhead body\n\n" + briefBlockStart + "\n" + briefBlockTitle + "\n\nOLD\n" + briefBlockEnd + "\n\n## Footer\nlast\n",
			},
			wantPath:     "tickets/foo.md",
			wantHasBrief: true,
			check: func(t *testing.T, worktreePath string) {
				b, err := os.ReadFile(filepath.Join(worktreePath, "tickets", "foo.md"))
				if err != nil {
					t.Fatal(err)
				}
				s := string(b)
				if !strings.Contains(s, "## Header") || !strings.Contains(s, "head body") {
					t.Errorf("header section lost:\n%s", s)
				}
				if !strings.Contains(s, "## Footer") || !strings.Contains(s, "last") {
					t.Errorf("footer section lost:\n%s", s)
				}
				if strings.Contains(s, "OLD") {
					t.Errorf("OLD still present:\n%s", s)
				}
				if !strings.Contains(s, "NEW") {
					t.Errorf("NEW missing:\n%s", s)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			worktreePath := t.TempDir()
			passPath := worktreePath
			if !tt.useWorktree {
				passPath = ""
			}
			if tt.pre != nil {
				writeFile(t, filepath.Join(worktreePath, tt.pre.relPath), tt.pre.content)
			}

			gotPath, gotHas, err := MergeTicketBrief(tt.ticket, passPath)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if gotPath != tt.wantPath {
				t.Errorf("path = %q, want %q", gotPath, tt.wantPath)
			}
			if gotHas != tt.wantHasBrief {
				t.Errorf("hasBrief = %v, want %v", gotHas, tt.wantHasBrief)
			}
			if tt.check != nil {
				tt.check(t, worktreePath)
			}
		})
	}
}

func TestMergeTicketBrief_Idempotent(t *testing.T) {
	worktreePath := t.TempDir()
	ticket := &board.Ticket{
		BranchName:  "task/foo",
		Description: "new notes",
	}
	pre := "# Header\n\n" + briefBlockStart + "\n" + briefBlockTitle + "\n\nold notes\n" + briefBlockEnd + "\n"
	writeFile(t, filepath.Join(worktreePath, "tickets", "foo.md"), pre)

	if _, _, err := MergeTicketBrief(ticket, worktreePath); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(filepath.Join(worktreePath, "tickets", "foo.md"))
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := MergeTicketBrief(ticket, worktreePath); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(filepath.Join(worktreePath, "tickets", "foo.md"))
	if err != nil {
		t.Fatal(err)
	}

	if string(first) != string(second) {
		t.Errorf("not idempotent:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestPreviewBriefMerge(t *testing.T) {
	type pre struct {
		relPath string
		content string
	}

	tests := []struct {
		name            string
		ticket          *board.Ticket
		useWorktree     bool // false => pass "" for worktreePath
		pre             *pre
		wantPath        string
		wantHasBrief    bool
		wantWouldChange bool
		wantErr         bool
		// optional checks on the returned content
		wantContains    []string
		wantNotContains []string
		// if non-empty, returned content must equal this exactly
		wantContentEq string
	}{
		{
			name:            "nil ticket",
			ticket:          nil,
			useWorktree:     true,
			wantPath:        "",
			wantHasBrief:    false,
			wantWouldChange: false,
		},
		{
			name:            "empty worktree path",
			ticket:          &board.Ticket{BranchName: "task/foo", Description: "x"},
			useWorktree:     false,
			wantPath:        "",
			wantHasBrief:    false,
			wantWouldChange: false,
		},
		{
			name:            "branch task/foo blank desc absent file",
			ticket:          &board.Ticket{BranchName: "task/foo", Description: ""},
			useWorktree:     true,
			wantPath:        "",
			wantHasBrief:    false,
			wantWouldChange: false,
		},
		{
			name: "branch task/foo desc set absent file",
			ticket: &board.Ticket{
				Title:       "Foo",
				BranchName:  "task/foo",
				Description: "implement foo",
			},
			useWorktree:     true,
			wantPath:        "tickets/foo.md",
			wantHasBrief:    true,
			wantWouldChange: true,
			wantContains:    []string{"# Foo", "implement foo"},
		},
		{
			name: "blank desc preexisting file",
			ticket: &board.Ticket{
				Title:       "Foo",
				BranchName:  "task/foo",
				Description: "",
			},
			useWorktree: true,
			pre: &pre{
				relPath: "tickets/foo.md",
				content: "## Brief\nstuff\n",
			},
			wantPath:        "tickets/foo.md",
			wantHasBrief:    true,
			wantWouldChange: false,
			wantContentEq:   "## Brief\nstuff\n",
		},
		{
			name: "preexisting block replaced",
			ticket: &board.Ticket{
				Title:       "Foo",
				BranchName:  "task/foo",
				Description: "new",
			},
			useWorktree: true,
			pre: &pre{
				relPath: "tickets/foo.md",
				content: "# Header\n\n" + briefBlockStart + "\n" + briefBlockTitle + "\n\nold\n" + briefBlockEnd + "\n",
			},
			wantPath:        "tickets/foo.md",
			wantHasBrief:    true,
			wantWouldChange: true,
			wantContains:    []string{"new"},
			wantNotContains: []string{"old"},
		},
		{
			name: "already in sync no change",
			ticket: &board.Ticket{
				Title:       "Foo",
				BranchName:  "task/foo",
				Description: "same",
			},
			useWorktree: true,
			pre: &pre{
				relPath: "tickets/foo.md",
				content: "# Header\n\n" + briefBlockStart + "\n" + briefBlockTitle + "\n\nsame\n" + briefBlockEnd + "\n",
			},
			wantPath:        "tickets/foo.md",
			wantHasBrief:    true,
			wantWouldChange: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			worktreePath := t.TempDir()
			passPath := worktreePath
			if !tt.useWorktree {
				passPath = ""
			}
			if tt.pre != nil {
				writeFile(t, filepath.Join(worktreePath, tt.pre.relPath), tt.pre.content)
			}

			gotPath, gotHas, gotWould, gotContent, err := PreviewBriefMerge(tt.ticket, passPath)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if gotPath != tt.wantPath {
				t.Errorf("path = %q, want %q", gotPath, tt.wantPath)
			}
			if gotHas != tt.wantHasBrief {
				t.Errorf("hasBrief = %v, want %v", gotHas, tt.wantHasBrief)
			}
			if gotWould != tt.wantWouldChange {
				t.Errorf("wouldChange = %v, want %v", gotWould, tt.wantWouldChange)
			}
			for _, w := range tt.wantContains {
				if !strings.Contains(gotContent, w) {
					t.Errorf("missing %q in content:\n%s", w, gotContent)
				}
			}
			for _, w := range tt.wantNotContains {
				if strings.Contains(gotContent, w) {
					t.Errorf("unexpected %q in content:\n%s", w, gotContent)
				}
			}
			if tt.wantContentEq != "" && gotContent != tt.wantContentEq {
				t.Errorf("content = %q, want %q", gotContent, tt.wantContentEq)
			}

			// Verify read-only: PreviewBriefMerge must never create files.
			// Only check this when there was no pre-existing file written by the harness.
			if tt.useWorktree && tt.pre == nil && tt.ticket != nil {
				slug := BranchSlug(tt.ticket.BranchName)
				if slug != "" {
					if _, err := os.Stat(filepath.Join(worktreePath, "tickets", slug+".md")); !os.IsNotExist(err) {
						t.Errorf("PreviewBriefMerge wrote to disk: stat err = %v", err)
					}
				}
			}
		})
	}

	t.Run("idempotency", func(t *testing.T) {
		worktreePath := t.TempDir()
		ticket := &board.Ticket{
			Title:       "Foo",
			BranchName:  "task/foo",
			Description: "new notes",
		}
		// Establish initial state by performing the merge for real.
		if _, _, err := MergeTicketBrief(ticket, worktreePath); err != nil {
			t.Fatalf("initial MergeTicketBrief: %v", err)
		}
		// A second preview with identical inputs must report no change.
		_, hasBrief, wouldChange, _, err := PreviewBriefMerge(ticket, worktreePath)
		if err != nil {
			t.Fatalf("PreviewBriefMerge: %v", err)
		}
		if !hasBrief {
			t.Errorf("hasBrief = false, want true")
		}
		if wouldChange {
			t.Errorf("wouldChange = true, want false (already in sync)")
		}
	})
}

func TestUpsertManagedBlock(t *testing.T) {
	tests := []struct {
		name     string
		existing string
		desc     string
		wantSubs []string
		wantNot  []string
	}{
		{
			name:     "preexisting block replaced",
			existing: "head\n\n" + briefBlockStart + "\nfoo\n" + briefBlockEnd + "\ntail\n",
			desc:     "NEW",
			wantSubs: []string{"head", "tail", briefBlockStart, "NEW", briefBlockEnd},
			wantNot:  []string{"foo"},
		},
		{
			name:     "no block appended",
			existing: "# Title\n\nbody\n",
			desc:     "added",
			wantSubs: []string{"# Title", "body", briefBlockStart, "added", briefBlockEnd},
		},
		{
			name:     "empty input yields just the block",
			existing: "",
			desc:     "x",
			wantSubs: []string{briefBlockStart, briefBlockTitle, "x", briefBlockEnd},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := upsertManagedBlock(tt.existing, tt.desc)
			for _, w := range tt.wantSubs {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in:\n%s", w, got)
				}
			}
			for _, w := range tt.wantNot {
				if strings.Contains(got, w) {
					t.Errorf("unexpected %q in:\n%s", w, got)
				}
			}
			if tt.name == "empty input yields just the block" {
				if !strings.HasPrefix(got, briefBlockStart) {
					t.Errorf("expected block at start for empty input, got:\n%s", got)
				}
			}
		})
	}
}

// TestMergeTicketBrief_AtomicRename is the one check that distinguishes the
// atomic temp+rename write from a non-atomic in-place os.WriteFile. A rename
// swaps the destination inode; a truncate-in-place keeps it. Reverting the
// write to os.WriteFile makes this test fail; the other invariant tests below
// pass under both implementations, so this is the real red-before-green guard.
func TestMergeTicketBrief_AtomicRename(t *testing.T) {
	wt := t.TempDir()
	ticket := &board.Ticket{Title: "Foo", BranchName: "task/foo", Description: "first"}
	if _, _, err := MergeTicketBrief(ticket, wt); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	path := filepath.Join(wt, "tickets", "foo.md")

	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	inoBefore := before.Sys().(*syscall.Stat_t).Ino

	// Materially change the description so wouldChange == true and a real
	// write happens on the second call.
	ticket.Description = "second, materially different notes"
	if _, _, err := MergeTicketBrief(ticket, wt); err != nil {
		t.Fatalf("update write: %v", err)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	inoAfter := after.Sys().(*syscall.Stat_t).Ino

	if inoAfter == inoBefore {
		t.Errorf("brief inode unchanged (%d) — write was not atomic temp+rename", inoBefore)
	}

	// The destination must always hold the complete new content, never a
	// partial brief.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); !strings.Contains(s, "second, materially different notes") || !strings.Contains(s, briefBlockEnd) {
		t.Errorf("brief not fully written:\n%s", s)
	}
}

// TestMergeTicketBrief_FileMode pins the 0644 mode. os.CreateTemp yields 0600,
// so this guards the explicit Chmod in the atomic write path. (Passes under the
// old os.WriteFile(...,0o644) path too — it is an implementation guard, not a
// new-vs-old discriminator.)
func TestMergeTicketBrief_FileMode(t *testing.T) {
	wt := t.TempDir()
	ticket := &board.Ticket{Title: "Foo", BranchName: "task/foo", Description: "notes"}
	if _, _, err := MergeTicketBrief(ticket, wt); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(wt, "tickets", "foo.md"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("brief mode = %o, want 644", got)
	}
}

// TestMergeTicketBrief_NoTempResidue ensures the temp file is renamed (or
// cleaned up), never left behind in the brief dir.
func TestMergeTicketBrief_NoTempResidue(t *testing.T) {
	wt := t.TempDir()
	ticket := &board.Ticket{Title: "Foo", BranchName: "task/foo", Description: "notes"}
	if _, _, err := MergeTicketBrief(ticket, wt); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(wt, "tickets"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("leftover temp file in brief dir: %s", e.Name())
		}
	}
}
