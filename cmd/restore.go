package cmd

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/service"
)

var (
	restoreDryRun     bool
	restoreYes        bool
	restoreOnConflict string
)

// validConflictModes lists every accepted --on-conflict value. Kept
// in one place so the planner's validation, the error message, and
// the prompt-loop dispatch can't drift out of sync.
var validConflictModes = []string{"skip", "overwrite", "prompt"}

var restoreCmd = &cobra.Command{
	Use:   "restore <archive>",
	Short: "Restore an openkanban backup archive (per-file conflict resolution)",
	Long: `Restore the contents of an openkanban backup zip into this machine.

The archive's manifest.json drives the destination set:

  config/*              → $OPENKANBAN_CONFIG_DIR (config.ConfigDir())
  repos/<name>/tickets/* → each archive-listed project's RepoPath/tickets,
                          ONLY if that RepoPath exists on disk

Anything that tries to write outside those roots (zip-slip) aborts the
whole restore with an error.

For every entry, the existing destination is compared byte-for-byte to
the archive entry. Identical files are skipped (we do NOT touch mtime).
Conflicting files follow --on-conflict:

  skip       always keep the existing file
  overwrite  always write the archive's bytes
  prompt     ask per-file (y/n/d/a/A)
              y = restore this file
              n = skip this file
              d = show a unified diff (uses diff(1) if available)
              a = yes to all remaining conflicts
              A = no  to all remaining conflicts

Projects whose RepoPath does not exist on this machine cannot receive
their repos/<name>/tickets/* entries; the planner will prompt to abort
(so you can clone the missing repo) or skip those entries. The
projects.json registry entry is still restored verbatim so the missing
project remains visible to openkanban after restore.

Use --dry-run to preview the plan without writing. Use --yes to skip
the initial confirmation; --yes does NOT change --on-conflict.`,
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		plan, err := planRestore(args[0])
		if err != nil {
			return err
		}

		out := cmd.OutOrStdout()
		printRestorePlan(out, plan)

		if restoreDryRun {
			fmt.Fprintln(out, "\n(dry-run — no files written)")
			return nil
		}

		// Single bufio.Reader for the whole restore. Used for the
		// initial confirmation AND the conflict loop so buffered
		// keystrokes survive across prompts.
		reader := bufio.NewReader(cmd.InOrStdin())

		// Missing-repo prompts at plan time (not execute time, so the
		// user can abort before any I/O happens).
		if err := resolveMissingRepos(reader, out, &plan); err != nil {
			return err
		}

		if !restoreYes {
			fmt.Fprint(out, "Proceed? [y/N] ")
			line, _ := reader.ReadString('\n')
			ans := strings.TrimSpace(line)
			if !(ans == "y" || ans == "Y" || ans == "yes" || ans == "YES") {
				fmt.Fprintln(out, "aborted")
				return nil
			}
		}

		return executeRestorePlan(reader, out, plan)
	},
}

// restorePlan is the resolved set of inputs for a restore. Built by
// planRestore, consumed by printRestorePlan and executeRestorePlan.
// Mirrors backupPlan / uninstallPlan.
type restorePlan struct {
	// ArchivePath is the absolute path to the zip we'll read.
	ArchivePath string

	// Manifest is the decoded manifest.json from the archive.
	Manifest backupManifest

	// ConfigDir is the destination ConfigDir on THIS machine
	// (config.ConfigDir()). Note: not the manifest's source_config_dir.
	ConfigDir string

	// ConflictMode is the validated --on-conflict value. Always one
	// of validConflictModes.
	ConflictMode string

	// ProjectRoots maps the archive's `repos/<ArchiveDir>` prefix to
	// the absolute write root on this machine
	// (<RepoPath>/tickets) — but ONLY for projects whose RepoPath
	// currently exists. Missing-repo projects are absent here and
	// their archive entries are dropped at execute time.
	ProjectRoots map[string]string

	// MissingRepos enumerates each manifest project whose RepoPath
	// is not an existing directory on this machine. Surfaced in
	// printRestorePlan and resolved interactively before execute.
	MissingRepos []missingRepoInfo

	// SkippedRepos collects archive prefixes the user (or -y default)
	// chose to skip when a repo is missing. Populated by
	// resolveMissingRepos. Used by executeRestorePlan to drop
	// entries that fall under those prefixes.
	SkippedRepos map[string]bool
}

