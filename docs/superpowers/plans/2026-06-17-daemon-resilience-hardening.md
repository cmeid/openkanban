# openkanbankd Resilience Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `openkanbankd` survive a single stuck PTY/session instead of freezing globally for hours, and recover itself (and the client) when it can't.

**Architecture:** Six workstreams in dependency order. **A** removes the single global `sessionsMu` RWMutex (replaced by a copy-on-write atomic registry: lock-free reads, COW writes) so one blocked reader can never starve every other RPC — shrinking blast radius from "whole daemon" to "one handler goroutine." **C** then bounds and time-boxes that one goroutine. **B** adds a daemon-side liveness watchdog that force-exits on a sustained wedge so launchd respawns (also the real fix for "stale binary never restarts"). **D** makes silent teardown leaks visible via accounting + a health RPC. **E** teaches the client to detect and force-restart an up-but-wedged daemon instead of printing futile advice. **F** locks the whole class with a regression test that wedges a pane and asserts the daemon stays responsive.

**Tech Stack:** Go 1.26, `sync/atomic`, `creack/pty`, `charmbracelet/x/vt`, cobra (CLI), bubbletea (TUI). Unix-socket framed JSON+binary RPC.

## Global Constraints

- **Worktree first:** This plan edits code in the shared primary clone. Per `openkanban/CLAUDE.md`, create a ticket + worktree (`openkanban ticket new --project openkanban --title "daemon resilience hardening"`) and do ALL work there. Never edit/commit/reset the primary clone.
- **Build/install:** `go build ./...` and `go test ./...` for iteration; never `go install .` (produces a guard stub). Use `./scripts/install.sh` only when installing.
- **Commit subjects ≤ 50 chars, body wraps at 72, NO AI attribution** (a commit hook rejects + unstages violations). Conventional-commit prefix counts toward 50.
- **Lock discipline (the invariant this plan defends):** no `Info`-reachable accessor (`Size`/`Running`/`PID`) may take `p.mu`; no RPC handler may block while holding a registry lock; PTY writes go through the per-pane writer goroutine, never synchronously under `p.mu`. (Documented in `internal/terminal/CLAUDE.md` + `internal/daemon/CLAUDE.md`.)
- **Test config isolation is mandatory:** any test that persists state calls `testutil.NewTestEnv(t)` or sets `OPENKANBAN_CONFIG_DIR`; `config.GuardHomeWrite` panics on writes under the real `~/.config`/`~/.cache`.
- **Preserve the 1:1 ticket↔session invariant** (`handleSpawn` dedup) and the **persistent-mode last-client-disconnect semantics** (no force-kill of live sessions on disconnect) — both load-bearing and covered by existing tests.

---

## File Structure

| File | Responsibility | Change |
|------|----------------|--------|
| `internal/daemon/registry.go` | COW atomic session registry type + methods | **Create** |
| `internal/daemon/registry_test.go` | Registry unit + concurrency tests | **Create** |
| `internal/daemon/server.go` | Migrate all `sessionsMu`/`s.sessions` access to the registry; add dispatch heartbeat + inflight counters; add health handler | **Modify** |
| `internal/daemon/watchdog.go` | Daemon self-watchdog: detect dispatch wedge → dump + `os.Exit(1)` | **Create** |
| `internal/daemon/watchdog_test.go` | Watchdog evaluate-logic tests (no real exit) | **Create** |
| `internal/daemon/protocol.go` | Add `HealthReq`/`HealthResp` wire types + `MsgHealthReq`/`MsgHealthResp` | **Modify** |
| `internal/daemon/sem.go` | In-flight handler semaphore + slow-handler deadline guard | **Create** |
| `internal/daemon/wedge_regression_test.go` | End-to-end: wedge a pane, assert List/Health still respond | **Create** |
| `internal/daemonclient/client.go` | `ForceRestart` + version-skew already handled | **Modify** |
| `internal/daemonclient/recover.go` | `ForceRestartDaemon(ctx)`: kill pidfile PID, wait socket gone, DialOrStart | **Create** |
| `internal/app/app.go` | On preflight wedge: attempt force-restart once before exit | **Modify** |
| `cmd/daemon.go` | `openkanban daemon health` subcommand | **Modify** |

---

## PHASE A — Copy-on-write session registry (kills G4/G5)

Removes the global RWMutex starvation vector. After A, every read path holds **no** lock (operates on an atomic snapshot); writes serialize on a small dedicated mutex and publish a new map via copy-on-write. A reader that later blocks (e.g. a stuck `sess.Info()`) blocks *alone* and cannot freeze other RPCs.

### Task A1: Create the registry type

**Files:**
- Create: `internal/daemon/registry.go`
- Test: `internal/daemon/registry_test.go`

**Interfaces:**
- Produces:
  - `type sessionRegistry struct{...}`
  - `func newSessionRegistry() *sessionRegistry`
  - `func (r *sessionRegistry) snapshot() map[string]*Session` — lock-free; callers MUST treat as read-only
  - `func (r *sessionRegistry) get(id string) (*Session, bool)` — lock-free
  - `func (r *sessionRegistry) len() int` — lock-free
  - `func (r *sessionRegistry) findByTicket(ticketID string) *Session` — lock-free
  - `func (r *sessionRegistry) store(id string, sess *Session)` — COW under writer mutex
  - `func (r *sessionRegistry) delete(id string)` — COW
  - `func (r *sessionRegistry) deleteIf(id string, want *Session) bool` — COW; deletes only if current entry == want; returns true if deleted
  - `func (r *sessionRegistry) storeIfNoTicket(ticketID, id string, sess *Session) (winner *Session, stored bool)` — COW; if a session already owns ticketID, returns it without storing
  - `func (r *sessionRegistry) drain() []*Session` — COW swap to empty map, returns the old values (for shutdown/cleanup)

- [ ] **Step 1: Write the failing test**

