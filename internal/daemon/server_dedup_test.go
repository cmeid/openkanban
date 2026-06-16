package daemon

import (
	"bufio"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"
)

// spawnAndDecode performs a SpawnReq RPC over conn and returns the
// decoded SpawnResp. Failures abort the test. Helper local to the
// dedup tests so the existing spawnHelper (which only returns the
// session id) doesn't need its signature widened — dedup tests care
// about both the SessionID and the PID to verify idempotent reuse.
//
// Read deadline is configurable so the concurrent-spawn test can use
// a longer window: under -race with 8 simultaneous /bin/cat forks the
// per-spawn round-trip can exceed 3s legitimately. Pass 0 for the
// default (5s) — generous enough for serial calls and forgiving
// enough for concurrent ones.
func spawnAndDecode(t *testing.T, conn net.Conn, r *bufio.Reader, req SpawnReq, readDeadline time.Duration) SpawnResp {
	t.Helper()
	if readDeadline <= 0 {
		readDeadline = 5 * time.Second
	}
	writeReq(t, conn, MsgSpawnReq, req)
	conn.SetReadDeadline(time.Now().Add(readDeadline))
	defer conn.SetReadDeadline(time.Time{})
	name, raw := readResp(t, r)
	if name != MsgSpawnResp {
		t.Fatalf("spawn: got %q want %q (payload=%s)", name, MsgSpawnResp, string(raw))
	}
	var resp SpawnResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode SpawnResp: %v", err)
	}
	if resp.SessionID == "" {
		t.Fatalf("SpawnResp.SessionID empty")
	}
	return resp
}

// dedupSpawnReq returns a SpawnReq using a long-lived /bin/cat child so
// the session stays alive long enough for the test to inspect the
// daemon's session map. cat blocks reading stdin and won't exit until
// the test kills it.
func dedupSpawnReq(ticketID string) SpawnReq {
	return SpawnReq{
		TicketID:    ticketID,
		SessionName: "dedup-test-" + ticketID,
		Command:     "/bin/cat",
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	}
}

// TestHandleSpawn_DuplicateTicketIDReturnsExisting verifies the
// idempotency contract: a second Spawn with the same TicketID returns
// the existing SessionID and does NOT add a second entry to the
// session registry.
func TestHandleSpawn_DuplicateTicketIDReturnsExisting(t *testing.T) {
	srv, errCh := startServer(t)

	a := dialTestClient(t, srv.SocketPath())
	ra := bufio.NewReader(a)
	helloAndUnpack(t, a, ra)

	first := spawnAndDecode(t, a, ra, dedupSpawnReq("DEDUP-1"), 0)

	// Sanity: registry contains exactly one entry.
	srv.sessionsMu.RLock()
	got := len(srv.sessions)
	srv.sessionsMu.RUnlock()
	if got != 1 {
		t.Fatalf("after first spawn: sessions count=%d want 1", got)
	}

	// Second Spawn with the same TicketID — must reuse.
	second := spawnAndDecode(t, a, ra, dedupSpawnReq("DEDUP-1"), 0)

	if second.SessionID != first.SessionID {
		t.Errorf("dedup miss: second SessionID=%q first SessionID=%q (want equal)",
			second.SessionID, first.SessionID)
	}
	if second.PID != first.PID {
		t.Errorf("dedup miss: second PID=%d first PID=%d (want equal)",
			second.PID, first.PID)
	}

	srv.sessionsMu.RLock()
	got = len(srv.sessions)
	srv.sessionsMu.RUnlock()
	if got != 1 {
		t.Errorf("after second spawn: sessions count=%d want 1 (dedup should have prevented insert)", got)
	}

	// Clean up: kill the live session so the daemon can shut down
	// without warning about the bypassed exit-guard.
	writeReq(t, a, MsgKillReq, KillReq{SessionID: first.SessionID, GraceSeconds: 0})
	a.SetReadDeadline(time.Now().Add(3 * time.Second))
	readResp(t, ra)
	a.SetReadDeadline(time.Time{})

	a.Close()
	waitServerDone(t, errCh, 5*time.Second)
}