// missingRepoInfo describes a project in the manifest whose RepoPath
// does not exist on this machine.
type missingRepoInfo struct {
	// ID is the project UUID.
	ID string

	// Name is the human-readable project name.
	Name string

	// RepoPath is the manifest-recorded RepoPath (absolute path
	// that was valid on the source machine; may be invalid here).
	RepoPath string

	// ArchivePrefix is the archive prefix whose entries WOULD have
	// gone into <RepoPath>/tickets — e.g. "repos/openkanban/tickets".
	// Populated so the prompt loop can attach the user's choice to
	// the right entries.
	ArchivePrefix string
}

// planRestore validates flags, opens the archive, reads manifest.json,
// and computes write roots + missing-repo info. Does NOT mutate the
// filesystem and does NOT execute any prompts.
func planRestore(archivePath string) (restorePlan, error) {
	// Validate --on-conflict early. Empty string folds to "prompt"
	// (the documented default), matching the cobra default; anything
	// else not in validConflictModes is a clear error.
	mode := strings.TrimSpace(restoreOnConflict)
	if mode == "" {
		mode = "prompt"
	}
	if !isValidConflictMode(mode) {
		return restorePlan{}, fmt.Errorf("invalid --on-conflict %q; must be one of: %s",
			restoreOnConflict, strings.Join(validConflictModes, ", "))
	}

	absArchive, err := filepath.Abs(archivePath)
	if err != nil {
		return restorePlan{}, fmt.Errorf("resolve archive path: %w", err)
	}
	if _, err := os.Stat(absArchive); err != nil {
		return restorePlan{}, fmt.Errorf("archive not readable: %w", err)
	}

	configDir, err := config.ConfigDir()
	if err != nil {
		return restorePlan{}, fmt.Errorf("resolve config dir: %w", err)
	}

	// Open the archive to read the manifest only. We close immediately;
	// executeRestorePlan re-opens for the streaming extract.
	zr, err := zip.OpenReader(absArchive)
	if err != nil {
		return restorePlan{}, fmt.Errorf("open archive: %w", err)
	}
	defer zr.Close()

	var manifest backupManifest
	manifestFound := false
	for _, f := range zr.File {
		if f.Name != "manifest.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return restorePlan{}, fmt.Errorf("open manifest.json in archive: %w", err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return restorePlan{}, fmt.Errorf("read manifest.json: %w", err)
		}
		if err := json.Unmarshal(data, &manifest); err != nil {
			return restorePlan{}, fmt.Errorf("parse manifest.json: %w", err)
		}
		manifestFound = true
		break
	}
	if !manifestFound {
		return restorePlan{}, fmt.Errorf("archive does not contain manifest.json (not an openkanban backup?)")
	}

	plan := restorePlan{
		ArchivePath:  absArchive,
		Manifest:     manifest,
		ConfigDir:    configDir,
		ConflictMode: mode,
		ProjectRoots: map[string]string{},
		SkippedRepos: map[string]bool{},
	}

	// Derive the ArchiveDir each manifest project would use. We don't
	// have the source machine's collision map, but the archive itself
	// tells us — scan its top-level `repos/<dir>/` entries and match
	// each manifest project to the dir whose entries actually exist.
	// This is robust to the source machine's collision-suffix logic
	// without requiring it to be embedded in the manifest.
	prefixesInArchive := map[string]bool{}
	for _, f := range zr.File {
		// Strip leading "repos/" and trailing "/..." to get "<dir>".
		if !strings.HasPrefix(f.Name, "repos/") {
			continue
		}
		rest := strings.TrimPrefix(f.Name, "repos/")
		// Skip the bare "repos/" dir entry.
		if rest == "" {
			continue
		}
		slash := strings.Index(rest, "/")
		if slash < 0 {
			continue
		}
		prefixesInArchive["repos/"+rest[:slash]+"/tickets"] = true
	}

	for _, p := range manifest.Projects {
		// Match this project to an archive prefix. Try the sanitized
		// name first; then sanitized + "-<id-prefix>" for the
		// collision-suffix case. Falls back to the bare sanitized
		// name even if it isn't in the archive — the executor will
		// simply see no matching entries.
		sanitized := sanitizeArchiveName(p.Name)
		candidates := []string{
			"repos/" + sanitized + "/tickets",
		}
		// Try a few collision-suffix shapes. Backup uses ID[:8] when
		// names collide; we accept that exact shape and the full ID
		// just in case.
		idPrefix := p.ID
		if len(idPrefix) > 8 {
			idPrefix = idPrefix[:8]
		}
		candidates = append(candidates,
			"repos/"+sanitized+"-"+idPrefix+"/tickets",
			"repos/"+sanitized+"-"+p.ID+"/tickets",
		)

		var matched string
		for _, c := range candidates {
			if prefixesInArchive[c] {
				matched = c
				break
			}
		}
		if matched == "" {
			matched = candidates[0]
		}

		repoTickets := filepath.Join(p.RepoPath, "tickets")
		if dirExists(p.RepoPath) {
			plan.ProjectRoots[matched] = repoTickets
		} else {
			plan.MissingRepos = append(plan.MissingRepos, missingRepoInfo{
				ID:            p.ID,
				Name:          p.Name,
				RepoPath:      p.RepoPath,
				ArchivePrefix: matched,
			})
		}
	}

	// Stable order for printed plans / deterministic prompt loop.
	sort.Slice(plan.MissingRepos, func(i, j int) bool {
		return plan.MissingRepos[i].Name < plan.MissingRepos[j].Name
	})

	return plan, nil
}

