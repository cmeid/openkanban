# fix garbled initial render on session attach

<!-- openkanban:card-notes start -->
## Notes (from openkanban card)

On a fresh spawn (and possibly on attach), the agent pane displays garbled output initially. Forcing a redraw — e.g., dragging to select some text in the pane — clears it and the session renders correctly thereafter. Bytes are intact in the emulator; only the render is stale.

## Suspected causes (from a quick read)
1. **Initial-render race across the daemon split.** The fork already carries an atomic `pty.StartWithSize` fix (see `[[openkanban-personal-fork]]`), but with daemon/client separation there's a second seam: `daemonclient.PaneView` attaches, the daemon-side PTY has already emitted bytes, and the TUI may not get a tea.Cmd to repaint until the next message arrives. Selection generates a mouse event → Update → View → fresh paint.
2. **SIGWINCH / SetSize timing.** First Resize may land after first output, so wrapping is computed against wrong cols until something re-flows.
3. **Mouse-mode child suppressing redraws** at attach time.

## Repro
- Spawn a fresh Claude session via `openkanban ticket new` (or open an existing ticket that has no pane yet).
- Observe garbled pane content on first paint.
- Drag-select any text in the pane — it clears.

## Diagnosis aid
`OPENKANBAN_PTY_DEBUG_LOG=/tmp/pty.log openkanban` and diff the byte stream against the emulator buffer state at first View.

## Investigated files
- internal/ui/daemon_subscribe.go — attach + subscribe path
- internal/daemonclient/paneview.go — Attach / Detach / Resize sequencing
- internal/terminal/* — vt emulator wrapper, scrollback
- internal/ui/model.go — Update→View loop after attach

Originally surfaced on the `set-claude-color-to-match-project` ticket — see that ticket for the conversation context.
<!-- openkanban:card-notes end -->
