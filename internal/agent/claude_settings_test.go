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

// ----- review-and-prune tests -----

// pruneCase pins the expected verdict for a single permissions.allow
// entry. Used by TestReviewAndPrune table.
type pruneCase struct {
	name   string
	entry  string
	reason string // "" means keep
}

func TestReviewAndPrune(t *testing.T) {
	// Resolve home for path-allowlist cases that bake in the absolute
	// form. If HOME isn't available, those rows degrade to "untrusted"
	// outcomes — captured in the test row's expected reason via the
	// fallback comment.
	home, _ := os.UserHomeDir()

	cases := []pruneCase{
		// (a) production entries — keep verdicts
		{name: "keep glob bash", entry: "Bash(git config *)", reason: ""},
		{name: "keep glob bash 2", entry: "Bash(git fetch *)", reason: ""},
		{name: "keep go-test glob", entry: "Bash(go test *)", reason: ""},
		{name: "keep find glob", entry: "Bash(find *)", reason: ""},
		{name: "keep bare cat", entry: "Bash(cat)", reason: ""},
		{name: "keep skill entry", entry: "Skill(oh-my-claude:prime)", reason: ""},
		{name: "keep read glob", entry: "Read(//Users/cmeid/.cache/openkanban/**)", reason: ""},
		{name: "keep agent entry", entry: "Agent(Explore)", reason: ""},

		// (a) production entries — prune verdicts
		{name: "prune xargs-stat", entry: `Bash(xargs -I{} stat -f "%Sm %N" -t '%H:%M' {})`, reason: "long-no-glob"},
		{name: "prune awk-timestamp", entry: `Bash(awk '/2026-06-16T1[0-1]:[34][0-9]/' ~/.cache/openkanban/daemon.log)`, reason: "untrusted-path"},
		{name: "prune xargs-rg-escapes", entry: `Bash(xargs rg -l "DeleteProject\\\\|app\\\\\\\\.Delete")`, reason: "escape-soup"},
		{name: "prune grep-escapes", entry: `Bash(grep -E "\\.\\(go|md\\)$")`, reason: "escape-soup"},

		// (d) hard-deny patterns
		{name: "hard-deny git push glob", entry: "Bash(git push *)", reason: "hard-deny"},
		{name: "hard-deny git push specific", entry: "Bash(git push origin main)", reason: "hard-deny"},
		{name: "hard-deny gh pr create", entry: "Bash(gh pr create --title foo)", reason: "hard-deny"},
		{name: "hard-deny gh pr merge", entry: "Bash(gh pr merge 22 --merge)", reason: "hard-deny"},
		{name: "hard-deny gh auth", entry: "Bash(gh auth login)", reason: "hard-deny"},
		{name: "hard-deny gh api", entry: "Bash(gh api /repos/foo)", reason: "hard-deny"},
		{name: "hard-deny op signin", entry: "Bash(op signin)", reason: "hard-deny"},
		{name: "hard-deny op item get", entry: "Bash(op item get vault Personal)", reason: "hard-deny"},
		{name: "hard-deny aws", entry: "Bash(aws s3 ls)", reason: "hard-deny"},
		{name: "hard-deny kubectl", entry: "Bash(kubectl get pods)", reason: "hard-deny"},
		{name: "hard-deny docker run", entry: "Bash(docker run alpine sh)", reason: "hard-deny"},
		{name: "hard-deny sudo", entry: "Bash(sudo rm /etc/foo)", reason: "hard-deny"},
		{name: "hard-deny chmod", entry: "Bash(chmod +x foo)", reason: "hard-deny"},
		{name: "hard-deny git remote add", entry: "Bash(git remote add upstream foo)", reason: "hard-deny"},
		{name: "hard-deny git config global", entry: "Bash(git config --global user.email a@b)", reason: "hard-deny"},

		// hard-deny path substrings
		{name: "hard-deny ssh path tilde", entry: "Bash(cat ~/.ssh/id_rsa)", reason: "hard-deny"},
		{name: "hard-deny ssh path absolute", entry: "Bash(cat " + home + "/.ssh/id_rsa)", reason: "hard-deny"},
		{name: "hard-deny aws path", entry: "Bash(cat ~/.aws/credentials)", reason: "hard-deny"},
		{name: "hard-deny gh-config", entry: "Bash(cat ~/.config/gh/hosts.yml)", reason: "hard-deny"},

		// (e) non-Bash passthrough
		{name: "passthrough skill", entry: "Skill(oh-my-claude:debugger)", reason: ""},
		{name: "passthrough read glob", entry: "Read(//tmp/**)", reason: ""},
		{name: "passthrough agent", entry: "Agent(oh-my-claude:librarian)", reason: ""},

		// (f) ./... glob exemption
		{name: "keep go test pkg-selector", entry: "Bash(go test ./...)", reason: ""},
		{name: "keep go vet pkg-selector", entry: "Bash(go vet ./...)", reason: ""},

		// (g) tilde-resolution rows — only meaningful if HOME resolves
		// well-formed; if HOME is empty (CI edge case), these would
		// otherwise prune via the fail-closed branch. Skip those rows
		// in that case.

		// untrusted-path: an absolute path outside the allowlist
		{name: "prune untrusted-path /etc", entry: "Bash(cat /etc/passwd)", reason: "untrusted-path"},
		{name: "prune untrusted-path /usr/local", entry: "Bash(ls /usr/local/bin/)", reason: "untrusted-path"},

		// short benign entries
		{name: "keep bare ls", entry: "Bash(ls)", reason: ""},
		{name: "keep ls short", entry: "Bash(ls /tmp)", reason: ""},
		{name: "keep go vet glob", entry: "Bash(go vet *)", reason: ""},

		// long-no-glob catch-all (no path token)
		{name: "prune long-no-glob no-paths", entry: "Bash(echo this is a long command without any wildcards anywhere)", reason: "long-no-glob"},
	}

	// HOME-dependent rows: separate slice so we can skip if HOME doesn't resolve.
	if home != "" {
		cases = append(cases,
			pruneCase{name: "keep tilde manifold workspace", entry: "Bash(grep -r foo " + home + "/manifold/dev/openkanban/internal/)", reason: ""},
			pruneCase{name: "keep tilde claude memory", entry: "Bash(ls " + home + "/.claude/projects/foo/)", reason: ""},
			pruneCase{name: "keep tilde cache dir", entry: "Bash(tail " + home + "/.cache/openkanban/daemon.log)", reason: ""},
		)
	}

	// Build the input perms map by aggregating every entry.
	allow := make([]any, 0, len(cases))
	for _, c := range cases {
		allow = append(allow, c.entry)
	}
	input := map[string]any{
		"permissions": map[string]any{
			"allow": allow,
		},
	}

	cleaned, pruned := reviewAndPrune(input)

	// Validate per-row verdicts against the pruned set.
	prunedByEntry := map[string]string{}
	for _, p := range pruned {
		prunedByEntry[p.Entry] = p.Reason
	}
	for _, c := range cases {
		got, wasPruned := prunedByEntry[c.entry]
		switch {
		case c.reason == "" && wasPruned:
			t.Errorf("%s: expected to KEEP %q, but pruned with reason %q", c.name, c.entry, got)
		case c.reason != "" && !wasPruned:
			t.Errorf("%s: expected to PRUNE %q with reason %q, but kept", c.name, c.entry, c.reason)
		case c.reason != "" && got != c.reason:
			t.Errorf("%s: pruned %q with reason %q, want %q", c.name, c.entry, got, c.reason)
		}
	}

	// (c) Table-driven idempotency: second prune produces no further changes.
	_, prunedAgain := reviewAndPrune(cleaned)
	if len(prunedAgain) != 0 {
		t.Errorf("idempotency violation: second prune produced %d records, want 0", len(prunedAgain))
		for _, r := range prunedAgain {
			t.Logf("  unexpectedly pruned on second pass: %s (%s)", r.Entry, r.Reason)
		}
	}
}

