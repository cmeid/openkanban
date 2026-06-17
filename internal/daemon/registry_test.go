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
