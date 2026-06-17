# openkanbankd stuck-daemon forensics — 2026-06-17

Daemon PID 16959, `openkanbankd daemon --persistent`, started **Jun 16 15:52:40**.
`daemon list` reports "not responding" (fast-fails 5s via client watchdog), process alive.

## Headline: this is a STALE PRE-FIX BINARY, not a fix failure

- Running process started **Jun 16 15:52:40**.
- Lock-ordering fixes landed **Jun 17 18:41–19:34** (~27–28h later):
  - `b8e49d4` fix(terminal): release pane lock across PTY write
  - `d1316af` fix(terminal): fix drain teardown deadlock
  - `89d9766`/`3711586`/`e003239` deadlock-proof writes + idempotent teardown
- All four are ancestors of HEAD `7796f20`. Current source: `WriteInput`
  (pane.go:1314) is non-blocking (select/default → `ErrInputBackpressure`);
  `Size` (pane.go:413) is lock-free. **The cascade cannot form in today's source.**
- The running daemon executes the two anti-patterns the fix forbids (per
  internal/terminal/CLAUDE.md): blocking `os.File.Write` under `p.mu`, and
  `Pane.Size` taking `p.mu` while the server holds `sessionsMu.RLock`.

## The deadlock cascade (from the goroutine dump in daemon.log ~line 55089)

Server RWMutex @ 0x209e5c1e22ec; Pane mutex @ 0x209e5dba2aa8 (session 0x209e5c284540).

1. **Keystone — goroutine 342 (77 min, syscall write):** `handleAttach → binaryLoop
   (attach.go:297) → Pane.WriteInput (pane.go:942) → os.File.Write` blocked writing
   7328 bytes to a PTY master (attached claude child stopped draining stdin). Holds
   the pane mutex.
2. **goroutine 336** (`handleOutput`, the output drain) blocks on that pane mutex →
   child output no longer drained.
3. **goroutines 222 & 443** in `handleList` hold **Server.RLock** (server.go:985)
   then call `Session.Info → Pane.Size` (pane.go:293) → block on the pane mutex
   **while holding the RLock**. ← bridge from per-pane stall to global wedge.
4. **goroutines 474/444/419/475/498/320** want `Server.Lock` (write) via
   `handleTicketDone` (server.go:1057) → blocked behind the held RLock.
5. **Go RWMutex writer-priority:** with a writer queued, all new `RLock()` block →
   every `handleList` (476,462,280,480,479,478,477,420,421,422) and
   `cleanupViewersForClient` (352,369,455) hang → `daemon list` "not responding".

## Findings / unfixed gaps