func TestReviewAndPrune_NilAndEmpty(t *testing.T) {
	// nil map
	cleaned, pruned := reviewAndPrune(nil)
	if cleaned != nil || len(pruned) != 0 {
		t.Errorf("nil input: got (%v, %v), want (nil, nil)", cleaned, pruned)
	}
	// no permissions key
	in := map[string]any{"other": "value"}
	cleaned, pruned = reviewAndPrune(in)
	if len(pruned) != 0 {
		t.Errorf("no permissions key: got pruned=%v, want empty", pruned)
	}
	if _, ok := cleaned["other"]; !ok {
		t.Errorf("no permissions key: other top-level key was dropped")
	}
	// empty allow
	in = map[string]any{"permissions": map[string]any{"allow": []any{}}}
	_, pruned = reviewAndPrune(in)
	if len(pruned) != 0 {
		t.Errorf("empty allow: got pruned=%v, want empty", pruned)
	}
}

func TestReviewAndPrune_PreservesAskAndDeny(t *testing.T) {
	// ask and deny buckets must be untouched — even if they contain
	// patterns that would be pruned in allow.
	in := map[string]any{
		"permissions": map[string]any{
			"allow": []any{"Bash(xargs -I{} stat -f \"%Sm %N\" -t '%H:%M' {})"}, // noise
			"ask":   []any{"Bash(git push *)"},                                  // would be hard-deny in allow
			"deny":  []any{"Bash(sudo *)"},                                       // would be hard-deny in allow
		},
	}
	cleaned, pruned := reviewAndPrune(in)
	if len(pruned) != 1 {
		t.Fatalf("expected 1 prune (from allow), got %d", len(pruned))
	}
	innerPerms := cleaned["permissions"].(map[string]any)
	if ask, _ := innerPerms["ask"].([]any); len(ask) != 1 || ask[0] != "Bash(git push *)" {
		t.Errorf("ask bucket modified: %v", ask)
	}
	if deny, _ := innerPerms["deny"].([]any); len(deny) != 1 || deny[0] != "Bash(sudo *)" {
		t.Errorf("deny bucket modified: %v", deny)
	}
}

