package agent

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/techdufus/openkanban/internal/board"
)

func TestMergeSettingsLocal(t *testing.T) {
	tests := []struct {
		name      string
		dst       map[string]any
		src       map[string]any
		wantDst   map[string]any
		wantAdded []string
	}{
		{
			name:      "empty dst gains all src",
			dst:       map[string]any{},
			src:       perms(map[string][]string{"allow": {"A", "B"}}),
			wantDst:   perms(map[string][]string{"allow": {"A", "B"}}),
			wantAdded: []string{"A", "B"},
		},
		{
			name:      "no duplicates and existing order preserved",
			dst:       perms(map[string][]string{"allow": {"A", "B"}}),
			src:       perms(map[string][]string{"allow": {"B", "C", "A"}}),
			wantDst:   perms(map[string][]string{"allow": {"A", "B", "C"}}),
			wantAdded: []string{"C"},
		},
		{
			name:      "populated dst, empty src is no-op",
			dst:       perms(map[string][]string{"allow": {"A"}}),
			src:       map[string]any{},
			wantDst:   perms(map[string][]string{"allow": {"A"}}),
			wantAdded: nil,
		},
		{
			name:      "ask and deny buckets merge too",
			dst:       perms(map[string][]string{"allow": {"A"}, "ask": {"B"}}),
			src:       perms(map[string][]string{"allow": {"A2"}, "ask": {"B2"}, "deny": {"D1"}}),
			wantDst:   perms(map[string][]string{"allow": {"A", "A2"}, "ask": {"B", "B2"}, "deny": {"D1"}}),
			wantAdded: []string{"A2", "B2", "D1"},
		},
		{
			name: "non-permissions keys in dst untouched",
			dst: map[string]any{
				"hooks":       "something",
				"permissions": map[string]any{"allow": []any{"A"}},
			},
			src: perms(map[string][]string{"allow": {"B"}}),
			wantDst: map[string]any{
				"hooks":       "something",
				"permissions": map[string]any{"allow": []any{"A", "B"}},
			},
			wantAdded: []string{"B"},
		},
		{
			name:      "nil src is no-op",
			dst:       perms(map[string][]string{"allow": {"A"}}),
			src:       nil,
			wantDst:   perms(map[string][]string{"allow": {"A"}}),
			wantAdded: nil,
		},
		{
			name:      "nil dst becomes empty + receives src",
			dst:       nil,
			src:       perms(map[string][]string{"allow": {"A"}}),
			wantDst:   perms(map[string][]string{"allow": {"A"}}),
			wantAdded: []string{"A"},
		},
		{
			name:      "src with no permissions key is no-op",
			dst:       perms(map[string][]string{"allow": {"A"}}),
			src:       map[string]any{"hooks": "x"},
			wantDst:   perms(map[string][]string{"allow": {"A"}}),
			wantAdded: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDst, gotAdded := mergeSettingsLocal(tt.dst, tt.src)
			if !reflect.DeepEqual(gotDst, tt.wantDst) {
				t.Errorf("dst:\ngot:  %s\nwant: %s", mustJSON(gotDst), mustJSON(tt.wantDst))
			}
			if !stringSliceEqual(gotAdded, tt.wantAdded) {
				t.Errorf("added: got %v, want %v", gotAdded, tt.wantAdded)
			}
		})
	}
}

