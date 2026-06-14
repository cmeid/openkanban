# Fix Claude Code TTY corruption from TUI log leaks

<!-- openkanban:card-notes start -->
## Notes (from openkanban card)

**Status:** shipped to `main` on `cmeid/openkanban` (tip `3c7ba71` after rebase). Pending in-the-wild verification.

## Problem

The TUI process never redirected the `log` package. 46+ `log.Printf` sites in `daemonclient`, `ui/daemon_subscribe`, `ui/model`, and `project/{tickets,migration}` wrote to stderr — the same TTY Bubble Tea was rendering into. During a Claude Code session in an openkanban-managed pane, lines like `openkanban client: handlePush event="exited"` interleaved with alt-screen output and broke the display.

## Fix

Three commits on `main`:

- `feat(app): add TUILogPath helper`
- `fix(app): redirect TUI logs to a file`
- `test(app): cover TUI log redirect end-to-end`

Approach: open `~/.cache/openkanban/tui.log` as the literal first statement of `app.Run`, before `LoadGlobalTicketStore` (which fans out to 11 migration-log sites). Honor `OPENKANBAN_TUI_LOG`. Fall back to `io.Discard` + stderr warning if open fails. Also flipped `internal/app/app.go:79` from `log.Printf` to `fmt.Fprintln(os.Stderr)` so the daemon-unavailable hint stays visible after the redirect.

## Verification pending

- [ ] Launch a real TUI with a live Claude Code agent and confirm `tail -f ~/.cache/openkanban/tui.log` fills while the alt-screen stays clean.
- [ ] Migration-trigger case: rename `<project>/tickets-store.json.migrated` back, launch, confirm no migration log on screen.
- [ ] Daemon-unavailable visibility: remove socket, confirm stderr message shows pre-TUI.

If the symptom persists, suspect:

- non-stdlib loggers in the TUI process
- direct `fmt.Fprintf(os.Stderr, …)` calls in hot paths
- daemon-side stderr leaking through PTY frames (the daemon already routes its stderr to `daemon.log` when autostarted, so this would be a regression)

## References

- Plan: `/Users/cmeid/.claude/plans/evalutate-and-propose-the-reactive-dolphin.md`
- Spawning session: `333e32ab-2b3c-4e55-9f9f-ccd6747a80a9`
<!-- openkanban:card-notes end -->