func TestReviewAndPruneRepoSettings_IdempotentEarlyReturn(t *testing.T) {
	repo, _ := setupRepoAndWorktree(t)
	// Write a clean file (no entries that would trigger prune).
	writeJSON(t, filepath.Join(repo, ".claude", "settings.local.json"),
		perms(map[string][]string{"allow": {"Bash(git config *)", "Bash(go test *)"}}))

	// First call: no prunes expected, no side effects.
	pruned, err := ReviewAndPruneRepoSettings(repo)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if len(pruned) != 0 {
		t.Errorf("first call: expected no prunes, got %d", len(pruned))
	}

	// No snapshot file should exist.
	matches, _ := filepath.Glob(filepath.Join(repo, ".claude", snapshotPrefix+"*"))
	if len(matches) != 0 {
		t.Errorf("idempotent no-op produced snapshot files: %v", matches)
	}

	// No .pruned-log should exist (or it should be empty).
	if info, err := os.Stat(filepath.Join(repo, ".claude", prunedLogFile)); err == nil && info.Size() > 0 {
		t.Errorf(".pruned-log written on no-op call (size=%d)", info.Size())
	}

	// Second call: still no side effects.
	pruned, err = ReviewAndPruneRepoSettings(repo)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if len(pruned) != 0 {
		t.Errorf("second call: expected no prunes, got %d", len(pruned))
	}
}

func TestReviewAndPruneRepoSettings_PrunesAndAuditsAndSnapshots(t *testing.T) {
	repo, _ := setupRepoAndWorktree(t)
	// Seed with a known-noise entry plus a human-baseline entry.
	writeJSON(t, filepath.Join(repo, ".claude", "settings.local.json"),
		perms(map[string][]string{"allow": {
			"Bash(git config *)",
			`Bash(awk '/2026-06-16T1[0-1]:[34][0-9]/' /tmp/fake.log)`,
		}}))

	pruned, err := ReviewAndPruneRepoSettings(repo)
	if err != nil {
		t.Fatalf("ReviewAndPruneRepoSettings: %v", err)
	}
	if len(pruned) != 1 {
		t.Fatalf("expected 1 prune, got %d: %v", len(pruned), pruned)
	}

	// Settings file no longer contains the noise.
	got := readJSON(t, filepath.Join(repo, ".claude", "settings.local.json"))
	allow := got["permissions"].(map[string]any)["allow"].([]any)
	if len(allow) != 1 || allow[0] != "Bash(git config *)" {
		t.Errorf("post-prune allow: got %v, want [Bash(git config *)]", allow)
	}

	// .pruned-log exists with one line.
	logData, err := os.ReadFile(filepath.Join(repo, ".claude", prunedLogFile))
	if err != nil {
		t.Fatalf(".pruned-log not written: %v", err)
	}
	if !strings.Contains(string(logData), "awk '/2026-06-16T") {
		t.Errorf(".pruned-log missing noise entry: %q", logData)
	}
	if lines := strings.Count(string(logData), "\n"); lines != 1 {
		t.Errorf(".pruned-log line count: got %d, want 1", lines)
	}

	// Snapshot file exists with the pre-prune state (both entries).
	matches, _ := filepath.Glob(filepath.Join(repo, ".claude", snapshotPrefix+"*"))
	if len(matches) != 1 {
		t.Fatalf("snapshot count: got %d, want 1: %v", len(matches), matches)
	}
	snapData, _ := os.ReadFile(matches[0])
	if !strings.Contains(string(snapData), "awk '/2026-06-16T") {
		t.Errorf("snapshot doesn't contain pre-prune state: %q", snapData)
	}
}

