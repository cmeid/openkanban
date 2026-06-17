package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/techdufus/openkanban/internal/board"
)

// claudeSettingsLocalFile is the basename Claude Code uses for the
// project-local settings file (gitignored by convention).
const claudeSettingsLocalFile = "settings.local.json"

// claudeSubdir is the directory Claude Code reads settings from,
// relative to the project root.
const claudeSubdir = ".claude"

// SeedClaudeSettings merges entries from
// <repoPath>/.claude/settings.local.json into
// <worktreePath>/.claude/settings.local.json. Source-repo entries are
// added to the worktree file if absent; existing worktree entries are
// preserved. Creates directories and files as needed. Idempotent.
//
// If the source repo's gitignore stack does not already ignore
// .claude/, SeedClaudeSettings also writes a defensive
// <repoPath>/.claude/.gitignore containing settings.local.json so
// user-specific approvals cannot accidentally be committed.
//
// Either path being empty, or both being equal, is a no-op.
func SeedClaudeSettings(worktreePath, repoPath string) error {
	if worktreePath == "" || repoPath == "" || worktreePath == repoPath {
		return nil
	}
	if err := ensureRepoSettingsScaffolding(repoPath); err != nil {
		return fmt.Errorf("seed claude settings: %w", err)
	}
	src, err := readClaudeSettings(repoPath)
	if err != nil {
		return fmt.Errorf("seed claude settings: read source: %w", err)
	}
	dst, err := readClaudeSettings(worktreePath)
	if err != nil {
		return fmt.Errorf("seed claude settings: read worktree: %w", err)
	}
	merged, _ := mergeSettingsLocal(dst, src)
	if err := writeClaudeSettings(worktreePath, merged); err != nil {
		return fmt.Errorf("seed claude settings: write worktree: %w", err)
	}
	return nil
}

// PromoteClaudeSettingsOnTransition calls PromoteClaudeSettings only
// when a ticket's status transition matches the policy: oldStatus
// differs from newStatus, and newStatus is in_review or done. All
// other transitions (in_progress, backlog, same-status no-ops) are
// silent no-ops returning (nil, nil). This is the single
// policy gate for "the user has consciously moved this ticket far
// enough along the lifecycle that approvals granted during it should
// become per-repo defaults."
func PromoteClaudeSettingsOnTransition(worktreePath, repoPath string, oldStatus, newStatus board.TicketStatus) ([]string, error) {
	if oldStatus == newStatus {
		return nil, nil
	}
	if newStatus != board.StatusInReview && newStatus != board.StatusDone {
		return nil, nil
	}
	if worktreePath == "" || repoPath == "" {
		return nil, nil
	}
	return PromoteClaudeSettings(worktreePath, repoPath)
}

// PromoteClaudeSettings merges entries from
// <worktreePath>/.claude/settings.local.json into
// <repoPath>/.claude/settings.local.json. Worktree entries are added
// to the repo file if absent; existing repo entries are preserved.
// Idempotent. Returns the slice of newly-promoted entry strings for
// logging.
//
// Either path being empty, or both being equal, is a no-op
// (nil, nil). If the repo file would be unchanged, no write occurs and
// added is nil.
func PromoteClaudeSettings(worktreePath, repoPath string) ([]string, error) {
	if worktreePath == "" || repoPath == "" || worktreePath == repoPath {
		return nil, nil
	}
	src, err := readClaudeSettings(worktreePath)
	if err != nil {
		return nil, fmt.Errorf("promote claude settings: read worktree: %w", err)
	}
	if err := ensureRepoSettingsScaffolding(repoPath); err != nil {
		return nil, fmt.Errorf("promote claude settings: %w", err)
	}
	dst, err := readClaudeSettings(repoPath)
	if err != nil {
		return nil, fmt.Errorf("promote claude settings: read repo: %w", err)
	}
	merged, added := mergeSettingsLocal(dst, src)
	if len(added) == 0 {
		return nil, nil
	}
	if err := writeClaudeSettings(repoPath, merged); err != nil {
		return nil, fmt.Errorf("promote claude settings: write repo: %w", err)
	}
	return added, nil
}

