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