func TestSeedClaudeSettings(t *testing.T) {
	t.Run("populated repo + empty worktree", func(t *testing.T) {
		repo, work := setupRepoAndWorktree(t)
		writeJSON(t, filepath.Join(repo, ".claude", "settings.local.json"),
			perms(map[string][]string{"allow": {"Bash(go test *)"}}))
		if err := SeedClaudeSettings(work, repo); err != nil {
			t.Fatalf("seed: %v", err)
		}
		got := readJSON(t, filepath.Join(work, ".claude", "settings.local.json"))
		want := perms(map[string][]string{"allow": {"Bash(go test *)"}})
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %s, want %s", mustJSON(got), mustJSON(want))
		}
	})

	t.Run("idempotent across two calls", func(t *testing.T) {
		repo, work := setupRepoAndWorktree(t)
		writeJSON(t, filepath.Join(repo, ".claude", "settings.local.json"),
			perms(map[string][]string{"allow": {"A", "B"}}))
		writeJSON(t, filepath.Join(work, ".claude", "settings.local.json"),
			perms(map[string][]string{"allow": {"A"}}))
		if err := SeedClaudeSettings(work, repo); err != nil {
			t.Fatal(err)
		}
		if err := SeedClaudeSettings(work, repo); err != nil {
			t.Fatal(err)
		}
		got := readJSON(t, filepath.Join(work, ".claude", "settings.local.json"))
		want := perms(map[string][]string{"allow": {"A", "B"}})
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %s, want %s", mustJSON(got), mustJSON(want))
		}
	})

	t.Run("inner .gitignore created when root does not ignore .claude", func(t *testing.T) {
		repo, work := setupRepoAndWorktree(t)
		if err := SeedClaudeSettings(work, repo); err != nil {
			t.Fatal(err)
		}
		gi, err := os.ReadFile(filepath.Join(repo, ".claude", ".gitignore"))
		if err != nil {
			t.Fatalf("inner .gitignore not created: %v", err)
		}
		if !strings.Contains(string(gi), "settings.local.json") {
			t.Errorf("inner .gitignore missing settings.local.json: %q", gi)
		}
	})

	t.Run("inner .gitignore NOT created when root ignores .claude", func(t *testing.T) {
		repo, work := setupRepoAndWorktree(t)
		if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".claude/\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := SeedClaudeSettings(work, repo); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(repo, ".claude", ".gitignore")); err == nil {
			t.Errorf("inner .gitignore should not have been created")
		}
	})

	t.Run("worktree-only entries preserved", func(t *testing.T) {
		repo, work := setupRepoAndWorktree(t)
		writeJSON(t, filepath.Join(repo, ".claude", "settings.local.json"),
			perms(map[string][]string{"allow": {"A"}}))
		writeJSON(t, filepath.Join(work, ".claude", "settings.local.json"),
			perms(map[string][]string{"allow": {"X"}}))
		if err := SeedClaudeSettings(work, repo); err != nil {
			t.Fatal(err)
		}
		got := readJSON(t, filepath.Join(work, ".claude", "settings.local.json"))
		want := perms(map[string][]string{"allow": {"X", "A"}})
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %s, want %s", mustJSON(got), mustJSON(want))
		}
	})

	t.Run("no-op when paths equal", func(t *testing.T) {
		repo, _ := setupRepoAndWorktree(t)
		if err := SeedClaudeSettings(repo, repo); err != nil {
			t.Fatalf("seed: %v", err)
		}
	})

	t.Run("no-op when worktree path empty", func(t *testing.T) {
		repo, _ := setupRepoAndWorktree(t)
		if err := SeedClaudeSettings("", repo); err != nil {
			t.Fatalf("seed: %v", err)
		}
	})
}

