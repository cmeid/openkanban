// Package ticketsvc is the single funnel for any operation that
// writes ticket.AgentSessionID or gates an attach to an existing
// Claude / opencode session. Both TUI (internal/ui) and CLI (cmd/)
// must call through here — no direct AgentSessionID assignments.
//
// Two public operations:
//
//   - LinkSession claims a session UUID for a ticket after verifying
//     no OTHER ticket already claims it. Used at ticket-creation
//     (`openkanban ticket new --session <uuid>`) and post-spawn
//     back-fill (`pollAgentStatusesAsync` discovery via
//     FindClaudeSession). Storage tolerates duplicates by policy, but
//     this function REFUSES to create new ones; the existing 87728fa3 /
//     f1898c56 pair is the canonical pre-existing duplicate.
//
//   - GateAttach asks whether a session UUID can safely be attached
//     by the requesting ticket right now. Probes lsof (any local
//     process holding the JSONL?) and the daemon (an existing daemon
//     session that's NOT ours?). Refusal returns *ErrSessionInUse with
//     the holder identity so the caller can surface it.
//
// SessionProbe is a function type rather than an interface — minimal
// abstraction for fake injection in tests, harder to grow into a
// "framework" than an interface. See [[feedback_openkanban_no_premature_service_abstraction]].
//
// TicketStore is a narrow CONSUMER interface (3 methods) defined here
// because ticketsvc is the consumer. *project.GlobalTicketStore
// implements it via FindByAgentSessionID + existing Get / Save. Not a
// service-layer abstraction in the producer-side sense — the user
// explicitly rejected a TicketService interface; this is the inverse.
package ticketsvc

import (
	"fmt"

	"github.com/techdufus/openkanban/internal/agent"
	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/daemon"
)

// TicketStore is the narrow consumer surface LinkSession needs.
// Implemented in production by *project.GlobalTicketStore. Two methods
// only — the uniqueness scan and the save. The requesting ticket is
// passed to LinkSession as a *board.Ticket pointer rather than looked
// up by ID, because CLI creation calls LinkSession BEFORE the new
// ticket has been added to the store.
type TicketStore interface {
	FindByAgentSessionID(uuid string) []*board.Ticket
	Save(t *board.Ticket) error
}

// LinkOpts modifies LinkSession behavior on uniqueness conflict.
// BestEffort and Force are mutually exclusive — passing both is an
// error returned at the head of LinkSession.
type LinkOpts struct {
	// BestEffort: when uuid is already claimed by a different ticket,
	// return (false, nil) instead of *ErrSessionAlreadyLinked. Used by
	// post-spawn back-fill where a conflict means "another ticket
	// holds this UUID first; skip our write."
	BestEffort bool

	// Force: when uuid is already claimed, clear the conflicting
	// ticket(s)' AgentSessionID first, then claim. Used by
	// `openkanban ticket new --session <uuid> --force`.
	Force bool
}

// ErrSessionAlreadyLinked is returned by LinkSession when uuid is
// already on a different ticket and neither BestEffort nor Force is
// set. ConflictTicketIDs lists every ticket that currently claims uuid
// (usually one, but the storage layer tolerates duplicates and a
// migration / restore could produce more).
type ErrSessionAlreadyLinked struct {
	UUID              string
	ConflictTicketIDs []board.TicketID
}

func (e *ErrSessionAlreadyLinked) Error() string {
	if len(e.ConflictTicketIDs) == 1 {
		return fmt.Sprintf("session %s is already linked to ticket %s", e.UUID, e.ConflictTicketIDs[0])
	}
	return fmt.Sprintf("session %s is already linked to tickets %v", e.UUID, e.ConflictTicketIDs)
}

// ErrSessionInUse is returned by GateAttach when the session JSONL is
// held by a local process (HolderPID > 0) or by a daemon session that
// is NOT for the requesting ticket (DaemonSessionID set). Exactly one
// of HolderPID / DaemonSessionID is populated.
type ErrSessionInUse struct {
	UUID             string
	HolderPID        int
	HolderPath       string
	DaemonSessionID  string
}

func (e *ErrSessionInUse) Error() string {
	if e.HolderPID > 0 {
		return fmt.Sprintf("session %s is held by pid %d (path %s)", e.UUID, e.HolderPID, e.HolderPath)
	}
	if e.DaemonSessionID != "" {
		return fmt.Sprintf("session %s is held by daemon session %s (a different ticket)", e.UUID, e.DaemonSessionID)
	}
	return fmt.Sprintf("session %s is in use", e.UUID)
}

