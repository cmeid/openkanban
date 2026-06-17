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