```go
// internal/daemon/registry_test.go
package daemon

import (
	"sync"
	"testing"
)

func TestRegistry_StoreGetLenDelete(t *testing.T) {
	r := newSessionRegistry()
	if r.len() != 0 {
		t.Fatalf("new registry len=%d want 0", r.len())
	}
	a := &Session{id: "a", ticketID: "t1"}
	r.store("a", a)
	got, ok := r.get("a")
	if !ok || got != a {
		t.Fatalf("get(a)=%v,%v want %v,true", got, ok, a)
	}
	if r.len() != 1 {
		t.Fatalf("len=%d want 1", r.len())
	}
	r.delete("a")
	if _, ok := r.get("a"); ok {
		t.Fatal("get(a) still ok after delete")
	}
}

func TestRegistry_SnapshotIsReadOnlyCopy(t *testing.T) {
	r := newSessionRegistry()
	r.store("a", &Session{id: "a"})
	snap := r.snapshot()
	r.store("b", &Session{id: "b"}) // must NOT appear in the earlier snapshot
	if _, ok := snap["b"]; ok {
		t.Fatal("snapshot reflected a post-snapshot write — not COW-isolated")
	}
}

func TestRegistry_DeleteIfOnlyMatching(t *testing.T) {
	r := newSessionRegistry()
	a1 := &Session{id: "a", ticketID: "t1"}
	a2 := &Session{id: "a", ticketID: "t1"}
	r.store("a", a1)
	if r.deleteIf("a", a2) {
		t.Fatal("deleteIf removed a different instance")
	}
	if !r.deleteIf("a", a1) {
		t.Fatal("deleteIf did not remove the matching instance")
	}
}

func TestRegistry_StoreIfNoTicket(t *testing.T) {
	r := newSessionRegistry()
	winner := &Session{id: "w", ticketID: "t1"}
	r.store("w", winner)
	loser := &Session{id: "l", ticketID: "t1"}
	got, stored := r.storeIfNoTicket("t1", "l", loser)
	if stored || got != winner {
		t.Fatalf("storeIfNoTicket=%v,%v want winner,false", got, stored)
	}
	fresh := &Session{id: "f", ticketID: "t2"}
	got, stored = r.storeIfNoTicket("t2", "f", fresh)
	if !stored || got != fresh {
		t.Fatalf("storeIfNoTicket(new)=%v,%v want fresh,true", got, stored)
	}
}

func TestRegistry_ConcurrentReadsNeverBlockWrites(t *testing.T) {
	// Race-detector smoke: hammer reads while writing. Run with -race.
	r := newSessionRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				_ = r.len()
				_, _ = r.get("a")
				_ = r.snapshot()
			}
		}()
	}
	for i := 0; i < 1000; i++ {
		r.store("a", &Session{id: "a"})
		r.delete("a")
	}
	wg.Wait()
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `unset GOROOT && go test ./internal/daemon/ -run TestRegistry -v`
Expected: FAIL — `undefined: newSessionRegistry`

- [ ] **Step 3: Implement the registry**

```go
// internal/daemon/registry.go
package daemon

import (
	"sync"
	"sync/atomic"
)

// sessionRegistry holds the daemon's live sessions behind a copy-on-write
// atomic pointer. Reads (snapshot/get/len/findByTicket) are lock-free: they
// load the current immutable map and never acquire a lock, so a reader that
// later blocks (e.g. a stuck Session.Info) cannot starve any other RPC.
// Writers serialize on writeMu, clone the map, mutate the clone, and publish
// it via v.Store — so a reader always sees a consistent, never-mutated map.
//
// This replaces the single global sync.RWMutex (sessionsMu) whose
// writer-priority semantics let one stuck RLock-holder + one queued writer
// freeze every subsequent acquirer. See docs/superpowers/plans/
// 2026-06-17-daemon-resilience-hardening.md (Phase A) for the incident.
type sessionRegistry struct {
	writeMu sync.Mutex
	v       atomic.Pointer[map[string]*Session]
}

func newSessionRegistry() *sessionRegistry {
	r := &sessionRegistry{}
	empty := map[string]*Session{}
	r.v.Store(&empty)
	return r
}

// snapshot returns the current session map. The returned map MUST be treated
// as read-only — it is shared with all other readers. Lock-free.
func (r *sessionRegistry) snapshot() map[string]*Session {
	return *r.v.Load()
}

func (r *sessionRegistry) get(id string) (*Session, bool) {
	m := *r.v.Load()
	s, ok := m[id]
	return s, ok
}

func (r *sessionRegistry) len() int {
	return len(*r.v.Load())
}

func (r *sessionRegistry) findByTicket(ticketID string) *Session {
	if ticketID == "" {
		return nil
	}
	for _, sess := range *r.v.Load() {
		if sess.TicketID() == ticketID {
			return sess
		}
	}
	return nil
}

// cloneLocked copies the current map. Caller must hold writeMu.
func (r *sessionRegistry) cloneLocked() map[string]*Session {
	old := *r.v.Load()
	next := make(map[string]*Session, len(old)+1)
	for k, v := range old {
		next[k] = v
	}
	return next
}

func (r *sessionRegistry) store(id string, sess *Session) {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	next := r.cloneLocked()
	next[id] = sess
	r.v.Store(&next)
}

func (r *sessionRegistry) delete(id string) {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	next := r.cloneLocked()
	delete(next, id)
	r.v.Store(&next)
}

// deleteIf removes id only if the current entry is exactly want. Returns
// true if a delete happened. Mirrors the watchSessionExit "delete only if
// still mine" guard.
func (r *sessionRegistry) deleteIf(id string, want *Session) bool {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	old := *r.v.Load()
	if cur, ok := old[id]; !ok || cur != want {
		return false
	}
	next := r.cloneLocked()
	delete(next, id)
	r.v.Store(&next)
	return true
}

// storeIfNoTicket stores sess under id unless a session already owns
// ticketID. Returns the existing owner (stored=false) or sess (stored=true).
// Replaces handleSpawn's WLock re-check race window — atomic under writeMu.
func (r *sessionRegistry) storeIfNoTicket(ticketID, id string, sess *Session) (*Session, bool) {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	if ticketID != "" {
		for _, existing := range *r.v.Load() {
			if existing.TicketID() == ticketID {
				return existing, false
			}
		}
	}
	next := r.cloneLocked()
	next[id] = sess
	r.v.Store(&next)
	return sess, true
}

