# UI spinner for long-running daemon ops (startup reconcile, attach retry, resync)

<!-- openkanban:card-notes start -->
## Notes (from openkanban card)

## Brief

Add a user-visible "in-flight operation" indicator (spinner + label) for any daemon RPC or background reconcile that may take more than ~500ms. The TUI today goes silent during operations that can run for 10–30 seconds — startup daemon reconcile, attach-with-retry, periodic resync, Spawn — and users see either a static `ModeSpawning` label or nothing at all. Reviewers flagged this in two recent PRs (#33 / the in-flight #PR-3 + #PR-4 for the same bug catalog) and the symptom is consistent enough across them that a general UX affordance, not per-call hacks, is the right shape.

This is a UI/UX ticket — no daemon-side changes. The infrastructure already exists: `m.spinner` is wired and `m.spinner.Tick` is dispatched during `ModeSpawning` today. The gap is (a) extending coverage to the other long-running paths, (b) naming the operation in plain text, and (c) making the worst offender — startup reconcile — actually paint something before it blocks.

## Why this matters

- **Startup reconcile** (`internal/ui/daemon_resync.go:listSessionsWithRetry`) is called synchronously from `NewModel` with a 3 × 10s retry budget. On a slow or dead daemon the user sees a blank terminal for up to ~31s with no feedback before `Init()` returns. A code-reviewer flagged this as NEEDS_REVISION on the resync PR: "30s silent startup block is the only thing that rises above polish."
- **Attach retry** (`internal/ui/model.go:retryAttach`, schedule `5s + 0.2s + 3s + 0.4s + 3s = ~11.6s`) runs after Spawn under the existing `ModeSpawning` label. The label says "Spawning…" but Spawn already returned; the actual blocker is Attach. Risk-assessor on PR-3 called the 12s misleading-label window "Medium" severity.
- **Periodic 30s resync tick** is silent by design (background), but if the daemon goes slow and a tick stretches across multiple seconds, the UI gives no hint anything is happening. Not user-blocking, but worth a footer indicator so a stuck UI is distinguishable from an idle UI.
- **Spawn itself** can take 2–5s on first-fork. Today's `ModeSpawning` splash handles that case, but the label could be more specific (currently a generic spinner).

The deeper principle: any TUI operation that depends on a daemon RPC or fork should have a "we know you're waiting, here's what we're doing" affordance — silence reads as "broken" to the user, even when the system is functioning correctly.

## How (proposed approach — not prescriptive)

The codebase already has the building blocks:

- `m.spinner` of type `spinner.Model` (bubbles/spinner package). Tick driver: `m.spinner.Tick`.
- The `ModeSpawning` arm in `View()` renders a centered overlay with a spinner and a fixed label.
- `m.notification` is the status-bar one-shot toast for transient messages.

Three deliverables:

### 1. Generalize the in-flight indicator

Introduce a `m.inflightOps map[string]inflightOp` (or similar) where each entry carries:
- A short label ("Attaching", "Reconciling", "Spawning", "Daemon resync")
- The start time (for the elapsed-counter that kicks in after 3s)
- Optionally a cancel func so Esc can abort cancellable ops

Render a footer row (above the status bar?) showing the active op when len > 0. Spinner glyph rotates via the existing `m.spinner.Tick`. Elapsed counter renders once an op exceeds 3 seconds — keeps the UI calm during the common fast case.

### 2. Wire the existing long-running paths

- **Startup reconcile** — biggest UX win. The blocking call in `NewModel` should move to an async `tea.Cmd` so `View()` can paint the spinner before the first daemon call returns. This is the structural change called out in PR-4's code review. Concrete shape:
  - `NewModel` returns immediately with `m.inflightOps["startup-reconcile"] = {label: "Contacting daemon", started: now}`.
  - `Init()` dispatches a `startupReconcileCmd` that runs `listSessionsWithRetry`.
  - On result, a `startupReconcileMsg` handler applies the reconcile and clears the in-flight entry.
  - On failure, the existing notification path fires (already in place from PR-4).
- **Attach retry** — wrap the `retryAttach` schedule in an in-flight op. Switch the label from "Spawning" to "Attaching" once Spawn returns, so the user sees the right thing during the ~11s window. The `ModeSpawning` splash can stay; the label inside it is what changes.
- **Periodic resync** — add a tiny footer breadcrumb only when a tick has been outstanding > 2s. Most ticks complete in <500ms; only surface when slow.
- **Spawn proper** — keep current label but pull it through the same map so the rendering path is uniform.

### 3. Tests

Pattern is already established in `internal/ui/tui_status_sync_test.go` and `internal/ui/daemon_resync_test.go` — fake `daemonGuardAPI` controls timing, then assert on Model state. Add cases:
- Startup reconcile shows "Contacting daemon" label before the first List returns; clears on success.
- A `retryAttach` failure cycle shows "Attaching" not "Spawning" once spawn returns.
- The elapsed counter doesn't render before 3s.

## Acceptance

- [ ] Any daemon RPC or local reconcile lasting > 500ms renders a labelled spinner.
- [ ] Operation name is meaningful (not generic) — startup vs attach vs resync vs spawn are visually distinguishable.
- [ ] Startup reconcile no longer blocks first paint — the UI is interactive (Ctrl+C exits, view renders) within ~50ms of launch even when the daemon is dead.
- [ ] Elapsed-time counter appears after 3 seconds, not before.
- [ ] Esc cancels operations that are cancellable (where context plumbing already exists); non-cancellable ops surface "press Ctrl+C to abort openkanban" hint instead.
- [ ] `go test ./internal/ui/... -race -count=1` green.
- [ ] Manual: launch openkanban with the daemon socket blocked (e.g. `mv ~/.openkanbankd.sock ~/.openkanbankd.sock.bak`) and verify the TUI paints something useful for the duration of the retry window.

## Must NOT

- Don't introduce new blocking paths — anything user-visible must be a `tea.Cmd`, not a synchronous call from `NewModel` or `Update`.
- Don't replace the existing `m.spinner` infrastructure — extend, don't rewrite.
- Don't show the spinner for sub-500ms operations — would flicker on every keystroke that triggers a fast RPC.
- Don't reintroduce a daemon-side dependency on UI state — the RPCs stay daemon-agnostic; the spinner is purely client-side rendering.
- Don't bundle this with other UI work (theme tweaks, layout, etc.) — keep the diff scoped to the in-flight indicator.

## File anchors

- `internal/ui/model.go` — `NewModel` (startup reconcile call site), `Update` (tea message routing), `m.spinner` field, `ModeSpawning` state, `retryAttach` / `attachWithRetry` helpers (added by PR-3).
- `internal/ui/daemon_resync.go` — `listSessionsWithRetry`, `daemonResyncTickMsg`, `daemonResyncMsg` handlers (added by PR-4).
- `internal/ui/view.go` — `renderStatusBar`, the `ModeSpawning` overlay block, footer composition.
- `internal/ui/daemon_subscribe.go` — `handleDaemonSessionEvent` ("exited" event handler).
- `internal/ui/CLAUDE.md` — has a "Status-mutation wrap-up" section that this work would complement; consider adding a sibling "In-flight operations" section.

## Context (read these)

- `[[openkanban-tui-status-wrapup]]` — the TUI/CLI symmetry pattern from PR #33; the in-flight indicator should follow the same `m.guardAPI` test seam discipline.
- `[[openkanban-daemon-spawn-dedup]]` — PR #34's idempotent Spawn means client-side spawn flow is now retryable without daemon-side consequences; the spinner just needs to survive the retry, not the user "fixing" anything by mashing keys.
- `[[guard-api-is-the-ui-daemon-seam]]` — when adding new daemon-RPC-driven UI states, route through `daemonGuardAPI`.

The two PRs that exposed this gap (#PR-3 client-spawn-discipline, #PR-4 startup-resync — both currently in code review with NEEDS_REVISION on the silence point) should be reviewed before starting this ticket. They illustrate the exact code shapes that need spinning.

## Soft dependencies

- This ticket is partially blocked by the merged form of PR-3 and PR-4. Wait until both are on `main` so the call sites this ticket wires the spinner into are stable.
- If `daemonGuardAPI` is split into smaller interfaces (a flagged follow-up from PR-3's risk assessor), the test pattern here will adapt — but the production code shouldn't depend on that split.
<!-- openkanban:card-notes end -->
