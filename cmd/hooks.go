package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// hookCommandPrefix is how we recognize our own previously-installed
// hook commands during dedupe. Any command on a watched event whose
// string starts with this is treated as "ours" and rewritten to the
// canonical form (rather than skipped) so re-running `hooks install`
// picks up fixes to that canonical form.
const hookCommandPrefix = "openkanban status set "

// hookEntry describes one (EventName, command) pair we want installed
// into ~/.claude/settings.json.
type hookEntry struct {
	Event   string
	Command string
}

// managedHooks is the canonical set of hooks we install. The Claude
// Code settings.json shape per event is:
//
//	"<EventName>": [
//	  {"matcher": "...optional...",
//	   "hooks": [{"type": "command", "command": "..."}]}
//	]
//
// Session-level events (SessionStart, UserPromptSubmit, Stop,
// Notification) don't need a matcher.
var managedHooks = []hookEntry{
	{Event: "SessionStart", Command: "openkanban status set working"},
	{Event: "UserPromptSubmit", Command: "openkanban status set working"},
	{Event: "Stop", Command: "openkanban status set idle"},
	{Event: "Notification", Command: "openkanban status set waiting"},
}

var (
	hooksInstallPath   string
	hooksInstallDryRun bool
)

var hooksCmd = &cobra.Command{
	Use:   "hooks",
	Short: "Manage Claude Code hook integration",
	Long: `Install or inspect the Claude Code hooks that report session status
back to openkanban.`,
}

var hooksInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install openkanban status hooks into Claude Code settings",
	Long: `Install four hook entries into ~/.claude/settings.json so that this
session's status (working / idle / waiting) is reported to openkanban
whenever Claude Code's SessionStart / UserPromptSubmit / Stop /
Notification events fire.

Existing settings.json keys are preserved verbatim. The original file
is backed up alongside itself with a timestamp suffix before write.
Running install twice does not duplicate entries — our previously-
installed entries are recognized and rewritten in place.

Use --dry-run to preview the merged settings.json without touching disk.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		dest, err := resolveHooksPath(hooksInstallPath)
		if err != nil {
			return err
		}
		return installHooks(dest, hooksInstallDryRun, cmd.OutOrStdout())
	},
}

// resolveHooksPath returns the absolute settings.json path, defaulting
// to $HOME/.claude/settings.json when --path is empty.
func resolveHooksPath(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// installHooks reads dest, merges managedHooks in idempotently, and
// writes the result atomically (with a timestamped backup of the
// original if one existed). When dryRun is true, it instead prints
// the would-be settings.json to out and touches nothing on disk.
//
// Returned errors do not modify dest — we always parse + plan before
// writing, and the write itself is atomic via temp-file + rename.
func installHooks(dest string, dryRun bool, out io.Writer) error {
	original, existed, err := readSettings(dest)
	if err != nil {
		return err
	}

	settings, err := parseSettings(original)
	if err != nil {
		// Refuse to clobber a malformed file — the user might have
		// in-progress edits or a JSON with trailing commas they care about.
		return fmt.Errorf("parse %s: %w", dest, err)
	}

	if err := mergeHooks(settings, managedHooks); err != nil {
		return err
	}

	merged, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal merged settings: %w", err)
	}
	merged = append(merged, '\n')

	if dryRun {
		fmt.Fprintf(out, "# would write %s\n", dest)
		_, _ = out.Write(merged)
		return nil
	}

	var backupPath string
	if existed {
		backupPath = dest + ".bak-" + time.Now().Format("20060102150405")
		if err := os.WriteFile(backupPath, original, 0644); err != nil {
			return fmt.Errorf("write backup %s: %w", backupPath, err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return fmt.Errorf("ensure parent dir for %s: %w", dest, err)
	}
	tmp := dest + ".tmp"
	if err := os.WriteFile(tmp, merged, 0644); err != nil {
		return fmt.Errorf("write temp file %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		// Best-effort cleanup of the partial temp file.
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s -> %s: %w", tmp, dest, err)
	}

	fmt.Fprintf(out, "wrote %s\n", dest)
	if backupPath != "" {
		fmt.Fprintf(out, "backup %s\n", backupPath)
	}
	fmt.Fprintf(out, "events updated: %s\n", strings.Join(eventNames(managedHooks), ", "))
	return nil
}

// readSettings reads dest. A non-existent file is not an error — we
// return (nil, false, nil) so the caller can treat it as "start from
// an empty object".
func readSettings(dest string) ([]byte, bool, error) {
	data, err := os.ReadFile(dest)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read %s: %w", dest, err)
	}
	return data, true, nil
}

// parseSettings decodes the raw bytes into a generic map so we
// preserve unknown top-level keys verbatim on round-trip. Empty
// input is treated as an empty object.
func parseSettings(data []byte) (map[string]any, error) {
	if len(data) == 0 || len(strings.TrimSpace(string(data))) == 0 {
		return map[string]any{}, nil
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, err
	}
	if settings == nil {
		settings = map[string]any{}
	}
	return settings, nil
}

// mergeHooks updates settings["hooks"] in place. For each managed
// event, it locates (or creates) the slice and either replaces our
// previously-installed entry (matched by the openkanban prefix on
// the inner hooks command) or appends a fresh one. User-owned
// entries on the same event are left untouched.
func mergeHooks(settings map[string]any, entries []hookEntry) error {
	hooksRaw, ok := settings["hooks"]
	var hooksMap map[string]any
	if ok {
		hooksMap, ok = hooksRaw.(map[string]any)
		if !ok {
			return fmt.Errorf(`"hooks" key is %T; want object`, hooksRaw)
		}
	} else {
		hooksMap = map[string]any{}
	}

	for _, entry := range entries {
		if err := upsertEventEntry(hooksMap, entry); err != nil {
			return err
		}
	}

	settings["hooks"] = hooksMap
	return nil
}

// upsertEventEntry adds (or rewrites) our entry on the named event,
// preserving all foreign entries on that event.
func upsertEventEntry(hooksMap map[string]any, entry hookEntry) error {
	canonical := canonicalEntry(entry.Command)

	existing, ok := hooksMap[entry.Event]
	if !ok {
		hooksMap[entry.Event] = []any{canonical}
		return nil
	}

	slice, ok := existing.([]any)
	if !ok {
		return fmt.Errorf(`hooks[%q] is %T; want array`, entry.Event, existing)
	}

	replaced := false
	for i, item := range slice {
		if isOursOrEmpty(item) {
			slice[i] = canonical
			replaced = true
			break
		}
	}
	if !replaced {
		slice = append(slice, canonical)
	}

	hooksMap[entry.Event] = slice
	return nil
}

// canonicalEntry returns the JSON-shaped entry we write for a given
// command. No matcher — session events don't need one.
func canonicalEntry(command string) map[string]any {
	return map[string]any{
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": command,
			},
		},
	}
}

// isOursOrEmpty reports whether item looks like a previously-installed
// openkanban entry, identified by any inner hook command starting with
// hookCommandPrefix. Foreign entries return false and are preserved.
func isOursOrEmpty(item any) bool {
	obj, ok := item.(map[string]any)
	if !ok {
		return false
	}
	inner, ok := obj["hooks"].([]any)
	if !ok {
		return false
	}
	for _, h := range inner {
		hobj, ok := h.(map[string]any)
		if !ok {
			continue
		}
		cmd, _ := hobj["command"].(string)
		if strings.HasPrefix(cmd, hookCommandPrefix) {
			return true
		}
	}
	return false
}

// eventNames is a tiny helper for the summary log line.
func eventNames(entries []hookEntry) []string {
	out := make([]string, 0, len(entries))
	seen := map[string]struct{}{}
	for _, e := range entries {
		if _, dup := seen[e.Event]; dup {
			continue
		}
		seen[e.Event] = struct{}{}
		out = append(out, e.Event)
	}
	return out
}

func init() {
	hooksInstallCmd.Flags().StringVar(&hooksInstallPath, "path", "",
		"Override settings.json path (default ~/.claude/settings.json)")
	hooksInstallCmd.Flags().BoolVar(&hooksInstallDryRun, "dry-run", false,
		"Print the would-be settings.json instead of writing")

	hooksCmd.AddCommand(hooksInstallCmd)
	rootCmd.AddCommand(hooksCmd)
}