// drain swaps in an empty map and returns the previous session values, for
// shutdown/cleanup. After drain the registry is empty.
func (r *sessionRegistry) drain() []*Session {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	old := *r.v.Load()
	live := make([]*Session, 0, len(old))
	for _, sess := range old {
		live = append(live, sess)
	}
	empty := map[string]*Session{}
	r.v.Store(&empty)
	return live
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `unset GOROOT && go test ./internal/daemon/ -run TestRegistry -race -v`
Expected: PASS (all five)

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/registry.go internal/daemon/registry_test.go
git commit -m "feat(daemon): add COW session registry"
```

### Task A2: Migrate Server to the registry

**Files:**
- Modify: `internal/daemon/server.go` (field decl line 59-60; all ~16 `sessionsMu` call sites)

**Interfaces:**
- Consumes: all `sessionRegistry` methods from A1.
- Produces: `Server.reg *sessionRegistry` replacing `Server.sessionsMu` + `Server.sessions`.

This is a mechanical-but-careful migration. Each old critical section maps to a registry call. The migration table below is exhaustive (every site the readers found):

| Old (server.go) | Old code | New code |
|---|---|---|
| field 59-60 | `sessionsMu sync.RWMutex` / `sessions map[...]` | `reg *sessionRegistry` |
| init (NewServer) | `sessions: map[string]*Session{}` | `reg: newSessionRegistry()` |
| watchBinaryStaleness 465 | RLock; `len(s.sessions)`; RUnlock | `s.reg.len()` |
| awaitSessionDrain 519 | RLock; `len(s.sessions)`; RUnlock | `s.reg.len()` |
| broadcastActivity 658-663 | RLock; copy map; RUnlock | `alive := s.reg.snapshot()` (read-only; do not mutate — drop the local copy loop) |
| handleList 1238-1244 | RLock+defer; range `s.sessions` → Info | `for _, sess := range s.reg.snapshot() { infos = append(...) }` (no defer/unlock) |
| handleKill 1249-1254 | Lock; lookup+delete | `sess, ok := s.reg.get(req.SessionID); if ok { s.reg.delete(req.SessionID) }` |
| handleSpawn 1059 | RLock; findByTicket | `existing := s.reg.findByTicket(req.TicketID)` |
| handleSpawn 1080-1093 | Lock; re-check + store / kill loser | `winner, stored := s.reg.storeIfNoTicket(req.TicketID, sess.ID(), sess); if !stored { kill sess; return winner info }` |
| handleTicketDone 1310-1320 | Lock; collect matches; delete each | snapshot, collect matches, then `for _, m := range matches { s.reg.deleteIf(m.ID(), m) }` |
| handleOwns 1373-1380 | RLock+defer; range | `for _, sess := range s.reg.snapshot() { ... }` |
| handleSetViewing 1426-1428 | RLock; lookup | `sess, ok := s.reg.get(req.SessionID)` |
| cleanupViewersForClient 1449-1454 | RLock; copy; RUnlock | `for _, sess := range s.reg.snapshot() { ... }` |
| handlePrepareExit 1501-1506 | RLock; range → Info | `for _, sess := range s.reg.snapshot() { infos = append(...) }` |
| handleShutdown 1529-1535 | Lock; collect; reset map | `live := s.reg.drain()` |
| handleLastClientDisconnect 1569 | RLock; `len` | `s.reg.len()` |
| cleanup 782-788 | Lock; collect; reset map | `live := s.reg.drain()` |
| watchSessionExit removeSession 1170-1189 | Lock; delete-if-mine + invariant log | `if s.reg.deleteIf(sessID, sess) { log "...removed" }`; then `if other := s.reg.findByTicket(ticketID); other != nil { log WARN invariant }` |
| findSessionForTicketLocked 1113-1127 | (helper) | DELETE — replaced by `reg.findByTicket` |

- [ ] **Step 1: Add a guard test that List stays responsive while a writer is mid-store** (the property A buys us)

```go
// internal/daemon/registry_test.go (append)
func TestRegistry_ReaderUnaffectedByConcurrentWriter(t *testing.T) {
	r := newSessionRegistry()
	r.store("a", &Session{id: "a"})
	release := make(chan struct{})
	writing := make(chan struct{})
	go func() {
		// Simulate a writer that holds writeMu briefly (COW is fast).
		close(writing)
		<-release
		r.store("b", &Session{id: "b"})
	}()
	<-writing
	// Reads must complete immediately regardless of the pending writer.
	done := make(chan int, 1)
	go func() { done <- r.len() }()
	select {
	case <-done:
	case <-timeAfter(2):
		t.Fatal("read blocked behind a pending writer")
	}
	close(release)
}
```

(Add the small helper `func timeAfter(sec int) <-chan time.Time { return time.After(time.Duration(sec) * time.Second) }` and `import "time"` if not present.)

- [ ] **Step 2: Run — expect PASS already** (registry reads are lock-free)

Run: `unset GOROOT && go test ./internal/daemon/ -run TestRegistry_ReaderUnaffected -race -v`
Expected: PASS

- [ ] **Step 3: Apply the migration table** above to `server.go`. Replace the struct fields, `NewServer` init, and every call site. Delete `findSessionForTicketLocked`. For `broadcastActivity`, replace the lock+copy block with `alive := s.reg.snapshot()` and use it read-only (do not delete entries from it; the memo-cleanup loops already build their own deletes keyed off `alive`).

- [ ] **Step 4: Build + run the full daemon suite**

Run: `unset GOROOT && go build ./... && go test ./internal/daemon/ -race`
Expected: PASS — existing tests (dedup, drain, lifecycle, owns-multimatch, remove-session-invariant) green. Note: `TestServerLifecycle_SpawnEcho` is a known pre-existing failure on main (see memory) — confirm it fails identically before/after, don't chase it.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/server.go internal/daemon/registry_test.go
git commit -m "refactor(daemon): swap sessionsMu for COW registry"
```

---

## PHASE B — Daemon self-watchdog (kills G1)

After A, a true wedge is far less likely, but the invariant is still unenforced (G4) and a stale binary still never restarts in persistent mode (G1). B adds a daemon-side liveness watchdog with teeth: on a sustained dispatch wedge it dumps stacks and `os.Exit(1)` so launchd respawns (into the new binary if one is on disk).

### Task B1: Dispatch heartbeat + inflight counters

**Files:**
- Modify: `internal/daemon/server.go` (Server struct; `dispatch`)

**Interfaces:**
- Produces on `Server`: `dispatchSeq atomic.Uint64`, `inflight atomic.Int64`, and `func (s *Server) dispatchStats() (seq uint64, inflight int64)`.

- [ ] **Step 1: Write the failing test**

```go
// internal/daemon/watchdog_test.go
package daemon

import "testing"

func TestDispatchStats_StartZero(t *testing.T) {
	s := &Server{}
	seq, inflight := s.dispatchStats()
	if seq != 0 || inflight != 0 {
		t.Fatalf("got seq=%d inflight=%d want 0,0", seq, inflight)
	}
}
```

- [ ] **Step 2: Run — FAIL** (`s.dispatchStats undefined`)

Run: `unset GOROOT && go test ./internal/daemon/ -run TestDispatchStats -v`

- [ ] **Step 3: Add fields + accessor + instrument dispatch**

Add to the `Server` struct (near line 60):
```go
	// dispatchSeq increments at the end of every dispatch() call; inflight
	// tracks handlers currently executing. The watchdog samples both: if
	// inflight>0 but dispatchSeq is frozen past the wedge threshold, the
	// daemon is stuck and must self-restart. Lock-free.
	dispatchSeq atomic.Uint64
	inflight    atomic.Int64
```
Add accessor:
```go
func (s *Server) dispatchStats() (uint64, int64) {
	return s.dispatchSeq.Load(), s.inflight.Load()
}
```
Wrap the body of `dispatch` (server.go:888). The simplest correct instrumentation is at the top of `dispatch`:
```go
func (s *Server) dispatch(c *clientConn, typeName string, raw json.RawMessage) {
	s.inflight.Add(1)
	defer func() {
		s.inflight.Add(-1)
		s.dispatchSeq.Add(1)
	}()
	switch typeName {
	// ... unchanged ...
	}
}
```
Note: `handleAttach` legitimately blocks for the session lifetime, so it will hold `inflight` high. The watchdog (B2) must therefore key on **dispatchSeq frozen** (no completions) rather than inflight alone — see B2.

- [ ] **Step 4: Run — PASS**

Run: `unset GOROOT && go test ./internal/daemon/ -run TestDispatchStats -v`

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/server.go internal/daemon/watchdog_test.go
git commit -m "feat(daemon): dispatch heartbeat counters"
```

### Task B2: Watchdog evaluate logic (pure, testable) + wiring

**Files:**
- Create: `internal/daemon/watchdog.go`
- Modify: `internal/daemon/watchdog_test.go`; `internal/daemon/server.go` (launch in `Serve`)

**Interfaces:**
- Produces:
  - `type wedgeSample struct { seq uint64; inflight int64; pendingRestart bool; nowNanos int64 }`
  - `type wedgeMonitor struct{...}` with `func (w *wedgeMonitor) evaluate(s wedgeSample) (exit bool, reason string)`
  - `func (s *Server) runWedgeWatchdog()` — ticker loop that samples, calls evaluate, and on `exit` dumps goroutines + `os.Exit(1)`.
- Consumes: `s.dispatchStats()` (B1), `s.pendingRestart` (under `stalenessMu`).

Detection rule (mirrors the TUI stallMonitor's "starved" shape): a wedge is when **work is queued but nothing completes** for longer than `wedgeThreshold`. Concretely: `inflight > 0` AND `seq` has not advanced for `wedgeThreshold`. Separately, if `pendingRestart` is set AND `seq` is frozen for `staleWedgeThreshold` (shorter), exit too — a stale binary that's also not completing work should not linger.

- [ ] **Step 1: Write failing tests for evaluate**

```go
// internal/daemon/watchdog_test.go (append)
func TestWedge_NoExitWhenProgressing(t *testing.T) {
	w := newWedgeMonitor(60, 30) // wedgeSeconds, staleWedgeSeconds
	sec := int64(1e9)
	// seq advances each tick → never a wedge even with inflight>0.
	w.evaluate(wedgeSample{seq: 1, inflight: 2, nowNanos: 10 * sec})
	exit, _ := w.evaluate(wedgeSample{seq: 2, inflight: 2, nowNanos: 80 * sec})
	if exit {
		t.Fatal("exit fired while dispatchSeq was advancing")
	}
}

func TestWedge_ExitOnFrozenSeqWithInflight(t *testing.T) {
	w := newWedgeMonitor(60, 30)
	sec := int64(1e9)
	w.evaluate(wedgeSample{seq: 5, inflight: 3, nowNanos: 10 * sec}) // baseline
	exit, reason := w.evaluate(wedgeSample{seq: 5, inflight: 3, nowNanos: 71 * sec})
	if !exit {
		t.Fatalf("no exit after %ds frozen with inflight>0", 61)
	}
	if reason == "" {
		t.Fatal("exit reason empty")
	}
}

func TestWedge_NoExitWhenIdle(t *testing.T) {
	w := newWedgeMonitor(60, 30)
	sec := int64(1e9)
	w.evaluate(wedgeSample{seq: 5, inflight: 0, nowNanos: 10 * sec})
	exit, _ := w.evaluate(wedgeSample{seq: 5, inflight: 0, nowNanos: 200 * sec})
	if exit {
		t.Fatal("exit fired on an idle daemon (inflight==0)")
	}
}

func TestWedge_StaleBinaryFrozenExitsSooner(t *testing.T) {
	w := newWedgeMonitor(60, 30)
	sec := int64(1e9)
	w.evaluate(wedgeSample{seq: 5, inflight: 1, pendingRestart: true, nowNanos: 10 * sec})
	exit, _ := w.evaluate(wedgeSample{seq: 5, inflight: 1, pendingRestart: true, nowNanos: 41 * sec})
	if !exit {
		t.Fatal("stale+frozen did not exit at the shorter threshold")
	}
}
```

- [ ] **Step 2: Run — FAIL** (`newWedgeMonitor undefined`)

Run: `unset GOROOT && go test ./internal/daemon/ -run TestWedge -v`

- [ ] **Step 3: Implement watchdog.go**

```go
// internal/daemon/watchdog.go
package daemon

import (
	"log"
	"os"
	"runtime"
	"time"
)

const (
	// wedgeCheckInterval is how often the watchdog samples dispatch stats.
	wedgeCheckInterval = 5 * time.Second
	// wedgeSeconds: inflight work that completes nothing for this long is a
	// wedge. Generous so a slow-but-progressing daemon is never killed.
	wedgeSeconds = 90
	// staleWedgeSeconds: a stale binary (pendingRestart) that also stops
	// completing work exits sooner — it has nothing to lose.
	staleWedgeSeconds = 45
)

type wedgeSample struct {
	seq            uint64
	inflight       int64
	pendingRestart bool
	nowNanos       int64
}

// wedgeMonitor decides, from successive dispatch-stat samples, whether the
// daemon is wedged (work queued, nothing completing) long enough to warrant
// a self-restart. Pure + injectable-time so the decision is unit-tested
// without a real os.Exit.
type wedgeMonitor struct {
	wedgeNanos      int64
	staleNanos      int64
	lastSeq         uint64
	lastSeqChangeNs int64
	primed          bool
}

func newWedgeMonitor(wedgeSeconds, staleWedgeSeconds int64) *wedgeMonitor {
	return &wedgeMonitor{
		wedgeNanos: wedgeSeconds * int64(time.Second),
		staleNanos: staleWedgeSeconds * int64(time.Second),
	}
}

// evaluate returns (exit, reason). exit=true means: force-restart now.
func (w *wedgeMonitor) evaluate(s wedgeSample) (bool, string) {
	if !w.primed || s.seq != w.lastSeq {
		w.primed = true
		w.lastSeq = s.seq
		w.lastSeqChangeNs = s.nowNanos
		return false, ""
	}
	// seq frozen since lastSeqChangeNs. Only a wedge if work is queued.
	if s.inflight <= 0 {
		return false, ""
	}
	frozen := s.nowNanos - w.lastSeqChangeNs
	if s.pendingRestart && frozen > w.staleNanos {
		return true, "stale binary wedged (no dispatch completion)"
	}
	if frozen > w.wedgeNanos {
		return true, "dispatch wedged (no completion with work in flight)"
	}
	return false, ""
}

// runWedgeWatchdog samples dispatch stats on a ticker and force-restarts the
// daemon if evaluate says it's wedged. Dumps every goroutine's stack to the
// log first (the postmortem), then os.Exit(1) so launchd/systemd respawns —
// picking up a new on-disk binary if one is present. Exits cleanly when the
// shutdown channel closes.
func (s *Server) runWedgeWatchdog() {
	mon := newWedgeMonitor(wedgeSeconds, staleWedgeSeconds)
	ticker := time.NewTicker(wedgeCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.shutdown:
			return
		case <-ticker.C:
			seq, inflight := s.dispatchStats()
			s.stalenessMu.Lock()
			pending := s.pendingRestart
			s.stalenessMu.Unlock()
			exit, reason := mon.evaluate(wedgeSample{
				seq:            seq,
				inflight:       inflight,
				pendingRestart: pending,
				nowNanos:       time.Now().UnixNano(),
			})
			if !exit {
				continue
			}
			buf := make([]byte, 1<<20)
			n := runtime.Stack(buf, true)
			log.Printf("openkanbankd: WEDGE WATCHDOG firing (%s); inflight=%d seq=%d. goroutine dump:\n%s",
				reason, inflight, seq, buf[:n])
			log.Printf("openkanbankd: exiting(1) for supervisor respawn")
			os.Exit(1)
		}
	}
}
```

- [ ] **Step 4: Run — PASS**

Run: `unset GOROOT && go test ./internal/daemon/ -run TestWedge -v`

- [ ] **Step 5: Wire into Serve** — add `go s.runWedgeWatchdog()` next to the other background goroutines (server.go ~344, after `go s.watchBinaryStaleness()`). Add a one-line comment.

- [ ] **Step 6: Fix the misleading staleness log** — in `watchBinaryStaleness` (server.go ~474), the WARN says "will exit when the last client disconnects" which is false in persistent mode. Make it mode-accurate:

```go
		if s.persistent {
			log.Printf("WARN: openkanbankd binary on disk is newer than running process (%d live session(s) still attached); persistent mode will NOT auto-restart — run `openkanban daemon restart` or rely on the wedge watchdog", liveSessions)
		} else {
			log.Printf("WARN: openkanbankd binary on disk is newer than running process (%d live session(s) still attached); will exit when the last client disconnects so the next launch picks up the update", liveSessions)
		}
