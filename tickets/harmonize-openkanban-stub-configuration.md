# harmonize openkanban stub configuration with rules from main CLAUDE.de

## Brief

The spawn-time `init_prompt` openkanban prepends to each new Claude session (set in `~/.config/openkanban/config.json`, agent `claude`) restates an absolute "NEVER push / NEVER PR — every repo, every project, every context" rule that contradicts the actual gated-on-destination rule in the user's global `~/.claude/CLAUDE.md`. Spawned agents see the misquoted version, not the real one.

Make the global the only source of truth: strip rule-restating content from the local stub so it carries only OpenKanban-specific orchestration (brief location, status flow, scope confinement). Add a config-load-time check that warns when an `init_prompt` overlaps with section headers in `~/.claude/CLAUDE.md` or contains strong-rule markers (e.g., `HARD RULE`, `NEVER ... gh pr create`, `every repo, every project`) — so the same drift can't happen again silently.

## Acceptance

- Chris's `~/.config/openkanban/config.json` claude `init_prompt` contains zero strings matching `HARD RULE`, `NEVER ... push`, `Per Chris's global`, `every repo, every project`.
- A canonical copy of the harmonized prompt lives at `.openkanban/claude-init-prompt.tmpl` in the repo (git-protected against TUI rewrites of config.json).
- `go test ./... -race -count=1` and `go vet ./...` pass.
- Launching the binary against a poisoned `init_prompt` (any of the strong markers, OR an H2 section sharing a rule keyword with the global) prints a `Config warnings:` block to stderr naming `agents.<name>.init_prompt` and the offending text.
- Launching against Chris's actual harmonized config + real `~/.claude/CLAUDE.md` emits no `init_prompt` warnings.
- The check is warning-only (never `AddError`) and degrades silently when `~/.claude/CLAUDE.md` is absent — other openkanban users aren't burdened.

## Must NOT

- Do not modify `~/.claude/CLAUDE.md` — it is the source of truth.
- Do not push or open PRs as part of this work. Local commits only.
- Do not turn contradiction detection into a hard error or a startup blocker.
- Do not assume every openkanban user has `~/.claude/CLAUDE.md`. Missing file = silent skip, not an error.
- Do not invent new init_prompt template directives or new `ContextData` fields. Heuristic check is regexp/keyword, not semantic.
- Do not edit the other ticket's brief (`tickets/rework-openkanban-start-script-and-insta.md`).

## File anchors

- `.openkanban/claude-init-prompt.tmpl` — **new**, canonical harmonized init_prompt (git-tracked recovery copy)
- `~/.config/openkanban/config.json` — Chris's local stub; `agents.claude.init_prompt` field synced from the canonical template
- `~/.claude/plans/openkanban-config-snapshot-20260613-pre-harmonize.json` — pre-edit snapshot per `[[openkanban-store-volatile]]`
- `internal/config/validate.go` — added `validateInitPromptOverlap` + `findGlobalClaudeMd`; hooked into `validateDefaults` (~line 137) and `validateAgents` (~line 160)
- `internal/config/validate_test.go` — added `TestValidateInitPromptOverlap` (table-driven, 8 subtests), `TestValidateInitPromptOverlap_OnDefaults`, `TestValidateInitPromptOverlap_NeverError`
- `internal/config/config.go:23-48` — `defaultAgentPrompt` evaluated and left unchanged (no contradiction with global; de-duplication would hurt readability)
- `cmd/root.go:48-50` — existing warning-surface used by the new check (no edit needed)

## Context (read these)

- [[openkanban-personal-fork]] — the fork's diverged state, why the spawn-template approach exists at all
- [[openkanban-store-volatile]] — why a git-tracked canonical copy is required and the snapshot convention for editing `~/.config/openkanban/config.json`
- [[openkanban-session-linking]] — adjacent to the spawn flow; this ticket doesn't change session-linking semantics
- [[openkanban-dev-loop]] — commit hook (50/72/no-AI-attrib), branch convention, test command

<!-- openkanban:card-notes start -->
## Notes (from openkanban card)

currently there are mismatched rules when launching new sessions from tickets
<!-- openkanban:card-notes end -->