1. **Update does not restart the persistent daemon (the real bug).** Fixes are in
   git HEAD but absent from BOTH the running process AND the installed bundle binary
   (`a488e76`, built 11:54, pre-fix). `openkanban update`/`install.sh` rebuilds the
   binary but nothing restarts the `--persistent`/launchd daemon, and nothing warns
   that daemon-side fixes need a restart. The watchdog (`a488e76` "arm TUI stall
   watchdog") is **detect-only** — detects the wedge, never recovers it.
   → A naive `kill -9 16959` would have launchd respawn `a488e76` = STILL vulnerable.
   Must reinstall from HEAD (`7796f20`) BEFORE restarting.

2. **No server-side fast-fail/backpressure under wedge.** The wedged daemon keeps
   `accept()`ing connections and spawning `handleConn` goroutines that immediately
   block on RLock → leaked goroutines + unix-socket fds (48 held, climbing ~1 per TUI
   reconcile, unbounded toward fd limit). `cleanupViewersForClient` (disconnect path)
   also needs the RLock, so disconnect cleanup is wedged too → sockets never close.

3. **Un-killable kernel-stuck child.** PID 28836 (`(claude)`, `?Es`, since 10:13)
   survived `kill -9` (returned 0, process persists), has **zero open files** (kernel
   already closed its fds), `sample` can't attach. Stuck in the final kernel exit
   path — un-reapable, will clear only on reboot. Not even a zombie, so the daemon's
   wait() will never see it.

4. **Zombie accumulation.** 10 unreaped zombie children — teardown/`watchSessionExit`
   can't complete because it needs the starved lock.

## Live children at time of investigation (all parented by wedged 16959)
- 6 live `Ss+` claude ticket sessions: 55465, 19226, 21455, 24055, 35165, 44091
- 1 un-killable exiting: 28836
- 10 zombies

## Specimen evolution (fresh SIGUSR1 dump 22:19 vs 16:57)
- SIGUSR1 dumps stacks on the LIVE wedged daemon without restarting it (runtime.Stack
  needs only the scheduler, not app locks). Usable live-introspection lever.
- Goroutines: 69 (16:57) → 95 (22:19) = +26 in 5.5h (~5/hr), dominated by 41 stuck
  `RWMutex.RLock`. The wedge is ACTIVELY LEAKING, not static.
- Keystone unchanged: goroutine 342 still blocked in the SAME `os.File.Write` to the
  SAME PTY master (session 0x209e5c284540), now **372 min**. Single stable root the
  whole time.
- goroutine 336 (`handleOutput`, the OUTPUT drain for that pane) also blocked on the
  pane mutex 372 min → daemon stopped draining that child's output too. Input-write
  stuck + output-drain stuck = the child can't make progress and becomes un-killable
  (the 28836 pathology).
- 12+ `handleTicketDone` goroutines stacked (18–356 min): the TUI repeatedly tries to
  wind these sessions down, and EACH attempt blocks on the starved write lock and
  leaks another goroutine. The system's own recovery path is swallowed by the wedge.

## VALIDATION AGAINST NEW SOURCE (HEAD 7796f20) — what's the goal of this exercise

### Specimen failure modes the new code already handles
- F1 Lock cascade: `WriteInput` non-blocking (inputCh + `ErrInputBackpressure`);
  `Pane.Size`/`Info` lock-free (atomic mirrors) → `handleList` never blocks under
  `sessionsMu.RLock` → cascade cannot form.
- F2 Teardown rescue: pane teardown does `signalGroup(SIGKILL)` + `f.Close()` OUTSIDE
  `p.mu` (pane.go:1196-1239) → unblocks the writer goroutine with EBADF.
- F3 `handleTicketDone` (server.go:1305): holds `sessionsMu` only for map scan+delete;
  `sess.Kill` runs off-lock in a goroutine → teardown can't starve dispatch.
- F4 Zombie reaping: indirectly fixed — not starving `sessionsMu` lets
  `watchSessionExit`'s map-removal proceed.
- F5 Status heartbeat: `broadcastActivity` uses `GetContentTry`/TryLock → one stuck
  teardown won't freeze the heartbeat for all sessions.
- F6 Stale-binary pickup: `watchBinaryStaleness` exits to pick up a new binary — but
  ONLY at zero sessions (see G1).

### Gaps that SURVIVE in the new code (the deliverable)
- **G1 (HIGH) No daemon self-recovery; persistent + non-draining sessions = permanent
  wedge.** `watchBinaryStaleness` defers to "sessions drain naturally", but a wedged
  session never drains — exactly the specimen. Recovery teeth exist only TUI-side
  (`internal/ui/stallwatch.go` recovery closure); the daemon has none. If the "no
  blocking under `sessionsMu`" invariant is ever violated again (new handler, slow
  syscall, third-party lock), the daemon wedges with zero self-recovery.
  → Add a daemon-side liveness watchdog: periodically probe dispatch liveness (timed
  `sessionsMu` TryLock / canary RPC round-trip); on sustained failure `os.Exit(non-0)`
  so launchd/systemd respawns. When binary is stale AND a wedge/non-drain is detected,
  escalate to force-restart instead of waiting for an impossible drain.
- **G2 (MED) Unbounded accept + no per-request timeout.** `Serve` loops
  `Accept(); go handleConn()` with no concurrency cap and no dispatch deadline.
  Specimen leaked +26 goroutines/5.5h with no bound. Any future blocking handler
  reproduces unbounded goroutine/fd/conn leak.
  → Bound in-flight handlers; give each dispatch a context deadline so a stuck handler
  abandons rather than pinning a goroutine+conn forever; ensure disconnect cleanup
  (`cleanupViewersForClient`) can't be starved by the same lock.
- **G3 (MED) Fire-and-forget Kill + un-reapable children.** `handleTicketDone` spawns
  `go sess.Kill()` untracked; if Kill can't unblock a child in uninterruptible kernel
  exit (28836: survives SIGKILL, zero fds, needs reboot), the goroutine + child leak
  silently with no daemon-level accounting.
  → Track in-flight kills with a timeout; on Kill-timeout log loudly + expose a health
  signal ("N sessions failed to reap"). Kernel-stuck children need reboot, but make
  them VISIBLE instead of silent.
- **G4 (LOW/STRUCTURAL) The no-wedge guarantee rests on an unenforced invariant.**
  "No Info-reachable accessor takes a lock; no handler blocks under `sessionsMu`" is
  documented in CLAUDE.md but enforced only by review. The specimen proves a violation
  is catastrophic and unrecoverable.
  → Add lock-discipline enforcement (a test/assertion that the Info path never touches
  `p.mu`), or restructure Info to read a structurally lock-free snapshot.

## DEEPER MINING (round 2) — additional evidence + new gaps

### Root structural insight: single global RWMutex → global blast radius
- One `sessionsMu` RWMutex gates ALL session access (~20 call sites in server.go).
  Readers: handleList/handlePeek/handleOwns/Session.Info/broadcastActivity/
  broadcastEvents/cleanupViewersForClient. Writers: handleSpawn re-check/handleKill/
  removeSession/handleTicketDone/cleanupViewers.