```

- [ ] **Step 7: Build + commit**

Run: `unset GOROOT && go build ./... && go test ./internal/daemon/ -run 'TestWedge|TestDispatchStats' -v`
```bash
git add internal/daemon/watchdog.go internal/daemon/watchdog_test.go internal/daemon/server.go
git commit -m "feat(daemon): self-restart wedge watchdog"
```

---

## PHASE C — Bound + time-box dispatch (kills G2)

Caps in-flight handler goroutines (no unbounded leak) and time-boxes the short RPCs so a stuck one abandons instead of pinning a goroutine+connection forever. `handleAttach` is excluded (it legitimately blocks for the session lifetime).

### Task C1: In-flight handler semaphore with fast-reject

**Files:**
- Create: `internal/daemon/sem.go`
- Modify: `internal/daemon/server.go` (Serve accept loop ~395)
- Test: `internal/daemon/sem_test.go`

**Interfaces:**
- Produces: `type connSem struct{...}`; `func newConnSem(max int) *connSem`; `func (c *connSem) tryAcquire() bool`; `func (c *connSem) release()`; const `maxConcurrentConns = 256`.

- [ ] **Step 1: Failing test**

```go
// internal/daemon/sem_test.go
package daemon

import "testing"

func TestConnSem_BoundsAndReleases(t *testing.T) {
	s := newConnSem(2)
	if !s.tryAcquire() || !s.tryAcquire() {
		t.Fatal("first two acquires should succeed")
	}
	if s.tryAcquire() {
		t.Fatal("third acquire should fail at cap=2")
	}
	s.release()
	if !s.tryAcquire() {
		t.Fatal("acquire after release should succeed")
	}
}
```

- [ ] **Step 2: Run — FAIL**

Run: `unset GOROOT && go test ./internal/daemon/ -run TestConnSem -v`

- [ ] **Step 3: Implement sem.go**

```go
// internal/daemon/sem.go
package daemon

