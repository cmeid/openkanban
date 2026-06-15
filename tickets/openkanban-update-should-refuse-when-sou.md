# openkanban update should refuse when source clone is not on main

## Brief

`openkanban update` (and the launch-time prompt) silently produces a binary built from the wrong code when the source clone's working tree is on a feature branch. Demonstrated 2026-06-15: the primary clone at `/Users/cmeid/manifold/dev/openkanban` was parked on `task/fix-garbled-initial-render-on-session-at`; accepting the update left the binary at commit `4062811` (the feature branch's base) instead of moving to `origin/main`.

The recent fix `7df2587` ("sync local main before pull on feature branches") syncs `refs/heads/main` via `git fetch origin main:main` but doesn't change what tree `go install` runs against — `go install <SourcePath>` still builds whatever is on disk at `SourcePath`, which is the feature-branch tree.

## Goal

Detect the situation up front and **refuse** the update with an actionable message rather than silently producing a wrong-code binary.

## Approach

Add a precondition to `CheckForUpdates` in `cmd/update.go`. After resolving `localSHA`, check the source clone's current branch:

```go
branch, err := currentBranch(ctx, SourcePath) // git -C SourcePath symbolic-ref --short HEAD
switch {
case err != nil || branch == "":
    return UpdateStatus{
        Available: false,
        Reason: fmt.Sprintf(
            "source clone has detached HEAD — switch back first:\n"+
            "  git -C %s checkout main && git -C %s pull --ff-only\n"+
            "then re-run `openkanban update`", SourcePath, SourcePath),
    }, nil
case branch != "main":
    return UpdateStatus{
        Available: false,
        Reason: fmt.Sprintf(
            "source clone on branch %q, not main — switch first:\n"+
            "  git -C %s checkout main && git -C %s pull --ff-only\n"+
            "then re-run `openkanban update`", branch, SourcePath, SourcePath),
    }, nil
}
```

The existing flow already prints `Reason` and exits cleanly without updating — no new code paths needed in callers.

## Acceptance

- `openkanban update` from a feature-branch source clone: exits 0 (non-actionable, not error), prints the refusal message naming the branch and the fix command, leaves the binary untouched.
- `openkanban update` from a detached-HEAD source clone: same shape, different message.
- `openkanban update` from `main`, no upstream change → existing "up to date" path unchanged.
- `openkanban update` from `main` with an available ff → existing happy path, binary rebuilt.
- Launch-time prompt surfaces the new `Reason` the same way it surfaces existing non-actionable reasons ("up to date", "ahead", "diverged"). Verify visually after the change lands; likely no change required if the prompt just renders `Reason`.
- Unit tests in `cmd/update_test.go` cover the four cases above. Mock the git invocations like `7df2587`'s `syncLocalMain` tests do.

## Out of scope

- Decoupling the build entirely from the user's working tree (e.g. building from a managed worktree at `~/.cache/openkanban/build-worktree/` checked out at `main`). That's the deeper structural fix, but refusing-with-message addresses the failure mode that just bit us at zero risk to the user's checkout. Revisit only if "switch back to main" friction proves recurring.
- Auto-switching the user's branch on their behalf. Their working tree, their call.

## Context

Discussed in the session-filter ticket's wrap-up (2026-06-15). The session-filter feature landed on `origin/main` but `openkanban update` didn't bring it into the running binary because of this exact bug — the primary clone was on a feature branch, so `go install` rebuilt against the feature-branch tree instead of `main`.