// mergeSettingsLocal merges src into dst, preserving every dst entry
// and adding any permissions.{allow,ask,deny} entries from src that
// dst does not already have. Only the permissions arrays are touched;
// every other top-level key in dst is left untouched, and keys in src
// outside of permissions are ignored. The returned added slice lists
// new entries by their string form in stable per-bucket order (allow,
// then ask, then deny). dst is mutated in place and also returned.
func mergeSettingsLocal(dst, src map[string]any) (map[string]any, []string) {
	if dst == nil {
		dst = map[string]any{}
	}
	if src == nil {
		return dst, nil
	}
	srcPerms, _ := src["permissions"].(map[string]any)
	if srcPerms == nil {
		return dst, nil
	}
	dstPerms, _ := dst["permissions"].(map[string]any)
	if dstPerms == nil {
		dstPerms = map[string]any{}
	}
	var added []string
	for _, bucket := range []string{"allow", "ask", "deny"} {
		srcList, _ := srcPerms[bucket].([]any)
		if len(srcList) == 0 {
			continue
		}
		dstList, _ := dstPerms[bucket].([]any)
		seen := map[string]bool{}
		for _, e := range dstList {
			if s, ok := e.(string); ok {
				seen[s] = true
			}
		}
		for _, e := range srcList {
			s, ok := e.(string)
			if !ok || seen[s] {
				continue
			}
			seen[s] = true
			dstList = append(dstList, s)
			added = append(added, s)
		}
		if len(dstList) > 0 {
			dstPerms[bucket] = dstList
		}
	}
	if len(dstPerms) > 0 {
		dst["permissions"] = dstPerms
	}
	return dst, added
}

