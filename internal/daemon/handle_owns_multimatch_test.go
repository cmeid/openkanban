package daemon

import (
	"testing"
)

// TestHandleOwns_NoMatch returns the empty-Owns response.
func TestHandleOwns_NoMatch(t *testing.T) {
	srv, _ := startServer(t)
	resp := srv.handleOwns(nil, OwnsReq{SessionUUID: "no-such-uuid"})
	if resp.Owned {
		t.Errorf("Owned: got true, want false")
	}
	if resp.Conflict {
		t.Errorf("Conflict: got true, want false (no matches)")
	}
}

// TestHandleOwns_SingleMatch populates the standard fields plus the
// new OwnedByTicketID field so callers can distinguish self vs foreign.
func TestHandleOwns_SingleMatch(t *testing.T) {
	srv, _ := startServer(t)
	const ticketID = "TICK-single"
	const uuid = "single-uuid"
	sess, err := NewSession(SpawnReq{
		TicketID:         ticketID,
		SessionName:      "single",
		AgentSessionUUID: uuid,
		Command:          "/bin/cat",
		Cols:             80,
		Rows:             24,
		Scrollback:       1000,
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	srv.sessionsMu.Lock()
	srv.sessions[sess.ID()] = sess
	srv.sessionsMu.Unlock()
	srv.watchSessionExit(sess)

	resp := srv.handleOwns(nil, OwnsReq{SessionUUID: uuid})
	if !resp.Owned {
		t.Fatalf("Owned: got false, want true")
	}
	if resp.SessionID != sess.ID() {
		t.Errorf("SessionID: got %q, want %q", resp.SessionID, sess.ID())
	}
	if resp.OwnedByTicketID != ticketID {
		t.Errorf("OwnedByTicketID: got %q, want %q", resp.OwnedByTicketID, ticketID)
	}
	if resp.Conflict {
		t.Errorf("Conflict: got true, want false (single match)")
	}
	if len(resp.ConflictSessionIDs) != 0 {
		t.Errorf("ConflictSessionIDs: got %v, want empty", resp.ConflictSessionIDs)
	}
}

// TestHandleOwns_MultiMatch_ReturnsConflict is the load-bearing
// red-before-green test for Task 5. Against main, the first-match
// early-return at server.go:1196 returns Owned=true with Conflict=false
// even when two real Sessions share an AgentSessionUUID — silently
// routing to one and hiding the duplicate. With the fix, the daemon
// surfaces Conflict=true and lists every matching session.
//
// Pre-fix proof: on main, this assertion `Conflict: got false, want
// true` fires.
func TestHandleOwns_MultiMatch_ReturnsConflict(t *testing.T) {
	srv, _ := startServer(t)
	const sharedUUID = "shared-uuid-duplicate"

	mkSess := func(ticketID, sessionName string) *Session {
		sess, err := NewSession(SpawnReq{
			TicketID:         ticketID,
			SessionName:      sessionName,
			AgentSessionUUID: sharedUUID,
			Command:          "/bin/cat",
			Cols:             80,
			Rows:             24,
			Scrollback:       1000,
		})
		if err != nil {
			t.Fatalf("NewSession(%s): %v", sessionName, err)
		}
		return sess
	}
	sess1 := mkSess("TICK-A", "dup-a")
	sess2 := mkSess("TICK-B", "dup-b")
	srv.sessionsMu.Lock()
	srv.sessions[sess1.ID()] = sess1
	srv.sessions[sess2.ID()] = sess2
	srv.sessionsMu.Unlock()
	srv.watchSessionExit(sess1)
	srv.watchSessionExit(sess2)

	resp := srv.handleOwns(nil, OwnsReq{SessionUUID: sharedUUID})

	if !resp.Owned {
		t.Errorf("Owned: got false, want true (matches exist)")
	}
	if !resp.Conflict {
		t.Errorf("Conflict: got false, want true (two sessions share UUID)")
	}
	if len(resp.ConflictSessionIDs) != 2 {
		t.Errorf("ConflictSessionIDs: got %d entries, want 2: %v",
			len(resp.ConflictSessionIDs), resp.ConflictSessionIDs)
	}
	// Verify both session IDs are present (set membership; order undefined).
	want := map[string]bool{sess1.ID(): false, sess2.ID(): false}
	for _, id := range resp.ConflictSessionIDs {
		if _, ok := want[id]; ok {
			want[id] = true
		}
	}
	for id, found := range want {
		if !found {
			t.Errorf("ConflictSessionIDs missing %q (got %v)", id, resp.ConflictSessionIDs)
		}
	}
}

// TestHandleOwns_EmptyUUID is the existing ill-formed-query path.
func TestHandleOwns_EmptyUUID(t *testing.T) {
	srv, _ := startServer(t)
	resp := srv.handleOwns(nil, OwnsReq{SessionUUID: ""})
	if resp.Owned {
		t.Errorf("Owned: got true, want false (empty UUID)")
	}
}
