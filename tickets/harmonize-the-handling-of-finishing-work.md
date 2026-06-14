# Harmonize the handling of finishing work in a ticket window to prevent all the same questions

## Brief

Several recurring "finishing work" flows ask the same questions or fail silently in ways that surface as user pain only much later. Two examples in scope:

1. **Branch deletion on ticket cleanup.** We have a design decision to auto-clean branches on ticket delete; agent sessions should know about it and how to use it without re-asking.
2. **Self-update silently no-op + local `main` drift.** Surfaced 2026-06-14 by Chris reporting "features I shipped aren't in the binary":
   - `cmd/launch_check.go:91-105` returns false (no prompt, no check) when `SourcePath` is empty. Binaries installed via plain `go install` (no ldflags) silently disable auto-update with no UI signal — even though `update.go:41-44` documents `go install` as a recovery path.
   - `cmd/update.go:101-128` (`ApplyUpdate`) runs `git pull --ff-only origin main` against HEAD. When the user is on a feature branch, the feature branch absorbs `origin/main`'s commits but local `main` ref doesn't move. Next checkout of `main` is stale; next `git checkout -b task/...` cuts from a stale base.

Find root cause (the shared bit: "finishing-work flows assume state the user actually has to maintain by hand") and address. Specific changes welcome but the unifying ask is to make finishing-work silent-success the default — never silent-failure.

## Acceptance

- On launch, when `SourcePath == ""` and stdout is a TTY and `Behavior.CheckForUpdatesOnLaunch == true`, the user sees a one-line notice that auto-update is disabled and how to re-enable it (`./scripts/install.sh`).
- After any `ApplyUpdate` run, `git rev-parse refs/heads/main` in the source clone equals `git ls-remote origin refs/heads/main`'s SHA — regardless of which branch was checked out. Exception: local `main` has commits not on `origin/main` (diverged / ahead) → leave it alone, never clobber.
- Test coverage in `cmd/update_test.go` for: feature-branch sync, on-main no-op (the existing pull advances main), diverged-no-clobber, detached-HEAD sync.
- The branch-deletion-on-cleanup flow doesn't ask the user a question that's already been answered by config; agent sessions know how to invoke the auto-cleanup path.
- `go test ./... -race -count=1` and `go vet ./...` pass.

## Must NOT

- Do not turn `SourcePath == ""` into a launch-time error or blocker — it's a warning, not a failure. Other openkanban users may be on plain `go install` deliberately.
- Do not propagate sync-helper errors up through `ApplyUpdate`. The helper is best-effort; the update itself succeeds even if local main can't be advanced (diverged, network blip, etc.).
- Do not change the `git pull --ff-only origin main` semantics inside `ApplyUpdate`. The sync helper is additive — it runs *before* the existing pull and operates only on `refs/heads/main`.
- Do not modify any other ticket's brief.

## File anchors

- `cmd/launch_check.go:91-105` — `shouldPromptForUpdate` gate; add the `SourcePath==""` warning surface here (or hoist to a wrapper).
- `cmd/update.go:101-128` — `ApplyUpdate`; insert sync-helper call before the existing pull.
- `cmd/update.go` (helper) — new `syncLocalMain(ctx, sourcePath)` near `runUpdate`; use `git fetch origin main:main` (ff-only by construction). Skip when `git symbolic-ref --short HEAD` returns `main`.
- `cmd/update_test.go:69-93` — existing `setupRepos` / `makeCommit` / `runGit` harness is reusable for the new tests.
- `internal/ticket/...` — branch-deletion-on-cleanup flow (locate during execution).

## Context (read these)

- [[openkanban-personal-fork]] — fork divergence; solo repo, no PR
- [[openkanban-store-volatile]] — canonical brief belongs here in repo `tickets/`, not in `~/.config/openkanban/tickets/`
- [[openkanban-dev-loop]] — install path, hook conventions
- [[openkanban-commit-and-push]] — "commit and push" on cmeid/openkanban means ff-merge to main; no PR ceremony

## Evidence captured 2026-06-14

- Chris's binary at `/Users/cmeid/golang/bin/openkanban` reported `commit: none / source: (release build)`; mtime `Jun 14 19:36`.
- `origin/main` was 6 commits ahead of the binary's effective state, including: `ff80b0d` (shift+enter), `9a02cb7` (OSC 9), `582b73c` (wheel scroll), `06e7871` (file-poll), `459441a`+`0007723` (TUI log redirect), `2619cfb` (chore).
- Local `main` and current feature branch were both at `3c7ba71`, behind `origin/main` (`2619cfb`) by 2 commits.
- Five running `openkanban` processes were all on the stale binary (1 daemon + 4 TUIs across projects).

<!-- openkanban:card-notes start -->
## Notes (from openkanban card)

Annoying questions are things like whether a branch should be deleteed etc. We have a design decision to auto-cleanup on deleting a ticket, so let's make sure that all agent sessions know that and know how to use it
<!-- openkanban:card-notes end -->