- Go RWMutex is writer-priority: **one stuck RLock-holder + one queued writer blocks
  EVERY future acquirer (readers AND writers) forever.** Hold-duration discipline does
  not help — *acquisition* starves. Blast radius of any single blocking-under-RLock is
  the whole daemon.
- Proof: `broadcastActivity` (server.go:631) uses the correct "snapshot under RLock,
  operate on copy" pattern (658-663, holds RLock only for an O(n) map copy) — yet it's
  **blocked on RLock for 348 min (status heartbeat dead 5.8h)**. The F5 GetContentTry
  hardening is defeated because the heartbeat can't even ACQUIRE sessionsMu.

### Daemon infra goroutine states (fresh dump)
- broadcastActivity (g12): BLOCKED on sessionsMu.RLock 348 min → heartbeat dead. (G5)
- broadcastEvents (g11): parked in select 321 min — alive but no data flowing.
- watchBinaryStaleness (g13): parked in select — alive, polling, detected staleness,
  chose to keep running (>0 sessions). Impotent by policy. (G1)
- 53 × Serve.func4 (handleConn workers) leaked + 1 attached session's binaryLoop/fanOut.

### Empirical G1 confirmation (staleness WARNs in daemon.log)
- This PID logged at 16:16:40 Jun 16 (24 min after start): "binary on disk is newer
  than running process (9 live session(s) still attached); **will exit when the last
  client disconnects** so the next launch picks up the update".
- That promise is HOLLOW in persistent mode — last-client-disconnect does NOT exit
  (CLAUDE.md). Misleading operator log + no actual auto-restart path. Ran stale ~30h.

### Resource cost of the wedge
- RSS 336 MB (~328 MB) for a kanban daemon owning a few PTYs — inflated by leaked
  goroutine stacks + leaked client structs + stuck-pane scrollback + buffered events.
- Only ONE stuck pane (0x209e5dba2a80) + ONE attached session drive the ENTIRE freeze.
  Blast radius catastrophically disproportionate to trigger (likely a ~7KB paste into
  a child not draining stdin — matches the historical paste-flood class).

### Multi-layer recovery blindspot (G6)
- daemonclient.New autostarts a DOWN daemon, but has NO handler for "daemon UP but
  WEDGED" (socket dials fine, RPC times out) → no kill+restart.
- stuck_modal.destroyStuckSession + reconcile both need RPCs a wedged daemon can't
  serve. Version-mismatch just prints "run `openkanban daemon restart`" (manual).
- TUI surfaces "restart openkanban to re-sync" on every failed reconcile — FUTILE: a
  TUI restart doesn't touch the persistent daemon, which is the wedged part.

## Consolidated gap list (plan foundation)
- **G1 (HIGH)** No daemon self-recovery; persistent + non-draining sessions = permanent
  stale wedge. Watchdog-with-teeth exists only TUI-side (and only for the TUI loop).
- **G2 (MED)** Unbounded accept + no per-request deadline → unbounded goroutine/conn/
  memory leak under any wedge (+26 goroutines/5.5h, 328 MB, 53 conns observed).
- **G3 (MED)** Untracked fire-and-forget Kill goroutines; un-reapable kernel-stuck
  children (28836) leak silently with no health accounting.
- **G4 (HIGH/STRUCTURAL)** No-wedge guarantee rests on an UNENFORCED invariant ("no
  RLock holder may block; Info accessors lock-free"). One violation = total freeze.
- **G5 (HIGH)** Single global RWMutex → global blast radius via writer-starvation; even
  correct snapshot-readers and the heartbeat die. Per-path non-blocking patches can't fix it.
- **G6 (MED)** Multi-layer recovery blindspot — no client/daemon path handles
  "up-but-wedged"; user-facing recovery advice is futile.

### Likely plan workstreams
- A. Kill the starvation vector (G5/G4): copy-on-write atomic session registry →
  lock-free reads (no acquisition to starve), COW writes under a small separate mutex.
- B. Daemon self-watchdog with teeth (G1): liveness probe (timed TryLock / canary RPC)
  → os.Exit(non-0) on sustained wedge → launchd respawn; escalate stale+wedged.
- C. Bound + time-box dispatch (G2): in-flight cap + per-request context deadline.
- D. Kill/reap accounting (G3): tracked kills w/ timeout + health signal for un-reapable.
- E. Client wedge-recovery (G6): detect up-but-wedged, offer/perform daemon kill+restart.
- F. Invariant enforcement (G4): lock-discipline test/assert on the Info path.

## Recovery options (after learning is captured)
- A. Reinstall from HEAD (`./scripts/install.sh`) → then `kill -9 16959` (launchd
  respawns FIXED binary). Loses all 6 sessions (PTYs close). 28836 may need reboot.
- B. Prune live sessions one-by-one to empirically confirm self-heal (old binary CAN
  recover once the keystone write clears via EIO), then reinstall+restart.