// TestHandleSpawn_ConcurrentSpawnsDedup fires two concurrent Spawn
// calls for the same TicketID and verifies the dedup invariant: both
// responses carry the same SessionID, only one session exists in the
// registry afterwards, and the race-loser's `sess.Kill(0)` ran (so
// no PTY leaked). N=2 is the minimum that exercises the
// construct-outside-lock race window (the WLock re-check branch);
// larger N adds cumulative race-loser teardown latency without
// gaining additional coverage.
func TestHandleSpawn_ConcurrentSpawnsDedup(t *testing.T) {
	srv, errCh := startServer(t)

	const n = 2
	conns := make([]net.Conn, n)
	readers := make([]*bufio.Reader, n)
	for i := 0; i < n; i++ {
		conns[i] = dialTestClient(t, srv.SocketPath())
		readers[i] = bufio.NewReader(conns[i])
		helloAndUnpack(t, conns[i], readers[i])
	}

	// Starting gate so the goroutines fire as close to simultaneously
	// as possible. The race window is "both callers observe an empty
	// slot under RLock and both reach NewSession" — likeliest when the
	// pre-Lock period overlaps.
	var start sync.WaitGroup
	start.Add(1)
	resps := make([]SpawnResp, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start.Wait()
			// Per-call deadline: a race-loser's handleSpawn runs
			// NewSession + sess.Kill(0) before returning, and Kill
			// blocks on stopReadLoop's pane goroutine. Under -race
			// stopReadLoop has been observed to take 15-25s, so we
			// budget 30s — well above the observed ceiling without
			// masking a true hang. Tests that exercised tighter
			// deadlines flaked at ~1/30 even at N=2.
			resps[i] = spawnAndDecode(t, conns[i], readers[i], dedupSpawnReq("CONCURRENT-1"), 30*time.Second)
		}(i)
	}
	start.Done()
	wg.Wait()

	// Both responses must reference the same SessionID. spawnAndDecode
	// would have t.Fatalf'd on read failure, so reaching this point
	// means both SessionIDs are populated.
	winner := resps[0].SessionID
	for i, r := range resps {
		if r.SessionID != winner {
			t.Errorf("client %d SessionID=%q != winner=%q", i, r.SessionID, winner)
		}
	}

	// Exactly one session survives.
	srv.sessionsMu.RLock()
	gotCount := len(srv.sessions)
	_, winnerLives := srv.sessions[winner]
	srv.sessionsMu.RUnlock()
	if gotCount != 1 {
		t.Errorf("post-race sessions count=%d want 1", gotCount)
	}
	if !winnerLives {
		t.Errorf("winner SessionID=%q not in registry", winner)
	}

	// Tear down via the wire so the daemon can exit cleanly.
	writeReq(t, conns[0], MsgKillReq, KillReq{SessionID: winner, GraceSeconds: 0})
	conns[0].SetReadDeadline(time.Now().Add(5 * time.Second))
	readResp(t, readers[0])
	conns[0].SetReadDeadline(time.Time{})

	for _, c := range conns {
		c.Close()
	}
	waitServerDone(t, errCh, 30*time.Second)
}

// TestHandleSpawn_DifferentTicketIDsCoexist verifies the dedup is
// scoped per-TicketID: two Spawns with different TicketIDs both
// succeed and both sessions land in the registry.
func TestHandleSpawn_DifferentTicketIDsCoexist(t *testing.T) {
	srv, errCh := startServer(t)

	a := dialTestClient(t, srv.SocketPath())
	ra := bufio.NewReader(a)
	helloAndUnpack(t, a, ra)

	r1 := spawnAndDecode(t, a, ra, dedupSpawnReq("COEXIST-A"), 0)
	r2 := spawnAndDecode(t, a, ra, dedupSpawnReq("COEXIST-B"), 0)

	if r1.SessionID == r2.SessionID {
		t.Errorf("different TicketIDs collapsed to same SessionID=%q", r1.SessionID)
	}

	srv.sessionsMu.RLock()
	gotCount := len(srv.sessions)
	srv.sessionsMu.RUnlock()
	if gotCount != 2 {
		t.Errorf("sessions count=%d want 2", gotCount)
	}

	// Clean up both.
	for _, id := range []string{r1.SessionID, r2.SessionID} {
		writeReq(t, a, MsgKillReq, KillReq{SessionID: id, GraceSeconds: 0})
		a.SetReadDeadline(time.Now().Add(3 * time.Second))
		readResp(t, ra)
		a.SetReadDeadline(time.Time{})
	}

	a.Close()
	waitServerDone(t, errCh, 5*time.Second)
}