func TestPromoteClaudeSettings(t *testing.T) {
	t.Run("worktree-only entries promoted to repo", func(t *testing.T) {
		repo, work := setupRepoAndWorktree(t)
		writeJSON(t, filepath.Join(work, ".claude", "settings.local.json"),
			perms(map[string][]string{"allow": {"Bash(ls *)"}}))
		added, err := PromoteClaudeSettings(work, repo)
		if err != nil {
			t.Fatal(err)
		}
		if !stringSliceEqual(added, []string{"Bash(ls *)"}) {
			t.Errorf("added: %v", added)
		}
		got := readJSON(t, filepath.Join(repo, ".claude", "settings.local.json"))
		want := perms(map[string][]string{"allow": {"Bash(ls *)"}})
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %s, want %s", mustJSON(got), mustJSON(want))
		}
	})

	t.Run("nothing to promote returns empty added", func(t *testing.T) {
		repo, work := setupRepoAndWorktree(t)
		writeJSON(t, filepath.Join(repo, ".claude", "settings.local.json"),
			perms(map[string][]string{"allow": {"A"}}))
		writeJSON(t, filepath.Join(work, ".claude", "settings.local.json"),
			perms(map[string][]string{"allow": {"A"}}))
		added, err := PromoteClaudeSettings(work, repo)
		if err != nil {
			t.Fatal(err)
		}
		if len(added) != 0 {
			t.Errorf("expected no entries, got %v", added)
		}
	})

	t.Run("idempotent second promote", func(t *testing.T) {
		repo, work := setupRepoAndWorktree(t)
		writeJSON(t, filepath.Join(work, ".claude", "settings.local.json"),
			perms(map[string][]string{"allow": {"X"}}))
		if _, err := PromoteClaudeSettings(work, repo); err != nil {
			t.Fatal(err)
		}
		added, err := PromoteClaudeSettings(work, repo)
		if err != nil {
			t.Fatal(err)
		}
		if len(added) != 0 {
			t.Errorf("expected empty added on second promote, got %v", added)
		}
	})

	t.Run("malformed worktree JSON returns error and does not write repo", func(t *testing.T) {
		repo, work := setupRepoAndWorktree(t)
		if err := os.MkdirAll(filepath.Join(work, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(
			filepath.Join(work, ".claude", "settings.local.json"),
			[]byte("not json"), 0o644,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := PromoteClaudeSettings(work, repo); err == nil {
			t.Errorf("expected error from malformed JSON")
		}
		if _, err := os.Stat(filepath.Join(repo, ".claude", "settings.local.json")); err == nil {
			t.Errorf("repo file should not have been written")
		}
	})

	t.Run("worktree file missing is silent no-op", func(t *testing.T) {
		repo, work := setupRepoAndWorktree(t)
		added, err := PromoteClaudeSettings(work, repo)
		if err != nil {
			t.Fatal(err)
		}
		if len(added) != 0 {
			t.Errorf("expected empty added, got %v", added)
		}
	})
}

func TestPromoteClaudeSettingsOnTransition(t *testing.T) {
	t.Run("in_progress → in_review promotes", func(t *testing.T) {
		repo, work := setupRepoAndWorktree(t)
		writeJSON(t, filepath.Join(work, ".claude", "settings.local.json"),
			perms(map[string][]string{"allow": {"X"}}))
		added, err := PromoteClaudeSettingsOnTransition(work, repo, board.StatusInProgress, board.StatusInReview)
		if err != nil {
			t.Fatal(err)
		}
		if !stringSliceEqual(added, []string{"X"}) {
			t.Errorf("added: %v", added)
		}
	})
	t.Run("backlog → done promotes", func(t *testing.T) {
		repo, work := setupRepoAndWorktree(t)
		writeJSON(t, filepath.Join(work, ".claude", "settings.local.json"),
			perms(map[string][]string{"allow": {"Y"}}))
		added, err := PromoteClaudeSettingsOnTransition(work, repo, board.StatusBacklog, board.StatusDone)
		if err != nil {
			t.Fatal(err)
		}
		if !stringSliceEqual(added, []string{"Y"}) {
			t.Errorf("added: %v", added)
		}
	})
	t.Run("same-status is no-op", func(t *testing.T) {
		repo, work := setupRepoAndWorktree(t)
		writeJSON(t, filepath.Join(work, ".claude", "settings.local.json"),
			perms(map[string][]string{"allow": {"X"}}))
		added, err := PromoteClaudeSettingsOnTransition(work, repo, board.StatusInReview, board.StatusInReview)
		if err != nil {
			t.Fatal(err)
		}
		if len(added) != 0 {
			t.Errorf("expected no-op, got %v", added)
		}
		if _, err := os.Stat(filepath.Join(repo, ".claude", "settings.local.json")); err == nil {
			t.Errorf("repo file should not have been written")
		}
	})
	t.Run("→ in_progress is no-op", func(t *testing.T) {
		repo, work := setupRepoAndWorktree(t)
		writeJSON(t, filepath.Join(work, ".claude", "settings.local.json"),
			perms(map[string][]string{"allow": {"X"}}))
		added, err := PromoteClaudeSettingsOnTransition(work, repo, board.StatusBacklog, board.StatusInProgress)
		if err != nil {
			t.Fatal(err)
		}
		if len(added) != 0 {
			t.Errorf("expected no-op, got %v", added)
		}
		if _, err := os.Stat(filepath.Join(repo, ".claude", "settings.local.json")); err == nil {
			t.Errorf("repo file should not have been written")
		}
	})
	t.Run("→ backlog is no-op", func(t *testing.T) {
		repo, work := setupRepoAndWorktree(t)
		writeJSON(t, filepath.Join(work, ".claude", "settings.local.json"),
			perms(map[string][]string{"allow": {"X"}}))
		added, err := PromoteClaudeSettingsOnTransition(work, repo, board.StatusInProgress, board.StatusBacklog)
		if err != nil {
			t.Fatal(err)
		}
		if len(added) != 0 {
			t.Errorf("expected no-op, got %v", added)
		}
	})
	t.Run("empty worktree path is no-op", func(t *testing.T) {
		repo, _ := setupRepoAndWorktree(t)
		added, err := PromoteClaudeSettingsOnTransition("", repo, board.StatusInProgress, board.StatusDone)
		if err != nil {
			t.Fatal(err)
		}
		if len(added) != 0 {
			t.Errorf("expected no-op, got %v", added)
		}
	})
}

func TestSeedPromoteRoundTrip(t *testing.T) {
	repo, work := setupRepoAndWorktree(t)
	writeJSON(t, filepath.Join(repo, ".claude", "settings.local.json"),
		perms(map[string][]string{"allow": {"A"}}))
	if err := SeedClaudeSettings(work, repo); err != nil {
		t.Fatal(err)
	}

	// Simulate the user approving B during a worktree-scoped Claude session.
	wt := readJSON(t, filepath.Join(work, ".claude", "settings.local.json"))
	wtPerms := wt["permissions"].(map[string]any)
	wtPerms["allow"] = append(wtPerms["allow"].([]any), "B")
	writeJSON(t, filepath.Join(work, ".claude", "settings.local.json"), wt)

	added, err := PromoteClaudeSettings(work, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !stringSliceEqual(added, []string{"B"}) {
		t.Errorf("added: %v", added)
	}
	got := readJSON(t, filepath.Join(repo, ".claude", "settings.local.json"))
	want := perms(map[string][]string{"allow": {"A", "B"}})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %s, want %s", mustJSON(got), mustJSON(want))
	}

	// Second promote with no new entries must be a no-op.
	added2, err := PromoteClaudeSettings(work, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(added2) != 0 {
		t.Errorf("expected empty added on second promote, got %v", added2)
	}
}

// setupRepoAndWorktree creates a TempDir-scoped pair: a `repo` directory
// initialized as a git repo (so check-ignore has a real stack to query)
// and a `work` directory that stands in for an openkanban worktree.
//
// HOME and XDG_CONFIG_HOME are redirected to TempDir so the user's
// global git config and core.excludesFile (~/.config/git/ignore) do
// not leak rules into tests that assert ignore-rule behavior. The
// production check-ignore call is supposed to respect those globals;
// only the tests need isolation.
func setupRepoAndWorktree(t *testing.T) (string, string) {
	t.Helper()
	isolation := t.TempDir()
	t.Setenv("HOME", isolation)
	t.Setenv("XDG_CONFIG_HOME", isolation)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(isolation, ".gitconfig-absent"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(isolation, ".gitconfig-system-absent"))
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "init", "-q")
	return repo, work
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func mustJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "<unmarshal-fail>"
	}
	return string(data)
}

func perms(buckets map[string][]string) map[string]any {
	p := map[string]any{}
	for k, vs := range buckets {
		arr := make([]any, 0, len(vs))
		for _, v := range vs {
			arr = append(arr, v)
		}
		p[k] = arr
	}
	return map[string]any{"permissions": p}
}

func stringSliceEqual(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