// maxConcurrentConns caps live client-connection handler goroutines. Well
// above any real fleet (a few TUIs + CLI probes); the cap exists only to
// bound a pathological leak (a wedge that accumulates stuck handlers). When
// full, the accept loop fast-rejects new conns rather than spawning an
// unbounded number of goroutines that each immediately block.
const maxConcurrentConns = 256

type connSem struct {
	ch chan struct{}
}

func newConnSem(max int) *connSem { return &connSem{ch: make(chan struct{}, max)} }

func (c *connSem) tryAcquire() bool {
	select {
	case c.ch <- struct{}{}:
		return true
	default:
		return false
	}
}

func (c *connSem) release() { <-c.ch }
```

- [ ] **Step 4: Wire into the accept loop.** Add `sem *connSem` to `Server`, init `newConnSem(maxConcurrentConns)` in `NewServer`. In `Serve` after `c := s.registerClient(conn)`:

```go
		if !s.sem.tryAcquire() {
			log.Printf("openkanbankd: connection cap (%d) reached — rejecting client %d", maxConcurrentConns, c.id)
			s.writeError(c, "server_busy", "daemon at connection capacity")
			conn.Close()
			s.unregisterClient(c)
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.sem.release()
			s.handleConn(c)
		}()
```

- [ ] **Step 5: Run + commit**

Run: `unset GOROOT && go test ./internal/daemon/ -run TestConnSem -v && go build ./...`
```bash
git add internal/daemon/sem.go internal/daemon/sem_test.go internal/daemon/server.go
git commit -m "feat(daemon): cap in-flight conn handlers"
```

### Task C2: Per-request deadline for short RPCs

**Files:**
- Modify: `internal/daemon/server.go` (`dispatch`)
- Test: `internal/daemon/dispatch_deadline_test.go`

**Interfaces:**
- Produces: `func (s *Server) runHandlerWithDeadline(name string, fn func()) bool` — runs fn in a goroutine; returns false if it exceeds `handlerDeadline`; const `handlerDeadline = 10 * time.Second`.

Apply only to the non-blocking short handlers (List/Owns/SetViewing/PrepareExit/CancelExit/Spawn/Kill/TicketDone/Peek/Hello/Subscribe). **Do NOT wrap `handleAttach`** — it blocks by design.

- [ ] **Step 1: Failing test**

```go
// internal/daemon/dispatch_deadline_test.go
package daemon

import (
	"testing"
	"time"
)

func TestRunHandlerWithDeadline(t *testing.T) {
	s := &Server{}
	if !s.runHandlerWithDeadline("fast", func() {}) {
		t.Fatal("fast handler reported as timed out")
	}
	// A handler that outlives the deadline returns false. Use a tiny
	// override via the package var so the test is fast.
	old := handlerDeadlineOverride
	handlerDeadlineOverride = 50 * time.Millisecond
	defer func() { handlerDeadlineOverride = old }()
	if s.runHandlerWithDeadline("slow", func() { time.Sleep(500 * time.Millisecond) }) {
		t.Fatal("slow handler should have reported timeout")
	}
}
```

- [ ] **Step 2: Run — FAIL**

- [ ] **Step 3: Implement** in server.go:

```go
const handlerDeadline = 10 * time.Second

// handlerDeadlineOverride lets tests shorten the deadline. Zero means use
// handlerDeadline.
var handlerDeadlineOverride time.Duration

