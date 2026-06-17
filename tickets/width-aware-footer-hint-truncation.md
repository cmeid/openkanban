# width-aware footer hint truncation

<!-- openkanban:card-notes start -->
## Notes (from openkanban card)

# Width-aware truncation for keybinding footer hints

## Brief

After [[task/ensure-all-the-controls-are-documented]] (merged 2026-06-16, PR #47), the footer hint line in `internal/ui/view.go:contextualHints()` packs as many context-relevant keys as the mode allows. The richest case — `ModeNormal` with no ticket selected and no filter — is ~157 visible characters:

```
h/j/k/l nav │ n new │ e edit │ d del │ Space/- move │ s spawn │ K/J prio │ o sort │ w filter │ W global │ / search │ [ sidebar │ O settings │ ? help │ q quit
```

Plus the mode badge (`◆ NORMAL`, ~12 chars), the separator, and right-side notification badges. Total left-side: ~172 chars. This fits a 200+ col terminal cleanly. On a 160-col terminal it just barely fits. On a typical 80-col window (or a narrower split-pane setup), the right end clips silently — the user sees nothing past `[ sidebar` or so.

## Acceptance

- `contextualHints()` measures the available width (= `m.width` minus mode badge + sep + notification badge widths).
- When the rendered hint string would exceed that budget, drop hints from the right end (lowest-priority first) instead of clipping mid-key.
- Priority ordering must be deterministic and explicit (a list, not "whatever's left at the end").
- Hints that would push over the budget should be dropped silently — no ellipsis chrome, no warning. (Or: a single trailing `…` indicating more is hidden, accessible via `?`.)
- `?` remains in every mode-with-help variant; never drop it. The whole point is "press `?` for the rest."
- `q quit` is also a candidate to always keep (universal exit).
- Drop targets in default-normal order (lowest priority first): `q quit`, `O settings`, `[ sidebar`, `W global`, `o sort`, `w filter`, `K/J prio`, ...
- For other modes (filtered, agent view, ticket form, etc.) the same idea applies — define a priority list per mode.

## Must NOT

- Don't introduce per-key visibility flags as a config option; the priority order should be a code-level decision.
- Don't try to be clever with abbreviation (e.g. "search" → "src"); just drop hints whole.
- Don't change which keys actually work — this is purely render-side.

## File anchors

- `internal/ui/view.go:711` — `contextualHints(hintStyle, sep)` — extend each mode branch with a priority-ordered key list and a width-aware accumulator.
- `internal/ui/view.go:683` — call site; `m.width` is the terminal width.
- `internal/ui/view.go:704-708` — left/right join + spacing; can be the reference for "available width" math.

## Verification

- Manual: resize terminal to 80 / 120 / 160 / 200 cols and confirm:
  - 200 cols: full hint visible
  - 160 cols: same or near-same
  - 120 cols: tail keys (settings, sidebar, etc.) drop progressively
  - 80 cols: only the core nav + `? help` (or similar) remain; no mid-character clipping
- Unit test: render the hint string for each mode at varying widths and assert specific keys are present/absent at known cutoffs.
- `go test ./internal/ui/...` clean.

## Why this is deferred

The original "document all controls" ticket scoped to documentation only. Width-responsive truncation is rendering logic that affects how the documentation surfaces — a separate concern worth its own design pass. Today the clipping is a known risk noted in PR #47's plan; if it actually bites someone working on a narrow split, this ticket becomes urgent.

## Soft deps

None. Isolated to `contextualHints()`.

## Context

- Origin ticket: [[task/ensure-all-the-controls-are-documented]] / PR #47
- Memory: [[reference_openkanban_keybinding_doc_surfaces]] — the two-surface contract
- Memory: [[feedback_complete_both_surfaces_and_future_proof]] — the audit pattern
- Memory: [[bubbletea_v1_ctrl_key_string_map]] — irrelevant here, but linked from the same area
<!-- openkanban:card-notes end -->
