package ui

import (
	"context"
	"time"

	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/daemonclient"
)

// Compile-time enforcement that the noop base implements every method
// on daemonAPI. If a future PR adds a method to daemonAPI and forgets
// to add it here, this assertion fails at test-compile time — the
// failure surfaces in one place instead of fanning out across every
// fake that embeds the noop. This is the load-bearing guarantee the
// noop pattern provides.
var _ daemonAPI = (*daemonAPINoop)(nil)

// Compile-time enforcement that *daemonclient.Client (the production
// implementation behind m.daemon) satisfies daemonAPI. Catches the
// inverse drift — if a method is added to daemonAPI but not the
// concrete client, this fails at test-compile time before any
// production code can break against an unimplemented method.
var _ daemonAPI = (*daemonclient.Client)(nil)

// daemonAPINoop is a zero-value implementation of daemonAPI suitable
// for embedding in test fakes. Each method returns the zero value of
// its response type and a nil error.
//
// Embed this in a fake, then override only the methods the test
// exercises. The pattern collapses each fake from "implement all N
// methods inline" to "embed noop + override the 1-3 methods this test
// cares about" — and removes the cross-PR breakage where adding a
// method to daemonAPI forced an edit in every fake.
//
//	type myStubAPI struct {
//	    daemonAPINoop
//	    callCount atomic.Int32
//	}
//
//	// Only override the one method this test cares about.
//	func (s *myStubAPI) Owns(_ context.Context, _ string) (daemon.OwnsResp, error) {
//	    s.callCount.Add(1)
//	    return daemon.OwnsResp{Owned: true}, nil
//	}
//
// NEW METHODS ADDED TO daemonAPI MUST BE ADDED HERE TOO. This is the
// one place; the whole point of the type is that adding a method
// does not propagate edits to every fake.
type daemonAPINoop struct{}

// --- daemonExitGuard ---

func (daemonAPINoop) PrepareExit(_ context.Context) (daemon.PrepareExitResp, error) {
	return daemon.PrepareExitResp{}, nil
}

func (daemonAPINoop) CancelExit(_ context.Context) error { return nil }

func (daemonAPINoop) Kill(_ context.Context, _ string, _ time.Duration) error { return nil }

func (daemonAPINoop) ClientID() uint16 { return 0 }

// --- daemonSessionLifecycle ---

func (daemonAPINoop) Spawn(_ context.Context, _ daemon.SpawnReq) (daemon.SpawnResp, error) {
	return daemon.SpawnResp{}, nil
}

func (daemonAPINoop) TicketDone(_ context.Context, _ string) (daemon.TicketDoneResp, error) {
	return daemon.TicketDoneResp{}, nil
}

// --- daemonSessionQuery ---

func (daemonAPINoop) Owns(_ context.Context, _ string) (daemon.OwnsResp, error) {
	return daemon.OwnsResp{}, nil
}

func (daemonAPINoop) List(_ context.Context) (daemon.ListResp, error) {
	return daemon.ListResp{}, nil
}