// runHandlerWithDeadline runs fn (a short RPC handler) and returns true if it
// finished within the deadline. On timeout it returns false and leaves the
// handler goroutine running (it will finish or leak — the wedge watchdog and
// the conn-sem bound the worst case). The caller writes an "unresponsive"
// error to the client so it doesn't hang. Not for handleAttach (blocks by
// design).
func (s *Server) runHandlerWithDeadline(name string, fn func()) bool {
	d := handlerDeadline
	if handlerDeadlineOverride > 0 {
		d = handlerDeadlineOverride
	}
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		log.Printf("openkanbankd: handler %q exceeded %s — abandoning (client will get unresponsive error)", name, d)
		return false
	}
}
```

Then in `dispatch`, wrap the short handlers. Example for List:
```go
	case MsgListReq:
		var req ListReq
		_ = json.Unmarshal(raw, &req)
		var resp ListResp
		if s.runHandlerWithDeadline("list", func() { resp = s.handleList(c, req) }) {
			s.writeResp(c, MsgListResp, resp)
		} else {
			s.writeError(c, "daemon_unresponsive", "list handler timed out")
		}
```
Apply the same pattern to Spawn/Kill/TicketDone/Owns/SetViewing/PrepareExit/CancelExit/Peek/Subscribe/Hello. Leave `MsgAttachReq` calling `s.handleAttach(c, req)` unwrapped.

- [ ] **Step 4: Run + full suite + commit**

Run: `unset GOROOT && go test ./internal/daemon/ -run TestRunHandlerWithDeadline -v && go test ./internal/daemon/ -race`
```bash
git add internal/daemon/server.go internal/daemon/dispatch_deadline_test.go
git commit -m "feat(daemon): time-box short RPC handlers"
```

---

## PHASE D — Kill/reap accounting + health RPC (kills G3)

Makes fire-and-forget `Kill` goroutines and un-reapable children (the 28836 pathology) visible instead of silent.

### Task D1: Kill accounting counters

**Files:**
- Modify: `internal/daemon/server.go` (Server struct; `handleTicketDone` ~1337; `handleKill`)
- Test: `internal/daemon/kill_accounting_test.go`

**Interfaces:**
- Produces on `Server`: `inflightKills atomic.Int64`, `reapFailures atomic.Int64`, `func (s *Server) killStats() (inflight, failures int64)`, and `func (s *Server) trackedKill(sess *Session, grace int)` wrapping the kill with timing.

- [ ] **Step 1: Failing test**

```go
// internal/daemon/kill_accounting_test.go
package daemon

import "testing"

func TestKillStats_StartZero(t *testing.T) {
	s := &Server{}
	inflight, failures := s.killStats()
	if inflight != 0 || failures != 0 {
		t.Fatalf("got %d,%d want 0,0", inflight, failures)
	}
}
```

- [ ] **Step 2: Run — FAIL**

- [ ] **Step 3: Implement** — add fields + `killStats()`. Add `trackedKill`:

```go
// reapTimeout is how long a Kill may run before we count it a reap failure
// (a child stuck in uninterruptible kernel exit — e.g. a PTY whose master
// the daemon already closed but the process won't die). The kill goroutine
// keeps running; the counter surfaces the leak via health.
const reapTimeout = 30 * time.Second

func (s *Server) trackedKill(sess *Session, grace int) {
	s.inflightKills.Add(1)
	done := make(chan error, 1)
	go func() { done <- sess.Kill(grace) }()
	go func() {
		defer s.inflightKills.Add(-1)
		select {
		case err := <-done:
			if err != nil {
				log.Printf("openkanbankd: kill session %s: %v", sess.ID(), err)
			}
		case <-time.After(reapTimeout):
			s.reapFailures.Add(1)
			log.Printf("WARN: openkanbankd: session %s did not reap within %s (possible kernel-stuck child); will keep trying", sess.ID(), reapTimeout)
			<-done // still account for eventual completion
			s.reapFailures.Add(-1)
		}
	}()
}

func (s *Server) killStats() (int64, int64) {
	return s.inflightKills.Load(), s.reapFailures.Load()
}
```

Replace the `go func(sess *Session){ sess.Kill(...) }(m)` in `handleTicketDone` (1337) with `s.trackedKill(m, shutdownGraceSeconds)`. Leave `handleKill`'s synchronous `sess.Kill` as-is (it's already RPC-scoped) OR route it through `trackedKill` if you want its accounting too — recommended for consistency; if so, return success immediately after starting the tracked kill (preserving current idempotent semantics).

- [ ] **Step 4: Run + commit**

Run: `unset GOROOT && go test ./internal/daemon/ -run TestKillStats -v && go build ./...`
```bash
git add internal/daemon/server.go internal/daemon/kill_accounting_test.go
git commit -m "feat(daemon): account for in-flight kills"
```

### Task D2: Health RPC

**Files:**
- Modify: `internal/daemon/protocol.go` (wire types + msg constants)
- Modify: `internal/daemon/server.go` (`handleHealth` + dispatch case)
- Test: `internal/daemon/health_test.go`

**Interfaces:**
- Produces: `MsgHealthReq = "health.req"`, `MsgHealthResp = "health.resp"`; `type HealthReq struct{}`; `type HealthResp struct { Goroutines int; Sessions int; InflightHandlers int64; InflightKills int64; ReapFailures int64; DispatchSeq uint64; PID int }`; `func (s *Server) handleHealth(c *clientConn, req HealthReq) HealthResp`.

- [ ] **Step 1: Failing test**

```go
// internal/daemon/health_test.go
package daemon

import "testing"

func TestHandleHealth_ReportsCounts(t *testing.T) {
	s := &Server{reg: newSessionRegistry()}
	s.reg.store("a", &Session{id: "a"})
	resp := s.handleHealth(&clientConn{id: 1}, HealthReq{})
	if resp.Sessions != 1 {
		t.Fatalf("Sessions=%d want 1", resp.Sessions)
	}
	if resp.Goroutines <= 0 || resp.PID <= 0 {
		t.Fatalf("Goroutines=%d PID=%d want positive", resp.Goroutines, resp.PID)
	}
}
```

- [ ] **Step 2: Run — FAIL**

- [ ] **Step 3: Implement.** Add to protocol.go (near the other Msg constants + req/resp structs):
```go
const MsgHealthReq = "health.req"
const MsgHealthResp = "health.resp"

type HealthReq struct{}

type HealthResp struct {
	Goroutines       int    `json:"goroutines"`
	Sessions         int    `json:"sessions"`
	InflightHandlers int64  `json:"inflight_handlers"`
	InflightKills    int64  `json:"inflight_kills"`
	ReapFailures     int64  `json:"reap_failures"`
	DispatchSeq      uint64 `json:"dispatch_seq"`
	PID              int    `json:"pid"`
}
```
Add the handler in server.go:
```go
func (s *Server) handleHealth(c *clientConn, req HealthReq) HealthResp {
	seq, inflight := s.dispatchStats()
	kills, reapFail := s.killStats()
	return HealthResp{
		Goroutines:       runtime.NumGoroutine(),
		Sessions:         s.reg.len(),
		InflightHandlers: inflight,
		InflightKills:    kills,
		ReapFailures:     reapFail,
		DispatchSeq:      seq,
		PID:              os.Getpid(),
	}
}
```
Add the dispatch case (wrapped by the deadline guard from C2):
```go
	case MsgHealthReq:
		var req HealthReq
		_ = json.Unmarshal(raw, &req)
		var resp HealthResp
		if s.runHandlerWithDeadline("health", func() { resp = s.handleHealth(c, req) }) {
			s.writeResp(c, MsgHealthResp, resp)
		} else {
			s.writeError(c, "daemon_unresponsive", "health handler timed out")
		}
