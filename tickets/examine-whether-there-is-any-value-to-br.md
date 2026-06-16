# examine whether there is any value to bringing in this PR

## Brief

Compare URL: https://github.com/cmeid/openkanban/compare/main...TechDufus:openkanban:main

The fork is 147 commits ahead of TechDufus's `main` and was severed on 2026-06-13; cmeid/openkanban is now canonical. TechDufus's branch is 1 commit ahead — a single `fix(core): address project review defects` patch touching 14 files. Question: any of it worth porting?

This ticket is read-only research; the deliverable is the assessment below. Real porting (if any) belongs in separate, narrowly-scoped follow-up tickets the user opens after reading this.

## Assessment

### Findings

Each row was verified directly against the worktree at the cited file:line.

| # | TechDufus fix | Current state in cmeid/openkanban | Recommendation |
|---|---|---|---|
| 1 | `pane.go Start()` holds `p.mu` while invoking the blocking read Cmd | **Already fixed independently.** `internal/terminal/pane.go:373–450` releases `p.mu` at line 449 before invoking `readCmd()`. Comments at 376–380 explain why no `defer Unlock`. | Skip. |
| 2 | opencode `queryOpencodeAPIOnPort` treats all sessions on a port as one global state | **Bug exists exactly.** `internal/agent/status.go:258` keys the cache by `opencode-port:%d` with no session; line 288 returns `AgentWorking` if *any* session on the port is busy. Done ticket `eb80172b` ("ensure that status of sessions is correct") was about a Claude session — different code path, no conflict. | **Port.** Clean, isolated change. |
| 3a | `LoadGlobalTicketStore` silently skips projects that fail to load | Fork still `continue`s on per-project errors (`internal/project/tickets.go:338–339`). No memory or commit records this as deliberate, but the leniency is consistent with the fork's "TUI keeps working through per-project corruption" posture (`feedback_openkanban_store_volatile`). | Don't port without asking — the leniency is plausibly intentional. |
| 3b | `GlobalTicketStore.RemoveProject` orphans the project's tickets in `g.allTickets` | **Bug exists.** `internal/project/tickets.go:515–537` deletes `g.projects[id]` and `g.ticketStores[id]` but never walks `g.allTickets` to remove that project's entries. Aggregate queries (`All()`, `Count()`, `GetByStatus()`) still see the orphans. | **Port.** Small fix, 3-line loop. |
| 3c | Legacy JSON migration swallows ReadFile/WriteFile errors | N/A — fork uses per-ticket markdown. The JSON migration path TechDufus's PR touched isn't the migration the fork runs. | Skip. |
| 4 | `CreateWorktree` reuses an existing worktree path without checking it's on the requested branch | **Bug exists** and is explicitly documented as a deferred concern. `reference_openkanban_worktree_fork_point.md:18` from the 2026-06-14 fork-point investigation: *"the path-existence check at `worktree.go:39-42` (returns an existing worktree if its `.git` file is valid, without checking the branch matches)… deliberately left for a focused fix; revisit if cross-contamination reappears in a different shape."* TechDufus's commit implements exactly that fix. | **Port.** Strongest "we already said we'd do this" candidate. |
| 5 | `validateOpencode` accepts port 0 | **Bug exists.** `internal/config/validate.go:209` uses `c.Opencode.ServerPort < 0`; the error message says "must be between 1 and 65535" but 0 passes silently. No memory or commit assigns "0 = disabled" semantics to opencode port. | Cheap port (one character: `<` → `<=`). Worth bundling with #2. |
| 6 | `cmd/config.go` validate calls `os.Exit(1)` instead of returning an error | **Bug exists** — `cmd/config.go:47` still calls `os.Exit(1)`. The only practical effect is that cobra can't print its usage footer and tests have to fork to assert exit codes. | Low value; defer unless touching `cmd/config.go` for other reasons. |
| 7 | `model.go handleKey` doesn't route ModeFilter keys to `handleFilterMode` | **Already present.** `internal/ui/model.go:1155–1156` routes `case ModeFilter: return m.handleFilterMode(msg)`. | Skip. |
| 8 | `confirmDeleteProject` calls `projectRegistry.Delete` before `globalStore.RemoveProject` | TechDufus's specific fix doesn't apply. The fork's `RemoveProject` (`internal/project/tickets.go:537`) itself calls `g.registry.Delete(id)`, so the UI at `internal/ui/model.go:2162–2168` invokes `Delete` twice — different bug, different fix. | Note as a separate latent bug; don't port the PR's diff as-is. |
| 9 | Makefile `test-integration` runs every test, not just `TestIntegration_` | Bug exists at `Makefile:16` (`-tags integration ./...` with no `-run` filter). | Cosmetic; port only if integration test count grows. |

### Recommendation

**Don't merge the PR.** Three items have real ongoing value and are best handled as small dedicated tickets that read the upstream patch as a reference but rewrite against current code:

- **#2** opencode per-session scoping — genuine functional bug in status detection.
- **#3b** `RemoveProject` ticket-orphan cleanup — state-consistency bug.
- **#4** `CreateWorktree` branch-mismatch reuse — explicitly aligns with the documented deferred-fix note in `reference_openkanban_worktree_fork_point.md:18`. Strongest "already on the list" candidate.

Minor items (#5 port 0, #6 `os.Exit`, #9 Makefile `-run` filter) can ride along on the same ticket(s) when convenient. None of them block anything.

Separately, **#8** surfaces a fork-specific bug not addressed by TechDufus's PR: the double `registry.Delete` in `confirmDeleteProject`. Worth its own ticket if pursued.

No prior decision in any ticket, commit message, or memory contradicts any of the recommended ports.

<!-- openkanban:card-notes start -->
## Notes (from openkanban card)

https://github.com/cmeid/openkanban/compare/main...TechDufus%3Aopenkanban%3Amain
<!-- openkanban:card-notes end -->
