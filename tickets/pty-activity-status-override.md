# PTY-activity override for stuck "waiting" status

## Problem

The TUI shows a ticket as `waiting` for the entire duration of a
long-running Claude Code tool execution, even when the agent is
actively producing output. Root cause is a gap in Claude Code's hook
coverage: `Notification` fires when a permission prompt appears
(status → `waiting`), but no hook fires between the user's approval
and `PostToolUse` (status → `working`). For a Bash that takes 60s, a
Task that spawns a long subagent, or any tool with a slow callee, the
file-based status stays pinned at `waiting`.

Symptom observed on 2026-06-16: tickets `asi04` and `add-sort-by-age`
both displayed `waiting` while their Claude sessions were actively
animating the "Cogitating…" spinner and streaming tool output.

## Fix

Layer PTY-activity detection over the existing hook-driven file. The
daemon already owns the PTY and the charm/x/vt emulator, so it can
timestamp every `vt.Write` and broadcast that to subscribers. The UI
uses the timestamp as an override: file says `waiting` + activity in
the last 60s → render `working`.

## Why not add more Claude Code hooks?

`Notification` fires *after* `PreToolUse` — adding `PreToolUse →
working` doesn't help because `Notification` then re-stamps `waiting`.
There is no Claude Code hook for "permission granted", so the gap is
structurally unclosable from the hook side. PTY activity also
incidentally covers `PreCompact`, long autonomous runs, and any
future lifecycle change.

## Why bytes-flowed and not grid-hash?

Cursor blinks are terminal-side (the local terminal handles them,
not the application — the daemon's PTY never sees blink bytes).
Claude's spinner emits bytes ~10 Hz throughout tool execution.
Permission prompts and the idle input prompt are static — no bytes
flow when truly idle. Hashing the visible grid per `handleOutput`
adds cost (1920-cell iteration under `p.mu`) without catching
anything bytes-flowed misses.

## Scope

Additive only. No behavior change for sessions where hooks are
firing correctly. The override is narrow: only `waiting → working`,
and only when `LastActivityAt` is non-zero and recent. Other file
states (`working`, `idle`, `completed`, `error`) pass through
untouched.