```

- [ ] **Step 4: Run + commit**

Run: `unset GOROOT && go test ./internal/daemon/ -run TestHandleHealth -v && go build ./...`
```bash
git add internal/daemon/protocol.go internal/daemon/server.go internal/daemon/health_test.go
git commit -m "feat(daemon): add health RPC"
```

### Task D3: `openkanban daemon health` CLI

**Files:**
- Modify: `cmd/daemon.go` (new subcommand mirroring `daemonListCmd`)

- [ ] **Step 1:** Add `daemonHealthCmd` modeled exactly on `daemonListCmd` (cmd/daemon.go:89): dial → Hello → `exchange(ctx, conn, r, daemon.MsgHealthReq, daemon.HealthReq{})` → unmarshal `daemon.HealthResp` → print fields. Register it in `init()` alongside the other daemon subcommands.

```go
var daemonHealthCmd = &cobra.Command{
	Use:           "health",
	Short:         "Show the running daemon's health counters",
	SilenceUsage:  true,
	SilenceErrors: false,
	RunE: func(cmd *cobra.Command, args []string) (err error) {
		defer func() { err = mapDaemonErr(err) }()
		ctx, cancel := context.WithTimeout(cmd.Context(), rpcTimeout)
		defer cancel()
		conn, err := dialDaemon(ctx)
		if err != nil {
			return err
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		if _, err := exchange(ctx, conn, r, daemon.MsgHelloReq, daemon.HelloReq{
			ProtocolVersion: daemon.ProtocolVersion, BinaryVersion: Version, ClientName: daemon.ClientNameCLI,
		}); err != nil {
			return fmt.Errorf("hello: %w", err)
		}
		raw, err := exchange(ctx, conn, r, daemon.MsgHealthReq, daemon.HealthReq{})
		if err != nil {
			return fmt.Errorf("health: %w", err)
		}
		var h daemon.HealthResp
		if err := json.Unmarshal(raw, &h); err != nil {
			return fmt.Errorf("decode HealthResp: %w", err)
		}
		fmt.Printf("pid=%d goroutines=%d sessions=%d inflight_handlers=%d inflight_kills=%d reap_failures=%d dispatch_seq=%d\n",
			h.PID, h.Goroutines, h.Sessions, h.InflightHandlers, h.InflightKills, h.ReapFailures, h.DispatchSeq)
		return nil
	},
}
```

- [ ] **Step 2: Build + commit**

Run: `unset GOROOT && go build ./... && ./scripts/install.sh && openkanban daemon health` (against a running daemon)
Expected: prints the counters line.
```bash
git add cmd/daemon.go
git commit -m "feat(cli): openkanban daemon health"
```

---

## PHASE E — Client force-restart of an up-but-wedged daemon (kills G6)

Today the client only autostarts a *down* daemon; an *up-but-wedged* one yields "run openkanban daemon restart" (futile via TUI) or a manual `kill -9`. E lets the client do it.

### Task E1: `ForceRestartDaemon`

**Files:**
- Create: `internal/daemonclient/recover.go`
- Test: `internal/daemonclient/recover_test.go`

**Interfaces:**
- Produces: `func ForceRestartDaemon(ctx context.Context) (*Client, error)` — reads the daemon pidfile, `kill -9`s it, waits for the socket to disappear, then `DialOrStart` (autostarts a fresh daemon) and returns a new Client. Returns an error if the pidfile is missing/dead (nothing to restart — caller falls back to plain `New`).
- Consumes: `daemon.PidPath()`, `daemon.SocketPath()`, `DialOrStart`, `New`.

- [ ] **Step 1: Failing test** (pure parts — pid parse + socket-gone wait, no real kill)

```go
// internal/daemonclient/recover_test.go
package daemonclient

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadPidFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "daemon.pid")
	if err := os.WriteFile(p, []byte("12345\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pid, err := readPidFile(p)
	if err != nil || pid != 12345 {
		t.Fatalf("readPidFile=%d,%v want 12345,nil", pid, err)
	}
	if _, err := readPidFile(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("expected error for missing pidfile")
	}
}
```

- [ ] **Step 2: Run — FAIL**

- [ ] **Step 3: Implement recover.go**

```go
// internal/daemonclient/recover.go
package daemonclient

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/techdufus/openkanban/internal/daemon"
)

func readPidFile(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("daemonclient: bad pidfile %q", path)
	}
	return pid, nil
}

// ForceRestartDaemon kills a wedged daemon (SIGKILL via its pidfile), waits
// for its socket to disappear, then autostarts + dials a fresh one. Use only
// after a liveness probe (PreflightListSessions) returns
// daemon.ErrDaemonUnresponsive: the daemon is up (socket dials) but not
// answering RPCs, so a normal reconnect won't help. Returns the error if
// there's no live daemon to kill (caller should fall back to New).
func ForceRestartDaemon(ctx context.Context) (*Client, error) {
	pidPath, err := daemon.PidPath()
	if err != nil {
		return nil, err
	}
	pid, err := readPidFile(pidPath)
	if err != nil {
		return nil, err
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		// ESRCH = already gone; fine, proceed to restart.
		if err != syscall.ESRCH {
			return nil, fmt.Errorf("daemonclient: kill wedged daemon %d: %w", pid, err)
		}
	}
	sock, err := daemon.SocketPath()
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, statErr := os.Stat(sock); os.IsNotExist(statErr) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	// Stale socket file may linger after SIGKILL (no clean unlink). Best-effort
	// remove so DialOrStart doesn't dial a dead socket.
	_ = os.Remove(sock)
	return New(ctx)
}
```

- [ ] **Step 4: Run + commit**

Run: `unset GOROOT && go test ./internal/daemonclient/ -run TestReadPidFile -v && go build ./...`
```bash
git add internal/daemonclient/recover.go internal/daemonclient/recover_test.go
git commit -m "feat(client): force-restart wedged daemon"
```

### Task E2: Use force-restart on startup-preflight wedge

**Files:**
- Modify: `internal/app/app.go` (~line 119, the `perr != nil` branch after `PreflightListSessions`)

**Interfaces:**
- Consumes: `daemonclient.ForceRestartDaemon`, `daemon.ErrDaemonUnresponsive`, `ui.PreflightListSessions`.

- [ ] **Step 1:** In app.go, when `PreflightListSessions` returns an error that `errors.Is(perr, daemon.ErrDaemonUnresponsive)` AND autostart is enabled, attempt one force-restart before giving up:

```go
	ownedByDaemon, perr := ui.PreflightListSessions(daemonClient)
	if perr != nil {
		if autostartDaemon && errors.Is(perr, daemon.ErrDaemonUnresponsive) {
			fmt.Fprintln(os.Stderr, "openkanban: daemon is wedged — force-restarting it...")
			_ = daemonClient.Close()
			rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Second)
			fresh, rerr := daemonclient.ForceRestartDaemon(rctx)
			rcancel()
			if rerr == nil {
				daemonClient = fresh
				ownedByDaemon, perr = ui.PreflightListSessions(daemonClient)
			}
		}
		if perr != nil {
			fmt.Fprintln(os.Stderr, daemon.UnresponsiveHint())
			_ = daemonClient.Close()
			return errors.New("openkanbankd unresponsive")
		}
	}
