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