// isValidConflictMode reports whether mode is one of the documented
// --on-conflict choices.
func isValidConflictMode(mode string) bool {
	for _, m := range validConflictModes {
		if mode == m {
			return true
		}
	}
	return false
}

// printRestorePlan writes a stable, scannable summary of what restore
// will do, mirroring printBackupPlan's shape. Never returns an error.
func printRestorePlan(out io.Writer, plan restorePlan) {
	fmt.Fprintln(out, "openkanban restore plan")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Archive: %s\n", plan.ArchivePath)
	if plan.Manifest.CreatedAt != "" {
		fmt.Fprintf(out, "  created:          %s\n", plan.Manifest.CreatedAt)
	}
	if plan.Manifest.OpenkanbanVersion != "" {
		fmt.Fprintf(out, "  openkanban version: %s\n", plan.Manifest.OpenkanbanVersion)
	}
	if plan.Manifest.SourceConfigDir != "" {
		fmt.Fprintf(out, "  source config dir: %s\n", plan.Manifest.SourceConfigDir)
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Conflict policy: %s\n", plan.ConflictMode)
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Will write to:")
	fmt.Fprintf(out, "  config dir: %s\n", plan.ConfigDir)
	if len(plan.ProjectRoots) == 0 {
		fmt.Fprintln(out, "  (no project tickets/ destinations resolved on this machine)")
	} else {
		// Sort by archive prefix for determinism.
		var keys []string
		for k := range plan.ProjectRoots {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(out, "  %s/* → %s\n", k, plan.ProjectRoots[k])
		}
	}

	if len(plan.MissingRepos) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Missing repos (RepoPath does not exist on this machine):")
		for _, mr := range plan.MissingRepos {
			fmt.Fprintf(out, "  - %s (id=%s, repo_path=%s)\n", mr.Name, mr.ID, mr.RepoPath)
		}
		fmt.Fprintln(out, "  You will be prompted to abort (so you can clone) or skip these.")
	}
	fmt.Fprintln(out)
}

