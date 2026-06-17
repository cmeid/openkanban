# scope an "Auto" mode, that automatically attaches to the session that has been waiting the longest

> Status: **scoped and implemented** in this branch. This section is the canonical spec; the implementation lives in `internal/ui/`.

## Brief

When babysitting several agent sessions you want to always be working the *oldest blocked* one. Today, leaving a session (Ctrl+G) drops you to the board and you hunt for who's waiting. "Auto" mode automates that triage hop: while Auto is on, leaving a session jumps you straight to the session that has been **waiting the longest**.

## Behavior

- **Auto is a board-level toggle**, key `a` (in-memory, default off, session-only). Flipping it shows `Auto-attach: on/off`.
- **The behavior fires on un-attach** (Ctrl+G, the existing "leave this session" key):
  - Auto **off** → return to the board (unchanged).
  - Auto **on** → jump to the oldest-waiting peer; if none qualifies, fall through to the board.
- **"Waiting the longest" = FIFO** by `ticket.StatusChangedAt` — the session that *entered* `waiting` earliest wins. `StatusChangedAt` is re-stamped each time a session transitions back into `waiting`, so an actively-working interlude legitimately resets its clock ("went into waiting the longest time ago").
- The jump reuses the cycle-attach modal so the target's live PTY content shows immediately.

## Keybinding decision (the ticket's open question)

The card asked: *another ctrl-command, or a toggle mode on Ctrl+G?* Resolved by **splitting the two concerns**:

- **Toggle = board key `a`.** Rejected overloading Ctrl+G as the toggle: Ctrl+G means "leave → board", and remapping it to "leave → jump" makes the board unreachable while anything waits — yet the board is the only stable surface to turn Auto *off*. That's a modal trap. Agent-view ctrl-combos are the scarce resource (Ctrl+[ ≡ Esc; Ctrl+]/Ctrl+\ taken by cycling), so a board *letter* is the cheap, coherent home. `a` is focus-gated (sidebar-focused `a` still = Add project).
- **Action = Ctrl+G, parameterized by the flag.** Ctrl+G keeps its literal meaning; only its destination changes. Auto-on with an empty waiting-set lands on the board — the always-available off-ramp, so no separate "exit Auto" gesture is needed.

## Candidate set & ranking

A peer qualifies iff: live pane (Attached/Unattached); `AgentStatus == waiting` (the activity-overridden value the poll writes back, so it matches what the card shows); not the session being left (else you'd re-attach to it — inescapable loop); non-nil `StatusChangedAt`; and **not held by another TUI client**. Pick min `StatusChangedAt`; ties resolve to board order.

### Multi-TUI takeover mitigation
A `List()` snapshot at jump time builds the set of sessions whose `AttachedClient` is another client; those are skipped. Because the chosen target is provably free, the attach is a plain `Attach`, never a Takeover — so Auto never yanks a session away from a second TUI. Degrades to a no-op filter when the daemon is unreachable.

## Deferred (explicit non-goals for v1)

- **Board-idle auto-jump** when a session enters `waiting` while you sit on the board — autonomous focus-stealing; also what would make multi-TUI takeover severe. Strictly un-attach-triggered for now.
- **Config persistence / settings cascade** — session-only flag; a persisted auto-pilot is a multi-TUI footgun.
- **Scheduler / age-weighting / skip-recently-visited** — plain FIFO only.
- **A new `ModeAuto`** — Auto is a one-bit flag reinterpreting one keystroke, not a top-level mode.

## File anchors

- `internal/ui/model.go`: `autoAttach` field; `toggleAutoAttach` (board `a`); `oldestWaitingPeer(attachedElsewhere)`; `sessionsAttachedElsewhere()`; `focusAndPromptAttach()` (shared by Auto and `cycleUnattachedSession`); the `terminal.ExitFocusMsg` branch in `handleAgentViewMode`.
- `internal/ui/view.go`: `contextualHints` (footer reflects Auto state) + `renderHelp` (help modal).
- `internal/ui/auto_attach_test.go`: selector + Ctrl+G-branch tests.

<!-- openkanban:card-notes start -->
## Notes (from openkanban card)

when the user un-attaches. also scope whether it should be another crtl-command, or a toggle mode on ctrl-g
<!-- openkanban:card-notes end -->