func readClaudeSettings(root string) (map[string]any, error) {
	path := filepath.Join(root, claudeSubdir, claudeSettingsLocalFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	out := map[string]any{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return out, nil
}

func writeClaudeSettings(root string, settings map[string]any) error {
	dir := filepath.Join(root, claudeSubdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, claudeSettingsLocalFile)
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

// ensureRepoSettingsScaffolding makes sure <repoPath>/.claude/ exists
// and, when the repo's gitignore stack doesn't already cover .claude/,
// writes <repoPath>/.claude/.gitignore covering settings.local.json
// (user-specific approvals), .pruned-log (audit log), and
// settings.local.json.bak.* (rotating snapshots). All three carry
// user-private data and must never be committed.
func ensureRepoSettingsScaffolding(repoPath string) error {
	claudeDir := filepath.Join(repoPath, claudeSubdir)
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		return err
	}
	if repoGitignoresClaudeDir(repoPath) {
		return nil
	}
	innerGitignore := filepath.Join(claudeDir, ".gitignore")
	if _, err := os.Stat(innerGitignore); err == nil {
		return nil
	}
	content := strings.Join([]string{
		claudeSettingsLocalFile,
		prunedLogFile,
		claudeSettingsLocalFile + ".bak.*",
	}, "\n") + "\n"
	return os.WriteFile(innerGitignore, []byte(content), 0o644)
}

// repoGitignoresClaudeDir asks git whether
// <repoPath>/.claude/settings.local.json would be ignored by the
// existing gitignore stack. Uses `git check-ignore` so root .gitignore,
// nested .gitignores, info/exclude, and core.excludesFile are all
// respected. Returns false on any git error (including the repo not
// being a git repo) so the defensive inner .gitignore still gets
// written.
func repoGitignoresClaudeDir(repoPath string) bool {
	probe := filepath.Join(claudeSubdir, claudeSettingsLocalFile)
	cmd := exec.Command("git", "-C", repoPath, "check-ignore", "-q", "--no-index", probe)
	return cmd.Run() == nil
}

// ----- review-and-prune machinery -----

// PruneRecord names a single allowlist entry removed by reviewAndPrune,
// tagged with the heuristic that triggered the removal. Records are
// appended to <repo>/.claude/.pruned-log so the user can audit (or
// recover) removals after the fact.
type PruneRecord struct {
	Entry  string
	Reason string // "hard-deny" | "escape-soup" | "untrusted-path" | "long-no-glob"
}

// hardDenyVerbPrefixes lists Bash verb-prefixes that ALWAYS prune,
// even if the entry would otherwise look well-formed. These collide
// with the global push-gate rule, secret-management workflows, or
// arbitrary-execution wrappers that the user must approve per-call.
// See plan ancient-sparking-dusk.md "Why prune-only and NOT
// verb-widening" for the threat model.
var hardDenyVerbPrefixes = []string{
	"git push",
	"gh pr create",
	"gh pr merge",
	"gh repo",
	"gh auth",
	"gh api",
	"git remote add",
	"git remote set-url",
	"git remote rename",
	"git config --global",
	"chmod",
	"sudo",
	"op",
	"aws",
	"kubectl",
	"docker run",
}

// hardDenyPathSubstrings lists path fragments whose presence in a
// Bash inner-arg always prunes the entry. Substrings match against
// the raw arg (no tilde resolution needed — both `~/.ssh/id_rsa` and
// `/Users/cmeid/.ssh/id_rsa` contain `/.ssh/`).
var hardDenyPathSubstrings = []string{
	"/.ssh/",
	"/.aws/",
	"/.config/gh/",
	"/.config/op/",
}

// pathAllowlistRoots lists path-prefixes where Bash entries are
// allowed to operate. Tilde entries are resolved to absolute prefixes
// at first use (memoized). Anything outside this set triggers
// untrusted-path pruning.
var pathAllowlistRoots = []string{
	"~/Documents/",
	"~/.config/openkanban/",
	"~/.cache/openkanban/",
	"~/manifold/dev/",
	"~/.claude/projects/",
	"/tmp/",
	"/private/tmp/",
	"/var/folders/",
}

var (
	homeResolveOnce sync.Once
	resolvedHomeAbs string
	resolvedHomeErr error

	allowlistResolveOnce sync.Once
	resolvedAllowlist    []string
)

// getResolvedHome returns the user's absolute home directory, memoized.
// Returns ("", err) on first failure; subsequent calls reuse the same
// error. The fail-closed behavior of reviewAndPrune treats a non-nil
// error as "no tilde paths are trustworthy" — see resolvedPathAllowlist.
func getResolvedHome() (string, error) {
	homeResolveOnce.Do(func() {
		h, err := os.UserHomeDir()
		if err != nil {
			resolvedHomeErr = err
			return
		}
		resolvedHomeAbs = h
	})
	return resolvedHomeAbs, resolvedHomeErr
}

// resolvedPathAllowlist returns the path-allowlist with `~` expanded.
// On HOME resolution failure, omits the tilde entries entirely — only
// /tmp/, /private/tmp/, /var/folders/ survive. This is the fail-closed
// fallback: under-resolve and over-prune, never under-prune.
func resolvedPathAllowlist() []string {
	allowlistResolveOnce.Do(func() {
		home, _ := getResolvedHome()
		for _, p := range pathAllowlistRoots {
			if strings.HasPrefix(p, "~/") {
				if home == "" {
					continue
				}
				resolvedAllowlist = append(resolvedAllowlist, home+p[1:])
			} else {
				resolvedAllowlist = append(resolvedAllowlist, p)
			}
		}
	})
	return resolvedAllowlist
}

// pathTokenRe finds path-like tokens in a Bash inner-arg. A token is
// a `/` or `~/` followed by non-whitespace, non-quote characters. The
// leading delimiter (start-of-string, whitespace, or quote) is
// non-capturing.
var pathTokenRe = regexp.MustCompile(`(?:^|[\s'"])((?:~/|/)[^\s'"]+)`)

// hasGlob reports whether arg contains a shell-style wildcard. `*`
// and `**` count; the literal substring `./...` counts (Go
// package-selector convention). Bare ellipsis (`...` in a URL) does
// NOT count — it's not a shell wildcard.
func hasGlob(arg string) bool {
	return strings.Contains(arg, "*") || strings.Contains(arg, "./...")
}

// extractBashArg returns the inner argument of "Bash(<arg>)" or the
// empty string and false if entry isn't a Bash entry. Takes the first
// `(` and last `)` as delimiters so an inner `)` doesn't break parsing.
func extractBashArg(entry string) (string, bool) {
	if !strings.HasPrefix(entry, "Bash(") || !strings.HasSuffix(entry, ")") {
		return "", false
	}
	return entry[len("Bash(") : len(entry)-1], true
}

// inPathAllowlist reports whether path (after tilde resolution) is
// rooted under any allowlisted prefix. Roots are stored with a
// trailing slash to anchor directory boundaries (so `/tmp` !=
// `/tmpfoo`); the bare-root case (path equals root without the
// trailing slash, e.g. `Bash(ls /tmp)`) is also accepted. Returns
// false (untrusted) when HOME isn't resolvable for a tilde path —
// the fail-closed branch.
func inPathAllowlist(path string) bool {
	if strings.HasPrefix(path, "~/") {
		home, err := getResolvedHome()
		if err != nil {
			return false
		}
		path = home + path[1:]
	}
	for _, root := range resolvedPathAllowlist() {
		if strings.HasPrefix(path, root) {
			return true
		}
		// Accept bare root form too: a trailing-slash root matches an
		// arg-side path that names the root directory without the
		// trailing slash. `/tmp/` allowlists `/tmp`.
		if strings.HasSuffix(root, "/") && path == strings.TrimSuffix(root, "/") {
			return true
		}
	}
	return false
}

// pruneVerdict applies the heuristic to a single Bash inner-arg and
// returns either "" (keep) or the reason that triggered the prune.
// Ordering matters: hard-deny first (always fires), then escape-soup,
// then path-based decision (allowlisted paths exempt the entry from
// long-no-glob), then long-no-glob as the catch-all for specific
// no-path commands.
func pruneVerdict(arg string) string {
	// Hard-deny by verb prefix.
	for _, prefix := range hardDenyVerbPrefixes {
		if arg == prefix || strings.HasPrefix(arg, prefix+" ") {
			return "hard-deny"
		}
	}
	// Hard-deny by path substring.
	for _, sub := range hardDenyPathSubstrings {
		if strings.Contains(arg, sub) {
			return "hard-deny"
		}
	}
	// Escape-soup.
	if strings.Count(arg, `\`) >= 3 {
		return "escape-soup"
	}
	// Path-allowlist decision. If ALL path tokens land in the
	// allowlist, exempt from the long-no-glob catch-all (the user's
	// approval was likely intentional and targets a known location).
	// If ANY path is outside, prune.
	if matches := pathTokenRe.FindAllStringSubmatch(arg, -1); len(matches) > 0 {
		for _, m := range matches {
			if !inPathAllowlist(m[1]) {
				return "untrusted-path"
			}
		}
		return ""
	}
	// Long no-glob catch-all.
	if len(arg) > 30 && !hasGlob(arg) {
		return "long-no-glob"
	}
	return ""
}

// reviewAndPrune walks permissions.allow and removes any Bash entry
// that triggers pruneVerdict. Skill / Read / Agent / unknown entries
// pass through untouched. Returns the (mutated) input map and the
// slice of pruned records (nil if no changes). Idempotent: a second
// call on the result produces no further prunes.
//
// Only permissions.allow is touched. permissions.ask and
// permissions.deny are left alone (deny entries the user has
// explicitly added must survive).
func reviewAndPrune(perms map[string]any) (map[string]any, []PruneRecord) {
	if perms == nil {
		return nil, nil
	}
	permsInner, _ := perms["permissions"].(map[string]any)
	if permsInner == nil {
		return perms, nil
	}
	allowList, _ := permsInner["allow"].([]any)
	if len(allowList) == 0 {
		return perms, nil
	}
	var kept []any
	var pruned []PruneRecord
	for _, e := range allowList {
		s, ok := e.(string)
		if !ok {
			kept = append(kept, e)
			continue
		}
		arg, isBash := extractBashArg(s)
		if !isBash {
			kept = append(kept, e)
			continue
		}
		if reason := pruneVerdict(arg); reason != "" {
			pruned = append(pruned, PruneRecord{Entry: s, Reason: reason})
			continue
		}
		kept = append(kept, e)
	}
	if len(pruned) == 0 {
		return perms, nil
	}
	permsInner["allow"] = kept
	perms["permissions"] = permsInner
	return perms, pruned
}

// ReviewAndPruneRepoSettings reads
// <repoPath>/.claude/settings.local.json, removes noise entries via
// reviewAndPrune, snapshots the pre-write state, writes the cleaned
// file, and appends each removal to the audit log. Fires
// unconditionally on every ticket transition — the load-bearing
// idempotency contract is that this function returns (nil, nil)
// without writing, snapshotting, or logging whenever no entries
// would change. Repeated transitions on a clean file produce zero
// side effects.
//
// Empty repoPath is a no-op. Write failures are returned but the
// status transition that called this is NOT rolled back —
// settings-file mutation is best-effort, not part of the move's
// authoritative state.
func ReviewAndPruneRepoSettings(repoPath string) ([]PruneRecord, error) {
	if repoPath == "" {
		return nil, nil
	}
	cur, err := readClaudeSettings(repoPath)
	if err != nil {
		return nil, fmt.Errorf("review-and-prune: read repo: %w", err)
	}
	cleaned, pruned := reviewAndPrune(cur)
	if len(pruned) == 0 {
		// Idempotency contract: no write, no snapshot, no log entry.
		return nil, nil
	}
	if err := snapshotSettings(repoPath); err != nil {
		// Snapshot is the recovery path; if it fails refuse to write
		// since there's no rollback path.
		return nil, fmt.Errorf("review-and-prune: snapshot: %w", err)
	}
	if err := writeClaudeSettings(repoPath, cleaned); err != nil {
		return nil, fmt.Errorf("review-and-prune: write repo: %w", err)
	}
	if err := appendPrunedLog(repoPath, pruned); err != nil {
		// Audit-log failure is non-fatal — the file is already cleaned.
		// Return the records AND the wrapped error so callers can log
		// it without rolling back.
		return pruned, fmt.Errorf("review-and-prune: append log: %w", err)
	}
	return pruned, nil
}

// ----- audit log + snapshot rotation -----

const (
	prunedLogFile     = ".pruned-log"
	snapshotPrefix    = claudeSettingsLocalFile + ".bak."
	snapshotKeepCount = 3
)

// appendPrunedLog opens (or creates) <repo>/.claude/.pruned-log in
// append mode and writes one RFC3339-timestamped line per record:
// `<timestamp> <reason> <entry>`. Best-effort — caller decides
// whether to surface a returned error.
func appendPrunedLog(repoPath string, pruned []PruneRecord) error {
	if len(pruned) == 0 {
		return nil
	}
	dir := filepath.Join(repoPath, claudeSubdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, prunedLogFile)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	ts := time.Now().Format(time.RFC3339)
	for _, r := range pruned {
		if _, err := fmt.Fprintf(f, "%s %s %s\n", ts, r.Reason, r.Entry); err != nil {
			return err
		}
	}
	return nil
}

// snapshotSettings copies the current settings.local.json to
// settings.local.json.bak.<unix-nanos>, then rotates older snapshots
// keeping only the 3 most recent (by nanos suffix, total-ordered
// lexicographically). No snapshot if the source file is missing.
//
// Nanosecond precision guarantees two snapshots in the same wall-clock
// second don't collide — second-precision suffixes would overwrite
// each other on rapid transitions.
func snapshotSettings(repoPath string) error {
	src := filepath.Join(repoPath, claudeSubdir, claudeSettingsLocalFile)
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	dir := filepath.Join(repoPath, claudeSubdir)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	dst := filepath.Join(dir, snapshotPrefix+suffix)
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return err
	}
	return rotateSnapshots(dir)
}

// rotateSnapshots scans <dir> for files matching settings.local.json.bak.*,
// sorts them by suffix lexicographically (== chronological under nanos),
// and removes all but the snapshotKeepCount most recent.
func rotateSnapshots(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var snaps []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), snapshotPrefix) {
			snaps = append(snaps, e.Name())
		}
	}
	if len(snaps) <= snapshotKeepCount {
		return nil
	}
	sort.Strings(snaps) // ascending = oldest first
	toRemove := snaps[:len(snaps)-snapshotKeepCount]
	for _, name := range toRemove {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