// resolveMissingRepos walks plan.MissingRepos and, for each, asks the
// user whether to abort (so they can clone the repo) or skip just
// that repo's tickets. With --yes, every missing repo is skipped
// (the prompt is a confirmation, not a policy). The user's choices
// land in plan.SkippedRepos.
func resolveMissingRepos(in *bufio.Reader, out io.Writer, plan *restorePlan) error {
	if len(plan.MissingRepos) == 0 {
		return nil
	}
	for _, mr := range plan.MissingRepos {
		if restoreYes {
			plan.SkippedRepos[mr.ArchivePrefix] = true
			fmt.Fprintf(out, "  -y: skipping tickets for missing repo %q\n", mr.Name)
			continue
		}
		for {
			fmt.Fprintf(out, "  Missing repo %q (path=%s) — [a]bort (to clone), [s]kip its tickets? ",
				mr.Name, mr.RepoPath)
			line, err := in.ReadString('\n')
			if err != nil && line == "" {
				return fmt.Errorf("read missing-repo response: %w", err)
			}
			ans := strings.TrimSpace(line)
			if ans == "a" || ans == "A" {
				return fmt.Errorf("aborted: missing repo %q at %s — clone it and rerun",
					mr.Name, mr.RepoPath)
			}
			if ans == "s" || ans == "S" {
				plan.SkippedRepos[mr.ArchivePrefix] = true
				break
			}
			fmt.Fprintln(out, "  (please answer 'a' or 's')")
		}
	}
	return nil
}

// executeRestorePlan extracts the archive, applying:
//   - zip-slip rejection (entry name must clean into one of the write
//     roots; anything else aborts the whole restore)
//   - identical-file skip (byte-equal contents → leave dest untouched,
//     no Chtimes)
//   - conflict policy (skip/overwrite/prompt) for non-identical writes
//   - skip-list for missing repos (entries under those prefixes are
//     dropped silently)
//
// reader must be the SAME bufio.Reader passed through from
// resolveMissingRepos / the initial confirmation prompt — buffered
// keystrokes survive between rounds that way.
func executeRestorePlan(reader *bufio.Reader, out io.Writer, plan restorePlan) error {
	zr, err := zip.OpenReader(plan.ArchivePath)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer zr.Close()

	// Pre-flight: scan all entries for zip-slip and fail BEFORE any
	// write. This avoids the partial-restore footgun where some files
	// land on disk and a later entry trips the path check.
	//
	// An entry classified as "drop" (under a SkippedRepos prefix) is
	// NOT a rejection — it just has no destination on this machine.
	// Only entries that fall outside every known root are rejected.
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if f.Name == "manifest.json" {
			continue
		}
		_, _, _, action := classifyEntry(f.Name, plan)
		if action == classifyReject {
			return fmt.Errorf("rejecting archive: entry %q does not map to any allowed write root", f.Name)
		}
	}

	// "Yes / no to all remaining" state for the prompt loop. Bound
	// to this restore run only.
	var (
		yesAll bool
		noAll  bool
	)

	var (
		written int
		skipped int
	)

	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if f.Name == "manifest.json" {
			// Manifest stays in the archive; not extracted to disk.
			continue
		}

		root, dest, kind, action := classifyEntry(f.Name, plan)
		switch action {
		case classifyReject:
			// Pre-flight already verified — defensive: should not happen.
			return fmt.Errorf("entry %q rejected at execute time (post pre-flight)", f.Name)
		case classifyDrop:
			// Under a SkippedRepos prefix — no destination on this
			// machine, just drop silently.
			continue
		}
		_ = kind // kept for future per-kind branching (e.g. permissions)
		_ = root // already validated by classifyEntry

		archiveBytes, err := readZipEntry(f)
		if err != nil {
			return fmt.Errorf("read archive entry %q: %w", f.Name, err)
		}

		// Identical-file check. If dest exists with the same bytes,
		// skip without touching anything (no mtime updates).
		if existingBytes, err := os.ReadFile(dest); err == nil {
			if bytes.Equal(existingBytes, archiveBytes) {
				skipped++
				continue
			}
			// Non-identical: conflict.
			switch plan.ConflictMode {
			case "skip":
				fmt.Fprintf(out, "skip %s (conflict)\n", dest)
				skipped++
				continue
			case "overwrite":
				// fall through to write
			case "prompt":
				if yesAll {
					// fall through to write
				} else if noAll {
					skipped++
					continue
				} else {
					choice, err := promptConflict(reader, out, dest, existingBytes, archiveBytes)
					if err != nil {
						return err
					}
					switch choice {
					case 'y':
						// write
					case 'n':
						skipped++
						continue
					case 'a':
						yesAll = true
						// write
					case 'A':
						noAll = true
						skipped++
						continue
					}
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat dest %q: %w", dest, err)
		}

		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return fmt.Errorf("mkdir for %q: %w", dest, err)
		}
		// Use the archive entry's recorded mode, falling back to 0644.
		mode := f.Mode().Perm()
		if mode == 0 {
			mode = 0644
		}
		if err := os.WriteFile(dest, archiveBytes, mode); err != nil {
			return fmt.Errorf("write %q: %w", dest, err)
		}
		written++
	}

	fmt.Fprintln(out)
	fmt.Fprintf(out, "restore complete: %d written, %d skipped\n", written, skipped)

	// Darwin-only service reminder. Print only when:
	//   - the source machine had the service installed at backup time
	//   - we are on darwin
	//   - the launchd plist is not currently on disk here
	if plan.Manifest.ServiceInstalled && runtime.GOOS == "darwin" {
		if p, err := service.PlistPath(); err == nil {
			if _, statErr := os.Stat(p); errors.Is(statErr, os.ErrNotExist) {
				fmt.Fprintln(out)
				fmt.Fprintln(out, "Reminder: this backup was taken on a machine with the openkanban")
				fmt.Fprintln(out, "launchd service installed. Run `openkanban daemon install-service`")
				fmt.Fprintln(out, "if you want the service active here too.")
			}
		}
	}

	return nil
}

