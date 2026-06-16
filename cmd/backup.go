package cmd

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/project"
	"github.com/techdufus/openkanban/internal/service"
)

var (
	backupOutput string
	backupDryRun bool
	backupYes    bool
)

var backupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Snapshot openkanban config and per-repo ticket briefs to a zip",
	Long: `Snapshot openkanban's volatile state to a single zip archive.

The archive captures the entire $OPENKANBAN_CONFIG_DIR tree (config.json,
projects.json, ticket briefs, archived tickets) plus each registered
project's repo-side tickets/ directory. Symlinks under the repo tickets/
trees are followed and stored as regular files (broken symlinks are
skipped with a warning).

The archive does NOT include the openkanban binary, the launchd plist,
Claude Code hook entries, or anything under ~/.cache — all of those are
re-creatable via re-install. The manifest does record whether a launchd
service was installed at backup time so restore can remind the user.

Output path resolution:
  --output <path>.zip   → use the path verbatim as the archive filename
  --output <dir>        → write openkanban-YYYYMMDD-HHMMSS.zip into <dir>
  (no --output)         → write into ~/backup/openkanban/

The command refuses to overwrite an existing zip file; if you really
want to replace one, delete it by hand first.

Use --dry-run to preview the plan without writing anything. Use --yes
to skip the interactive confirmation prompt.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		plan, err := planBackup()
		if err != nil {
			return err
		}

		out := cmd.OutOrStdout()
		printBackupPlan(out, plan)

		if backupDryRun {
			fmt.Fprintln(out, "\n(dry-run — no archive written)")
			return nil
		}

		if !backupYes {
			if !confirm(cmd.InOrStdin(), out, "Proceed? [y/N] ") {
				fmt.Fprintln(out, "aborted")
				return nil
			}
		}

		return executeBackupPlan(out, plan)
	},
}

// backupPlan is the resolved set of inputs and outputs for a backup
// run. Built by planBackup and consumed by both the printer and the
// executor so they stay in lockstep. Mirrors the uninstall pattern in
// cmd/uninstall.go.
type backupPlan struct {
	// OutputPath is the absolute path of the zip file we will write.
	// Always populated; planBackup refuses if a file already exists at
	// this path. The parent directory may not exist yet — executor
	// MkdirAll's it just-in-time.
	OutputPath string

	// ConfigDir is the source openkanban config directory we'll archive
	// under `config/` in the zip. Resolved via config.ConfigDir().
	ConfigDir string

	// ConfigDirExists records whether ConfigDir was present on disk at
	// plan time. A backup with no config dir is legal (returns a
	// near-empty zip with just a manifest) but worth flagging in the
	// plan output.
	ConfigDirExists bool

	// Projects is the snapshot of registry entries we'll archive. Each
	// entry corresponds to a `repos/<sanitized-name>/tickets/` subtree
	// in the zip. Order is stable (sorted by Name) so the plan output
	// and the zip's central directory don't churn between runs.
	Projects []projectArchiveItem

	// ServiceInstalled is true ONLY when runtime.GOOS == "darwin" AND
	// service.PlistPath() returns a path that os.Stat's successfully.
	// Recorded in manifest.json so restore can remind the user to
	// reinstall the launchd service after a cross-machine restore.
	ServiceInstalled bool

	// EnvWarnings holds one entry per agent in config.Agents whose Env
	// map has a non-empty value. The archive is NOT encrypted, so any
	// secrets in there will land plaintext on disk; users are warned
	// at plan time but not blocked.
	EnvWarnings []string
}

// projectArchiveItem captures the minimum needed to walk a project's
// tickets/ directory at execute time and to populate the manifest.
type projectArchiveItem struct {
	// ID is the project UUID. Used to disambiguate collisions when two
	// projects share a Name — the sanitized archive directory name gets
	// an 8-char ID suffix in that case.
	ID string

	// Name is the project's human-readable name. Used (after
	// sanitization for filesystem-unsafe runes) as the `repos/<Name>/`
	// directory inside the zip.
	Name string

	// RepoPath is the absolute path on disk to the project's git repo
	// root. The actual archive source is <RepoPath>/tickets/.
	RepoPath string

	// TicketsDirExists records whether <RepoPath>/tickets was present
	// at plan time. We still include the project in the manifest if
	// the dir is missing (the registry entry is real), but executor
	// skips the walk and just notes "no tickets/ at <path>".
	TicketsDirExists bool

	// ArchiveDir is the sanitized, collision-resolved directory name
	// used inside the zip (e.g. `repos/openkanban/tickets/...`). Made
	// deterministic in planBackup so executor doesn't have to reason
	// about collisions during the walk.
	ArchiveDir string
}

// backupManifest is the JSON shape written as manifest.json (last
// entry in the zip, so partial archives are detectable). No schema
// version — the shape is small and stable.
type backupManifest struct {
	CreatedAt         string                 `json:"created_at"`
	OpenkanbanVersion string                 `json:"openkanban_version"`
	SourceConfigDir   string                 `json:"source_config_dir"`
	ServiceInstalled  bool                   `json:"service_was_installed"`
	Projects          []manifestProjectEntry `json:"projects"`
}

type manifestProjectEntry struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	RepoPath string `json:"repo_path"`
}

// planBackup resolves the output path, source config dir, registered
// projects, env-var warnings, and the launchd-installed flag without
// touching disk. Returns an error only when:
//   - $HOME / config dir resolution fails outright
//   - the resolved output zip file already exists (refuse-don't-clobber)
func planBackup() (backupPlan, error) {
	configDir, err := config.ConfigDir()
	if err != nil {
		return backupPlan{}, fmt.Errorf("resolve config dir: %w", err)
	}

	outputPath, err := resolveBackupOutputPath(backupOutput, time.Now())
	if err != nil {
		return backupPlan{}, err
	}
	if _, err := os.Stat(outputPath); err == nil {
		return backupPlan{}, fmt.Errorf("refuse to overwrite existing file: %s", outputPath)
	}

	plan := backupPlan{
		OutputPath:      outputPath,
		ConfigDir:       configDir,
		ConfigDirExists: dirExists(configDir),
	}

	// Service detection: PlistPath exists on both darwin and non-darwin
	// (the latter returns ErrUnsupported), so we gate the Stat by GOOS
	// rather than by the function's error return. This keeps the
	// manifest field meaningfully false on linux/windows even if a
	// future build accidentally calls the darwin PlistPath.
	if runtime.GOOS == "darwin" {
		if p, err := service.PlistPath(); err == nil {
			if _, statErr := os.Stat(p); statErr == nil {
				plan.ServiceInstalled = true
			}
		}
	}

	// Best-effort registry load. A missing or unreadable projects.json
	// is treated as "no projects" rather than fatal — backup should not
	// fail on a fresh install.
	reg, regErr := project.LoadRegistry()
	if regErr == nil && reg != nil {
		projects := reg.List()
		// reg.List already sorts by Name, but we depend on that here
		// for deterministic archive-dir disambiguation, so don't trust
		// it implicitly — re-sort.
		sort.Slice(projects, func(i, j int) bool {
			if projects[i].Name == projects[j].Name {
				return projects[i].ID < projects[j].ID
			}
			return projects[i].Name < projects[j].Name
		})

		// First pass: count Name collisions so we know whether to
		// append an ID suffix.
		nameCounts := map[string]int{}
		for _, p := range projects {
			nameCounts[sanitizeArchiveName(p.Name)]++
		}

		for _, p := range projects {
			sanitized := sanitizeArchiveName(p.Name)
			archiveDir := sanitized
			if nameCounts[sanitized] > 1 {
				// Suffix with first 8 chars of ID for stability.
				suffix := p.ID
				if len(suffix) > 8 {
					suffix = suffix[:8]
				}
				archiveDir = sanitized + "-" + suffix
			}
			ticketsDir := filepath.Join(p.RepoPath, "tickets")
			plan.Projects = append(plan.Projects, projectArchiveItem{
				ID:               p.ID,
				Name:             p.Name,
				RepoPath:         p.RepoPath,
				TicketsDirExists: dirExists(ticketsDir),
				ArchiveDir:       archiveDir,
			})
		}
	}

	// Walk agent env maps for plaintext-secret warnings. We use
	// config.Load("") so the user's actual config.json drives this,
	// not the defaults — the defaults all have empty Env maps so we'd
	// never warn.
	cfg, cfgErr := config.Load("")
	if cfgErr == nil && cfg != nil {
		// Sort agent names for stable output.
		names := make([]string, 0, len(cfg.Agents))
		for name := range cfg.Agents {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			agent := cfg.Agents[name]
			if len(agent.Env) == 0 {
				continue
			}
			hasValue := false
			for _, v := range agent.Env {
				if v != "" {
					hasValue = true
					break
				}
			}
			if !hasValue {
				continue
			}
			keys := make([]string, 0, len(agent.Env))
			for k, v := range agent.Env {
				if v != "" {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			plan.EnvWarnings = append(plan.EnvWarnings,
				fmt.Sprintf("agent %q has non-empty env vars: keys=%v — these will be archived plaintext", name, keys))
		}
	}

	return plan, nil
}

// resolveBackupOutputPath implements the --output semantics documented
// in the command's Long help:
//
//	--output ending in .zip → use verbatim (path becomes the zip file)
//	--output anything else  → treat as directory; auto-name inside it
//	--output == ""          → default ~/backup/openkanban/<auto>.zip
//
// `now` is passed in so tests can pin the timestamp.
func resolveBackupOutputPath(flag string, now time.Time) (string, error) {
	stamp := now.UTC().Format("20060102-150405")
	autoName := fmt.Sprintf("openkanban-%s.zip", stamp)

	if flag == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		return filepath.Join(home, "backup", "openkanban", autoName), nil
	}

	if strings.HasSuffix(strings.ToLower(flag), ".zip") {
		// Verbatim — take it as the file path. Resolve to absolute
		// for clearer error messages.
		abs, err := filepath.Abs(flag)
		if err != nil {
			return "", fmt.Errorf("resolve --output: %w", err)
		}
		return abs, nil
	}

	// Treat as directory.
	abs, err := filepath.Abs(flag)
	if err != nil {
		return "", fmt.Errorf("resolve --output: %w", err)
	}
	return filepath.Join(abs, autoName), nil
}

// sanitizeArchiveName replaces filesystem-unsafe runes (path
// separators, nul) with `_`. The archive is allowed to be portable
// across hosts so we apply both `/` and `\` regardless of GOOS, plus
// the runtime os.PathSeparator for paranoia.
func sanitizeArchiveName(name string) string {
	if name == "" {
		return "_"
	}
	replacer := strings.NewReplacer(
		"/", "_",
		"\\", "_",
		"\x00", "_",
		string(os.PathSeparator), "_",
	)
	out := replacer.Replace(name)
	if out == "" {
		return "_"
	}
	return out
}

// printBackupPlan dumps the plan to out in a stable, scannable shape.
// Mirrors the uninstall.printPlan style: never returns an error.
func printBackupPlan(out io.Writer, plan backupPlan) {
	fmt.Fprintln(out, "openkanban backup plan")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Output archive: %s\n", plan.OutputPath)
	fmt.Fprintln(out)

	fmt.Fprintln(out, "Will archive:")
	configMarker := ""
	if !plan.ConfigDirExists {
		configMarker = " (not present — manifest only)"
	}
	fmt.Fprintf(out, "  config dir: %s%s\n", plan.ConfigDir, configMarker)

	if len(plan.Projects) == 0 {
		fmt.Fprintln(out, "  projects:   (none registered)")
	} else {
		fmt.Fprintln(out, "  projects:")
		for _, p := range plan.Projects {
			ticketsDir := filepath.Join(p.RepoPath, "tickets")
			marker := ""
			if !p.TicketsDirExists {
				marker = " (no tickets/ — skipped)"
			}
			fmt.Fprintf(out, "    %-30s → repos/%s/tickets/%s\n", p.Name, p.ArchiveDir, marker)
			fmt.Fprintf(out, "      source: %s\n", ticketsDir)
		}
	}

	fmt.Fprintln(out)
	if plan.ServiceInstalled {
		fmt.Fprintln(out, "Service: launchd plist present — recorded in manifest.")
	} else {
		fmt.Fprintln(out, "Service: no launchd plist (or non-darwin host).")
	}

	if len(plan.EnvWarnings) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "WARNINGS:")
		for _, w := range plan.EnvWarnings {
			fmt.Fprintf(out, "  ! %s\n", w)
		}
	}
	fmt.Fprintln(out)
}

// executeBackupPlan writes the archive. It creates the output
// directory just-in-time (MkdirAll), opens a single archive/zip writer,
// streams in `config/` and `repos/<dir>/tickets/` trees, then writes
// `manifest.json` LAST so a partial archive is easy to detect (no
// manifest = aborted run).
//
// Symlinks under either tree are followed via os.Stat (NOT Lstat);
// broken symlinks are logged and skipped, contributing to a count that
// shows up in the post-execute summary. Files whose name contains
// `.tmp-` or ends in `.tmp` are skipped — matches the convention in
// internal/project/tickets.go:92.
func executeBackupPlan(out io.Writer, plan backupPlan) error {
	if err := os.MkdirAll(filepath.Dir(plan.OutputPath), 0755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}

	f, err := os.Create(plan.OutputPath)
	if err != nil {
		return fmt.Errorf("create archive: %w", err)
	}
	// We use a labeled defer-ish flow: zw.Close must precede f.Close
	// or the central directory won't be flushed. errors.Join keeps both
	// failure modes visible.
	var closeErrs []error
	zw := zip.NewWriter(f)

	var (
		fileCount    int
		symlinkCount int
		brokenCount  int
	)

	// Archive the config dir under `config/`. A missing config dir is
	// allowed; we just skip the walk.
	if plan.ConfigDirExists {
		fc, sc, bc, err := writeTreeToZip(out, zw, plan.ConfigDir, "config")
		fileCount += fc
		symlinkCount += sc
		brokenCount += bc
		if err != nil {
			closeErrs = append(closeErrs, fmt.Errorf("archive config dir: %w", err))
		}
	}

	// Archive each project's repo-side tickets/ directory.
	for _, p := range plan.Projects {
		if !p.TicketsDirExists {
			continue
		}
		ticketsDir := filepath.Join(p.RepoPath, "tickets")
		prefix := filepath.ToSlash(filepath.Join("repos", p.ArchiveDir, "tickets"))
		fc, sc, bc, err := writeTreeToZip(out, zw, ticketsDir, prefix)
		fileCount += fc
		symlinkCount += sc
		brokenCount += bc
		if err != nil {
			closeErrs = append(closeErrs, fmt.Errorf("archive repo %q tickets: %w", p.Name, err))
		}
	}

	// Manifest LAST: writers can detect a partial archive by the
	// absence of manifest.json.
	manifest := backupManifest{
		CreatedAt:         time.Now().UTC().Format(time.RFC3339),
		OpenkanbanVersion: Version,
		SourceConfigDir:   plan.ConfigDir,
		ServiceInstalled:  plan.ServiceInstalled,
		Projects:          make([]manifestProjectEntry, 0, len(plan.Projects)),
	}
	for _, p := range plan.Projects {
		manifest.Projects = append(manifest.Projects, manifestProjectEntry{
			ID:       p.ID,
			Name:     p.Name,
			RepoPath: p.RepoPath,
		})
	}
	if err := writeManifest(zw, manifest); err != nil {
		closeErrs = append(closeErrs, fmt.Errorf("write manifest: %w", err))
	}

	if err := zw.Close(); err != nil {
		closeErrs = append(closeErrs, fmt.Errorf("close zip writer: %w", err))
	}
	if err := f.Close(); err != nil {
		closeErrs = append(closeErrs, fmt.Errorf("close archive file: %w", err))
	}

	if len(closeErrs) > 0 {
		return errors.Join(closeErrs...)
	}

	// Final size readout. Best-effort — a missing file at this point
	// would have surfaced via the close errors above.
	size := int64(-1)
	if st, err := os.Stat(plan.OutputPath); err == nil {
		size = st.Size()
	}

	fmt.Fprintln(out)
	fmt.Fprintf(out, "wrote %s\n", plan.OutputPath)
	fmt.Fprintf(out, "  files:    %d\n", fileCount)
	fmt.Fprintf(out, "  symlinks followed: %d\n", symlinkCount)
	if brokenCount > 0 {
		fmt.Fprintf(out, "  broken symlinks skipped: %d (see warnings above)\n", brokenCount)
	}
	if size >= 0 {
		fmt.Fprintf(out, "  size:     %d bytes\n", size)
	}
	return nil
}

// writeTreeToZip walks root and copies regular files (plus followed
// symlinks) into zw under archivePrefix. Returns (files, symlinks,
// broken) counts so the caller can roll them up for the summary. The
// returned error covers anything beyond a benign "file vanished
// mid-walk" or a broken symlink, which are tolerated.
func writeTreeToZip(out io.Writer, zw *zip.Writer, root, archivePrefix string) (int, int, int, error) {
	var fileCount, symlinkCount, brokenCount int

	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("resolve root %q: %w", root, err)
	}

	walkErr := filepath.Walk(rootAbs, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			// "vanished between readdir and stat" can happen during an
			// active session; treat as benign and continue.
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}

		// Skip the root itself; we don't write an empty entry for it.
		if path == rootAbs {
			return nil
		}

		name := filepath.Base(path)
		// .tmp-* and *.tmp skip (matches internal/project/tickets.go:92).
		if strings.Contains(name, ".tmp-") || strings.HasSuffix(name, ".tmp") {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Follow symlinks via os.Stat. A broken symlink reports
		// os.IsNotExist on Stat.
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Stat(path)
			if err != nil {
				fmt.Fprintf(out, "  ! skipping broken symlink: %s\n", path)
				brokenCount++
				return nil
			}
			symlinkCount++
			info = target
			// Fall through with the resolved info.
		}

		rel, err := filepath.Rel(rootAbs, path)
		if err != nil {
			return fmt.Errorf("relative path for %q: %w", path, err)
		}
		// Zip paths are forward-slash regardless of host OS.
		zipPath := archivePrefix + "/" + filepath.ToSlash(rel)

		if info.IsDir() {
			// archive/zip writes directories as entries ending in `/`.
			// Not strictly required (most readers infer dirs from
			// member paths) but keeps the central directory readable.
			hdr := &zip.FileHeader{
				Name:   zipPath + "/",
				Method: zip.Store,
			}
			hdr.SetMode(info.Mode() | os.ModeDir)
			hdr.Modified = info.ModTime()
			if _, err := zw.CreateHeader(hdr); err != nil {
				return fmt.Errorf("zip create dir %q: %w", zipPath, err)
			}
			return nil
		}

		if !info.Mode().IsRegular() {
			// e.g. device files, named pipes — silently skip; nothing
			// useful to archive.
			return nil
		}

		// Open and copy. Tolerate vanish-during-copy by treating
		// os.IsNotExist as benign (a tmp file that got renamed away).
		src, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("open %q: %w", path, err)
		}
		hdr := &zip.FileHeader{
			Name:   zipPath,
			Method: zip.Deflate,
		}
		hdr.SetMode(info.Mode())
		hdr.Modified = info.ModTime()
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			src.Close()
			return fmt.Errorf("zip create %q: %w", zipPath, err)
		}
		if _, err := io.Copy(w, src); err != nil {
			src.Close()
			return fmt.Errorf("zip copy %q: %w", zipPath, err)
		}
		if err := src.Close(); err != nil {
			return fmt.Errorf("close source %q: %w", path, err)
		}
		fileCount++
		return nil
	})

	return fileCount, symlinkCount, brokenCount, walkErr
}

// writeManifest serializes the manifest into the zip as the final
// entry. Pretty-printed JSON because `unzip -p ... manifest.json` is
// a common debugging move.
func writeManifest(zw *zip.Writer, m backupManifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	hdr := &zip.FileHeader{
		Name:   "manifest.json",
		Method: zip.Deflate,
	}
	hdr.SetMode(0644)
	hdr.Modified = time.Now()
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	return nil
}

func init() {
	backupCmd.Flags().StringVar(&backupOutput, "output", "",
		"Output path: .zip file (verbatim) or directory (auto-named). Default ~/backup/openkanban/.")
	backupCmd.Flags().BoolVar(&backupDryRun, "dry-run", false,
		"Print the plan without writing the archive")
	backupCmd.Flags().BoolVarP(&backupYes, "yes", "y", false,
		"Skip the interactive confirmation prompt")

	rootCmd.AddCommand(backupCmd)
}
