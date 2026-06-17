package ui

import (
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/project"
)

// TestReadNextDaemonEventCoalescesBurst pins the O(N-agents) churn fix:
// a burst of events already queued on the subscribe channel must be
// drained into ONE daemonSessionEventsMsg (one Update/render cycle),
// not delivered one per read. Order must be preserved.
func TestReadNextDaemonEventCoalescesBurst(t *testing.T) {
	ch := make(chan daemon.SessionEvent, 16)
	for i, s := range []string{"a", "b", "c", "d"} {
		ch <- daemon.SessionEvent{Event: "activity", SessionID: s, TicketID: s, LastActivityAt: time.Unix(int64(i+1), 0)}
	}

	msg := readNextDaemonEvent(ch)()
	batch, ok := msg.(daemonSessionEventsMsg)
	if !ok {
		t.Fatalf("expected daemonSessionEventsMsg, got %T", msg)
	}
	if len(batch.Events) != 4 {
		t.Fatalf("burst of 4 should coalesce into one msg of 4; got %d", len(batch.Events))
	}
	for i, want := range []string{"a", "b", "c", "d"} {
		if batch.Events[i].SessionID != want {
			t.Errorf("event %d: order not preserved, got %q want %q", i, batch.Events[i].SessionID, want)
		}
	}
}

// TestReadNextDaemonEventSingle: a lone event still delivers as a batch
// of one (the common steady-state case).
func TestReadNextDaemonEventSingle(t *testing.T) {
	ch := make(chan daemon.SessionEvent, 4)
	ch <- daemon.SessionEvent{Event: "activity", SessionID: "x", TicketID: "x"}

	batch, ok := readNextDaemonEvent(ch)().(daemonSessionEventsMsg)
	if !ok || len(batch.Events) != 1 {
		t.Fatalf("single event should be a batch of 1; got %#v", batch)
	}
}

// TestHandleDaemonSessionEventsAppliesWholeBatch proves the batch
// handler applies every event in one call, in order: two activity
// stamps land for two tickets, and an in-batch viewing→unviewing pair
// nets to zero (order-sensitive semantics preserved).
func TestHandleDaemonSessionEventsAppliesWholeBatch(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	gs := project.NewGlobalTicketStore(nil)
	gs.AddProject(proj)
	for _, id := range []board.TicketID{"t1", "t2"} {
		if err := gs.Add(&board.Ticket{ID: id, Title: string(id), ProjectID: "test", Status: board.StatusInProgress}); err != nil {
			t.Fatalf("add %s: %v", id, err)
		}
	}
	m := &Model{
		globalStore:     gs,
		daemonOwned:     map[board.TicketID]struct{}{"t1": {}, "t2": {}},
		daemonViewing:   map[board.TicketID]int{},
		lastPTYActivity: map[board.TicketID]time.Time{},
	}

	ts1 := time.Unix(100, 0)
	ts2 := time.Unix(200, 0)
	m.handleDaemonSessionEvents(daemonSessionEventsMsg{Events: []daemon.SessionEvent{
		{Event: "activity", TicketID: "t1", LastActivityAt: ts1},
		{Event: "activity", TicketID: "t2", LastActivityAt: ts2},
		{Event: "viewing", TicketID: "t1"},
		{Event: "unviewing", TicketID: "t1"},
	}})

	if got := m.lastPTYActivity["t1"]; !got.Equal(ts1) {
		t.Errorf("t1 activity = %v, want %v", got, ts1)
	}
	if got := m.lastPTYActivity["t2"]; !got.Equal(ts2) {
		t.Errorf("t2 activity = %v, want %v", got, ts2)
	}
	if _, present := m.daemonViewing["t1"]; present {
		t.Errorf("viewing→unviewing in one batch should net to 0 and delete the key")
	}
}