// classifyAction is the result-kind of classifyEntry. Tri-state so
// the pre-flight can distinguish "drop silently" (skipped missing
// repo) from "reject the archive" (zip-slip).
type classifyAction int

const (
	// classifyWrite means the entry has a valid destination on disk
	// and should be extracted there.
	classifyWrite classifyAction = iota
	// classifyDrop means the entry is intentionally without a
	// destination on this machine (e.g. tickets for a skipped
	// missing repo). The executor should skip it without error.
	classifyDrop
	// classifyReject means the entry doesn't map to any allowed
	// root. The whole restore aborts (zip-slip defense).
	classifyReject
)

// classifyEntry maps an archive entry name to (write-root, full-dest,
// kind, action). The kind is "config" or "repo:<archive-prefix>" —
// used only for logging today, kept distinguishable for future
// per-kind behavior.
func classifyEntry(name string, plan restorePlan) (root, dest, kind string, action classifyAction) {
	// config/* → ConfigDir
	if strings.HasPrefix(name, "config/") {
		rel := strings.TrimPrefix(name, "config/")
		return checkRoot(plan.ConfigDir, rel, "config")
	}

	// repos/<dir>/tickets/* → matching ProjectRoots[prefix]
	if strings.HasPrefix(name, "repos/") {
		// Find the longest matching ProjectRoots prefix.
		for prefix, target := range plan.ProjectRoots {
			withSlash := prefix + "/"
			if strings.HasPrefix(name, withSlash) {
				rel := strings.TrimPrefix(name, withSlash)
				return checkRoot(target, rel, "repo:"+prefix)
			}
		}
		// Maybe this is a skipped missing-repo prefix. If so, drop
		// the entry silently (it has no destination on this machine).
		for prefix := range plan.SkippedRepos {
			withSlash := prefix + "/"
			if strings.HasPrefix(name, withSlash) || name == prefix {
				return "", "", "", classifyDrop
			}
		}
		// Unmapped repos entry — reject.
		return "", "", "", classifyReject
	}

	// Anything else (top-level miscellany) — reject.
	return "", "", "", classifyReject
}

// checkRoot resolves rel under root, validates the cleaned path is
// rooted under root (zip-slip), and returns (root, dest, kind,
// classifyWrite) on success or (..., classifyReject) on failure.
func checkRoot(root, rel, kind string) (string, string, string, classifyAction) {
	// Reject empty rel (would resolve to root itself — no file to write).
	if rel == "" || rel == "/" {
		return "", "", "", classifyReject
	}
	// Normalize the archive name into a host-OS path before Clean.
	// archive/zip names are always forward-slash.
	relHost := filepath.FromSlash(rel)
	cleaned := filepath.Clean(filepath.Join(root, relHost))
	// Trailing separator on root for prefix check — Clean strips it.
	rootClean := filepath.Clean(root)
	wantPrefix := rootClean + string(os.PathSeparator)
	if !strings.HasPrefix(cleaned, wantPrefix) && cleaned != rootClean {
		return "", "", "", classifyReject
	}
	// Also reject the case where the cleaned path equals the root
	// itself (that's a dir, not a file).
	if cleaned == rootClean {
		return "", "", "", classifyReject
	}
	return root, cleaned, kind, classifyWrite
}