```

Note: force-restart kills in-flight sessions. That's acceptable here — the daemon is already wedged and those sessions are unreachable. Gate behind `autostartDaemon` so externally-managed (launchd `--no-launch-daemon`) setups don't get their daemon killed from under them (launchd will respawn, but the user opted out of client-managed lifecycle — surface the hint instead).

- [ ] **Step 2: Build + commit**

Run: `unset GOROOT && go build ./...`
```bash
git add internal/app/app.go
git commit -m "feat(app): auto-recover wedged daemon at startup"
```

---

## PHASE F — Regression test that locks the class (enforces G4)

Proves the property the whole plan buys: **one wedged pane must not freeze the daemon.** This is the executable enforcement of the lock-discipline invariant.

### Task F1: Wedge-a-pane / assert-responsive end-to-end test

**Files:**
- Create: `internal/daemon/wedge_regression_test.go`

**Interfaces:**
- Consumes: the test helpers used by existing daemon tests (`server_drain_test.go`, `server_subscribe_test.go` show the in-process server + dialed client harness). Reuse that harness; do not invent a new one.

- [ ] **Step 1: Write the test.** Start an in-process `Server`, spawn a session whose pane wedges its writer (a child that never drains stdin), force `WriteInput` backpressure, then assert a `List` and a `Health` RPC each return within a short deadline.

```go
// internal/daemon/wedge_regression_test.go
package daemon

import (
	"context"
	"testing"
	"time"
)

// TestWedgedPane_DoesNotFreezeList is the regression guard for the
// 2026-06-17 global-freeze incident: a single session whose PTY writer is
// blocked must NOT prevent List/Health from answering. Pre-COW-registry this
// hung forever (one stuck RLock-holder + a queued writer starved all RPCs).
func TestWedgedPane_DoesNotFreezeList(t *testing.T) {
	srv, client, cleanup := newTestServerAndClient(t) // reuse existing harness
	defer cleanup()

	// Spawn a session whose pane blocks all input writes (child not draining).
	// Use the existing test seam that constructs a Session over a Pane backed
	// by a non-draining pty/pipe; see server_drain_test.go for the pattern.
	sess := newWedgedTestSession(t)
	srv.reg.store(sess.ID(), sess)

	// Drive the pane into input backpressure so a writer would block (in the
	// old code, under p.mu; here it must be isolated).
	wedgePaneInput(t, sess)

	// List must answer within a tight deadline despite the wedged session.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := client.List(ctx)
	if err != nil {
		t.Fatalf("List did not answer with a wedged pane present: %v", err)
	}
	if len(resp.Sessions) == 0 {
		t.Fatal("List returned no sessions; expected the wedged one")
	}
}
```

If `newTestServerAndClient`, `newWedgedTestSession`, or `wedgePaneInput` don't already exist, add minimal versions next to this test, modeled on the existing `server_drain_test.go` / `server_subscribe_test.go` harness (do not duplicate — extract a shared helper if the existing tests already build a server+client).

- [ ] **Step 2: Run — confirm it PASSES on the post-Phase-A code**

Run: `unset GOROOT && go test ./internal/daemon/ -run TestWedgedPane_DoesNotFreezeList -race -v`
Expected: PASS (the COW registry makes List lock-free; the wedged session blocks only its own writer goroutine).

- [ ] **Step 3: Prove it's a real guard (red-before-green via revert).** Temporarily stash Phase A (or, on a scratch branch, restore the `sessionsMu` version of `handleList` that calls `sess.Info()` under `RLock`) and confirm THIS test hangs/times out. Then restore. Document the result in the commit body. This satisfies the "could this test pass even if the fix were absent?" gate.

- [ ] **Step 4: Lock-discipline unit test** — assert `Pane.Size`/`Running`/`PID` never block on `p.mu`:

```go
// internal/terminal/pane_lockfree_test.go
package terminal

import (
	"testing"
	"time"
)

func TestSizeIsLockFreeWhileMuHeld(t *testing.T) {
	p := newTestPane(t) // existing test constructor
	p.mu.Lock()
	defer p.mu.Unlock()
	done := make(chan struct{})
	go func() { _, _ = p.Size(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Pane.Size blocked while p.mu was held — Info path is not lock-free")
	}
}
```

(Use the package's existing test pane constructor; if none, build a minimal `Pane` with the atomic size mirror populated.)

- [ ] **Step 5: Run + commit**

Run: `unset GOROOT && go test ./internal/daemon/ ./internal/terminal/ -run 'Wedged|LockFree' -race -v`
```bash
git add internal/daemon/wedge_regression_test.go internal/terminal/pane_lockfree_test.go
git commit -m "test(daemon): guard against single-pane wedge"
```

---

## Final integration & validation

- [ ] **Full suite:** `unset GOROOT && go test ./... -race` — all green (except the known-flaky `TestServerLifecycle_SpawnEcho`, unchanged from main).
- [ ] **Install + smoke:** `./scripts/install.sh`, then `openkanban daemon restart`, `openkanban daemon health` (counters print), spawn a session, confirm List/attach work.
- [ ] **Manual wedge drill (optional, high-value):** reproduce the incident — paste a large blob into a session whose agent isn't reading stdin — and confirm (a) other RPCs stay responsive (Phase A), (b) `daemon health` shows rising `inflight_handlers`/`reap_failures` if applicable (Phase D), and (c) if you force a true wedge, the watchdog dumps + restarts within ~90s and launchd respawns (Phase B).
- [ ] **Docs:** update `internal/daemon/CLAUDE.md` to document the registry (replaces `sessionsMu`), the wedge watchdog, the conn-sem cap, the health RPC, and the client force-restart path. Update `internal/terminal/CLAUDE.md` lock-discipline note to reference the new regression test as the enforcement.
- [ ] **Memory:** add a one-line bullet to `[[project_openkanban_personal_fork]]` summarizing the resilience hardening (registry + watchdog + health + force-restart).

## Self-review notes (gap → task coverage)

- G1 (no self-recovery / stale persistent) → B2 (watchdog `os.Exit` → respawn) + B2 step 6 (honest log) + E (client force-restart).
- G2 (unbounded accept / no timeout) → C1 (conn-sem) + C2 (handler deadline).
- G3 (untracked kills / un-reapable) → D1 (kill accounting) + D2/D3 (health RPC + CLI).
- G4 (unenforced invariant) → A (removes the global-lock vector) + F (regression + lock-free test).
- G5 (global RWMutex starvation) → A (COW registry: lock-free reads).
- G6 (client recovery blindspot) → E1/E2 (force-restart) + B2 step 6.