// TestHandleTicketDone_KillsAllMatchesForTicket verifies the
// defense-in-depth path in handleTicketDone: a daemon that already has
// two sessions sharing a TicketID (simulating duplicates inherited
// from a pre-dedup daemon) terminates both on the next TicketDone.
//
// handleSpawn now refuses to create such a duplicate, so we
// synthesize the bad state by calling NewSession directly (which
// bypasses the daemon's dedup check) twice with the same TicketID,
// then inserting both into srv.sessions under the lock. ticketID is
// stamped at NewSession time and never mutated afterwards, so this
// avoids racing the watchSessionExit goroutine's read of it.
func TestHandleTicketDone_KillsAllMatchesForTicket(t *testing.T) {
	srv, errCh := startServer(t)

	// Subscribe B first so it observes the "exited" events the
	// ticket-done flow emits.
	b := dialTestClient(t, srv.SocketPath())
	rb := bufio.NewReader(b)
	subscribeClient(t, b, rb)

	a := dialTestClient(t, srv.SocketPath())
	ra := bufio.NewReader(a)
	helloAndUnpack(t, a, ra)

	// Build two sessions sharing a TicketID using NewSession directly.
	// This is the *only* way to drive the "iterate all matches" branch
	// — handleSpawn would now dedup the second call. Both sessions
	// must have the watcher wired so cleanup runs naturally.
	const sharedTicket = "DEDUP-DONE-SHARED"
	mkSess := func(name string) *Session {
		sess, err := NewSession(SpawnReq{
			TicketID:    sharedTicket,
			SessionName: name,
			Command:     "/bin/cat",
			Cols:        80,
			Rows:        24,
			Scrollback:  1000,
		})
		if err != nil {
			t.Fatalf("NewSession(%s): %v", name, err)
		}
		return sess
	}
	sess1 := mkSess("dup-1")
	sess2 := mkSess("dup-2")

	srv.sessionsMu.Lock()
	srv.sessions[sess1.ID()] = sess1
	srv.sessions[sess2.ID()] = sess2
	srv.sessionsMu.Unlock()

	// Wire pane-exit observation for both, mirroring what handleSpawn
	// does. Without this the "exited" events won't fire and the test
	// can't verify the full kill path.
	srv.watchSessionExit(sess1)
	srv.watchSessionExit(sess2)

	// TicketDone for the shared TicketID should sweep both.
	writeReq(t, a, MsgTicketDoneReq, TicketDoneReq{TicketID: sharedTicket})
	a.SetReadDeadline(time.Now().Add(3 * time.Second))
	name, raw := readResp(t, ra)
	a.SetReadDeadline(time.Time{})
	if name != MsgTicketDoneResp {
		t.Fatalf("ticket_done: got %q want %q", name, MsgTicketDoneResp)
	}
	var resp TicketDoneResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode TicketDoneResp: %v", err)
	}
	if !resp.Killed {
		t.Errorf("TicketDoneResp.Killed=false; want true")
	}

	// The registry must drop both sessions. The matches are removed
	// synchronously in handleTicketDone before the kill goroutine
	// fires, so we can assert without polling.
	srv.sessionsMu.RLock()
	remaining := len(srv.sessions)
	srv.sessionsMu.RUnlock()
	if remaining != 0 {
		t.Errorf("after ticket-done: %d sessions remain; want 0 (both should have been swept)", remaining)
	}

	// Both sessions should publish "exited" events with
	// Expected=true. Collect two and check. The watcher only emits
	// "exited" — not "started" — when sessions are inserted directly
	// into srv.sessions (handleSpawn is what emits "started"), so the
	// next two SessionEvents B sees are the pair we want.
	exitedSeen := map[string]SessionEvent{}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(exitedSeen) < 2 {
		ev, ok := readNextSessionEvent(t, b, rb, 500*time.Millisecond)
		if !ok {
			continue
		}
		if ev.Event == "exited" {
			exitedSeen[ev.SessionID] = ev
		}
	}
	if len(exitedSeen) != 2 {
		t.Errorf("expected 2 'exited' events; got %d (%v)", len(exitedSeen), exitedSeen)
	}
	for sid, ev := range exitedSeen {
		if !ev.Expected {
			t.Errorf("exited event for session %s: Expected=false; want true", sid)
		}
		if ev.Reason != "ticket_done" {
			t.Errorf("exited event for session %s: Reason=%q; want %q", sid, ev.Reason, "ticket_done")
		}
	}

	a.Close()
	b.Close()
	waitServerDone(t, errCh, 5*time.Second)
}