// readZipEntry slurps a single zip entry into memory. Restore entries
// are small (ticket briefs, config JSON); in-memory is fine and lets
// us byte-compare against the existing dest without a second pass.
func readZipEntry(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// promptConflict runs the y/n/d/a/A interactive loop for a single
// conflicting file. Returns the user's final non-'d' choice — 'd'
// is handled in-loop (show the diff, re-prompt). Reads from `in`,
// which is the shared bufio.Reader bound by RunE.
func promptConflict(in *bufio.Reader, out io.Writer, dest string, existing, archived []byte) (byte, error) {
	diffAvailable := false
	if _, err := exec.LookPath("diff"); err == nil {
		diffAvailable = true
	}

	for {
		if diffAvailable {
			fmt.Fprintf(out, "  conflict: %s — [y]es / [n]o / [d]iff / [a]ll-yes / [A]ll-no? ", dest)
		} else {
			fmt.Fprintf(out, "  conflict: %s — [y]es / [n]o / [a]ll-yes / [A]ll-no? ", dest)
		}
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			return 0, fmt.Errorf("read conflict response: %w", err)
		}
		ans := strings.TrimRight(line, "\r\n")
		// Single-char only — extra trailing chars are tolerated by
		// looking only at the first rune.
		if len(ans) == 0 {
			fmt.Fprintln(out, "  (please answer y/n/a/A or d for diff)")
			continue
		}
		switch ans[0] {
		case 'y':
			return 'y', nil
		case 'n':
			return 'n', nil
		case 'a':
			return 'a', nil
		case 'A':
			return 'A', nil
		case 'd':
			if !diffAvailable {
				fmt.Fprintf(out, "  (diff unavailable; existing=%d bytes, archive=%d bytes — restore? [y/n])\n",
					len(existing), len(archived))
				continue
			}
			if err := showDiff(out, dest, existing, archived); err != nil {
				fmt.Fprintf(out, "  (diff failed: %v)\n", err)
			}
			continue
		default:
			if diffAvailable {
				fmt.Fprintln(out, "  (please answer y/n/d/a/A)")
			} else {
				fmt.Fprintln(out, "  (please answer y/n/a/A)")
			}
		}
	}
}

// showDiff writes archived to a temp file and shells out to
// `diff -u <existing> <tmp>`, streaming output to `out`. The temp
// file is removed on return. diff(1) exits non-zero when files
// differ; that's the expected case here and not a real error.
func showDiff(out io.Writer, dest string, existing, archived []byte) error {
	// We need an existing path for the LHS — use `dest` directly,
	// which IS on disk (we read it as `existing` already).
	_ = existing
	tmp, err := os.CreateTemp("", "openkanban-restore-*.diff")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(archived); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	cmd := exec.Command("diff", "-u", dest, tmpPath)
	cmd.Stdout = out
	cmd.Stderr = out
	// diff exit 1 = files differ (expected); only treat other failures
	// as errors. exec.ExitError carries that info.
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// Exit 1 is "files differ" — fine. >1 is a real error.
			if exitErr.ExitCode() <= 1 {
				return nil
			}
		}
		return err
	}
	return nil
}

func init() {
	restoreCmd.Flags().BoolVar(&restoreDryRun, "dry-run", false,
		"Print the plan without writing any files")
	restoreCmd.Flags().BoolVarP(&restoreYes, "yes", "y", false,
		"Skip the initial confirmation; missing-repo prompts default to skip")
	restoreCmd.Flags().StringVar(&restoreOnConflict, "on-conflict", "prompt",
		"How to handle conflicting files: skip | overwrite | prompt")

	rootCmd.AddCommand(restoreCmd)
}