func TestSnapshot_TightLoop_NoCollision(t *testing.T) {
	repo, _ := setupRepoAndWorktree(t)
	// Write a settings file so snapshot has something to copy.
	writeJSON(t, filepath.Join(repo, ".claude", "settings.local.json"),
		perms(map[string][]string{"allow": {"Bash(go test *)"}}))

	// Call snapshotSettings twice in tight succession.
	if err := snapshotSettings(repo); err != nil {
		t.Fatalf("first snapshot: %v", err)
	}
	if err := snapshotSettings(repo); err != nil {
		t.Fatalf("second snapshot: %v", err)
	}

	matches, _ := filepath.Glob(filepath.Join(repo, ".claude", snapshotPrefix+"*"))
	if len(matches) != 2 {
		t.Errorf("tight-loop snapshots: got %d distinct files, want 2 (nanos collision avoidance broken?): %v",
			len(matches), matches)
	}
}

func TestSnapshot_Rotation_KeepsLastThree(t *testing.T) {
	repo, _ := setupRepoAndWorktree(t)
	writeJSON(t, filepath.Join(repo, ".claude", "settings.local.json"),
		perms(map[string][]string{"allow": {"Bash(go test *)"}}))

	// Take 5 snapshots.
	for i := 0; i < 5; i++ {
		if err := snapshotSettings(repo); err != nil {
			t.Fatalf("snapshot %d: %v", i, err)
		}
	}

	matches, _ := filepath.Glob(filepath.Join(repo, ".claude", snapshotPrefix+"*"))
	if len(matches) != 3 {
		t.Errorf("rotation: got %d snapshots, want 3 (keepCount): %v", len(matches), matches)
	}
}

func TestSnapshot_NoFile_NoError(t *testing.T) {
	repo, _ := setupRepoAndWorktree(t)
	// Don't write a settings file. Snapshot should noop.
	if err := snapshotSettings(repo); err != nil {
		t.Errorf("snapshot on missing file should be noop, got: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(repo, ".claude", snapshotPrefix+"*"))
	if len(matches) != 0 {
		t.Errorf("snapshot created files when source missing: %v", matches)
	}
}

func TestAppendPrunedLog(t *testing.T) {
	repo, _ := setupRepoAndWorktree(t)
	records := []PruneRecord{
		{Entry: "Bash(git push *)", Reason: "hard-deny"},
		{Entry: "Bash(awk '/x/' /etc/passwd)", Reason: "untrusted-path"},
	}
	if err := appendPrunedLog(repo, records); err != nil {
		t.Fatalf("appendPrunedLog: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(repo, ".claude", prunedLogFile))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, "hard-deny Bash(git push *)") {
		t.Errorf("log missing record 1: %q", s)
	}
	if !strings.Contains(s, "untrusted-path Bash(awk") {
		t.Errorf("log missing record 2: %q", s)
	}
	if lines := strings.Count(s, "\n"); lines != 2 {
		t.Errorf("log line count: got %d, want 2", lines)
	}

	// Second call appends, doesn't overwrite.
	if err := appendPrunedLog(repo, records[:1]); err != nil {
		t.Fatalf("appendPrunedLog second: %v", err)
	}
	data, _ = os.ReadFile(filepath.Join(repo, ".claude", prunedLogFile))
	if lines := strings.Count(string(data), "\n"); lines != 3 {
		t.Errorf("after second append, line count: got %d, want 3", lines)
	}
}