// LinkSession writes uuid to the given ticket's AgentSessionID after
// verifying no OTHER ticket in the store already claims it. See
// LinkOpts for the conflict-resolution policy.
//
// requesting is passed as a *board.Ticket pointer so the CLI creation
// path can call this BEFORE adding the new ticket to the store —
// uniqueness scans existing tickets and excludes requesting by ID.
//
// **The caller is responsible for persisting `requesting`.** LinkSession
// only mutates AgentSessionID in place. It DOES persist Force-cleared
// conflict tickets (because the caller can't know which were touched);
// the requesting ticket's persistence stays at the call site (the CLI
// already calls store.SaveTicket(ticket) after applySessionFlags; the
// back-fill path already calls Save after the in-memory assignment).
//
// Returns (written=true, nil) when the write happened in memory.
// Returns (false, nil) when:
//   - uuid is empty, store is nil, or requesting is nil (no-op)
//   - requesting already has this UUID (idempotent)
//   - conflict + BestEffort=true (silent skip)
// Returns (false, *ErrSessionAlreadyLinked) on conflict without Force/BestEffort.
//
// On Force, conflicting tickets' AgentSessionID is cleared (and SAVED)
// FIRST, then requesting claims. If any conflict save fails the
// function returns the error WITHOUT having claimed (partial-success
// avoided).
func LinkSession(store TicketStore, requesting *board.Ticket, uuid string, opts LinkOpts) (written bool, err error) {
	if store == nil || requesting == nil || uuid == "" {
		return false, nil
	}
	if opts.BestEffort && opts.Force {
		return false, fmt.Errorf("ticketsvc: LinkOpts.BestEffort and Force are mutually exclusive")
	}

	// Idempotent: requesting ticket already has this UUID.
	if requesting.AgentSessionID == uuid {
		return false, nil
	}

	matches := store.FindByAgentSessionID(uuid)
	var conflicts []*board.Ticket
	var conflictIDs []board.TicketID
	for _, t := range matches {
		if t.ID != requesting.ID {
			conflicts = append(conflicts, t)
			conflictIDs = append(conflictIDs, t.ID)
		}
	}

	if len(conflicts) > 0 {
		switch {
		case opts.BestEffort:
			return false, nil
		case !opts.Force:
			return false, &ErrSessionAlreadyLinked{UUID: uuid, ConflictTicketIDs: conflictIDs}
		default:
			// Force: clear conflicts first.
			for _, t := range conflicts {
				t.AgentSessionID = ""
				t.Touch()
				if serr := store.Save(t); serr != nil {
					return false, fmt.Errorf("ticketsvc: clear conflict ticket %s: %w", t.ID, serr)
				}
			}
		}
	}

	requesting.AgentSessionID = uuid
	requesting.Touch()
	// Caller persists requesting via store.SaveTicket — see doc comment.
	return true, nil
}

// SessionProbe queries whether a Claude session UUID is currently held.
//
//   - lsof.PID > 0 means a local process holds the JSONL.
//   - daemonOwn != nil && daemonOwn.Owned means the openkanban daemon
//     has an existing session bound to this UUID.
//
// Returning an error is reserved for genuine probe failures (lsof
// missing, daemon dial broken on a path that wasn't the canonical
// "daemon down" signal). A clean "not held" is (zero, nil, nil).
//
// Implementations:
//   - Production: RealProbe (wraps agent.SessionActive + daemonclient
//     Owns RPC). Built once per spawn site.
//   - Tests: inline closures. Pass a recorded fake to assert call shape.
type SessionProbe func(uuid string) (lsof agent.SessionHolder, daemonOwn *daemon.OwnsResp, err error)

// GateAttach is the shared check before launching a new Claude on a
// ticket whose AgentSessionID is non-empty. Returns nil when attach is
// safe; *ErrSessionInUse with the holder identity when it isn't.
//
// daemonOwn matching selfDaemonSessionID is treated as "idempotent
// re-attach" (the daemon already has a session for us — Spawn's per-
// TicketID idempotency would have returned it; we shouldn't refuse).
// Pass selfDaemonSessionID="" when there's no known existing session
// for this ticket — any daemon-owns is then foreign and refused.
//
// lsof is a hard refuse regardless of selfDaemonSessionID: the daemon
// owns its child Claude's FDs, so reaching GateAttach with a non-zero
// lsof PID means an EXTERNAL process (a user-launched `claude --resume`,
// a sibling TUI's child, etc.) is holding the JSONL.
func GateAttach(probe SessionProbe, uuid string, selfDaemonSessionID string) error {
	if probe == nil || uuid == "" {
		return nil
	}
	lsof, daemonOwn, err := probe(uuid)
	if err != nil {
		return fmt.Errorf("ticketsvc: probe session %s: %w", uuid, err)
	}
	if lsof.PID > 0 {
		return &ErrSessionInUse{UUID: uuid, HolderPID: lsof.PID, HolderPath: lsof.Path}
	}
	if daemonOwn != nil && daemonOwn.Owned {
		if daemonOwn.SessionID == selfDaemonSessionID {
			return nil
		}
		return &ErrSessionInUse{UUID: uuid, DaemonSessionID: daemonOwn.SessionID}
	}
	return nil
}
