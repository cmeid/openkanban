package ui

import (
	"context"
	"time"

	"github.com/techdufus/openkanban/internal/daemon"
)

// daemonAPI is the full surface the TUI uses against openkanbankd. It's
// held as an interface (rather than the concrete *daemonclient.Client)
// so tests can swap in a fake without bringing up a real daemon. The
// real daemonclient.Client satisfies daemonAPI by virtue of its
// PrepareExit / CancelExit / Kill / ClientID / Owns / TicketDone /
// List / Spawn methods.
//
// Structurally, daemonAPI is the composition of three concern-grouped
// sub-interfaces:
//
//   - daemonExitGuard      — the PrepareExit / CancelExit / ClientID
//     handshake the TUI uses on quit, plus per-session Kill.
//   - daemonSessionLifecycle — Spawn / Kill / TicketDone, the calls that
//     create or wind-down sessions. (Kill lives in the exit-guard
//     interface because the exit modal needs it; nothing else in the
//     UI calls it directly.)
//   - daemonSessionQuery   — Owns / List, the read-only queries the spawn
//     gate and the periodic resync use.
//
// The composite groups the seam by concern so each piece is documented
// independently and test fakes can declare which concern they exercise.
// Embedding (rather than three separate fields on Model) keeps the
// call-site ergonomics: `m.daemon.TicketDone(...)` doesn't have to
// know which sub-interface owns TicketDone.
type daemonAPI interface {
	daemonExitGuard
	daemonSessionLifecycle
	daemonSessionQuery
}

// daemonExitGuard is the handshake the TUI uses on quit, plus the
// per-session Kill the exit-confirmation modal fires when the user
// terminates a live session before quitting.
type daemonExitGuard interface {
	// PrepareExit asks the daemon for an authoritative snapshot of live
	// sessions and the per-client exit-intent count. The caller uses
	// the response's OtherActiveClients field to decide whether a
	// silent quit is safe (a peer TUI is still attached and not also
	// exiting) or whether to surface the exit-confirmation modal.
	PrepareExit(ctx context.Context) (daemon.PrepareExitResp, error)
	// CancelExit clears the per-client exiting flag set by PrepareExit.
	// Fired fire-and-forget when the user dismisses the exit modal so
	// peer TUIs see this client as active again on their next
	// PrepareExit.
	CancelExit(ctx context.Context) error
	// Kill terminates a daemon-side PTY session. The grace duration is
	// the SIGTERM-to-SIGKILL window the daemon honours before forcing
	// the kill. Used by the exit-confirmation modal's per-session kill
	// action; also the chokepoint the daemon-resync paths would use if
	// they needed to tear down a session, though today they don't.
	Kill(ctx context.Context, sessionID string, grace time.Duration) error
	// ClientID returns the daemon-assigned ID for this client. Used
	// for diagnostic logging and for peer-vs-self comparisons in the
	// exit-guard decision tree.
	ClientID() uint16
}

// daemonSessionLifecycle covers the RPCs that create or wind down
// sessions on the daemon side.
type daemonSessionLifecycle interface {
	// Spawn forwards the spawn RPC. Routed through this seam so the
	// closure inside prepareSpawnWith can be exercised in tests
	// without a live daemon — letting an Owns fast-path test assert
	// no Spawn was issued.
	Spawn(ctx context.Context, req daemon.SpawnReq) (daemon.SpawnResp, error)
	// TicketDone informs the daemon that a ticket is wrapping up so it
	// can terminate the live PTY for that ticket and broadcast an
	// Expected=true SessionEvent. Used by the TUI's board-promotion
	// wrap-up to mirror the CLI's `openkanban ticket done` path.
	TicketDone(ctx context.Context, ticketID string) (daemon.TicketDoneResp, error)
}

// daemonSessionQuery groups the read-only queries the TUI makes against
// the daemon's session set.
type daemonSessionQuery interface {
	// Owns reports whether the daemon owns a live PTY for the given
	// session UUID. Used by spawnAgent to short-circuit the on-disk
	// JSONL dead-session check (and, symmetrically, by the spawn
	// fast-path in prepareSpawnWith to skip a fresh Spawn) when the
	// daemon already has a live PTY for the session UUID.
	Owns(ctx context.Context, sessionUUID string) (daemon.OwnsResp, error)
	// List returns the daemon's current set of sessions. Used by the
	// startup reconcile (with retry/backoff) and the periodic 30s
	// resync to keep m.panes / m.daemonOwned aligned with the daemon's
	// authoritative view.
	List(ctx context.Context) (daemon.ListResp, error)
}
