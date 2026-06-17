package ui

import (
	"path/filepath"

	"github.com/techdufus/openkanban/internal/agent"
	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/project"
	"github.com/techdufus/openkanban/internal/ticketsvc"
)

// historyPurger is the injection seam for
// agent.PurgeClaudePrimingHistory. Real callers pass the real function;
// tests pass a recording fake to assert call-vs-no-call (the 1:1
// invariant gate must prevent purge-when-claim-refused).
type historyPurger func(historyPath, uuid string, prefixes ...string) error

// backfillAgentSession runs the post-spawn UUID back-fill step for one
// pane in pollAgentStatusesAsync. Returns the JSONL UUID discovered for
// the pane's worktree (or empty when no JSONL exists, the ticket isn't
// in the store, or the BestEffort claim was refused because a different
// ticket already holds this UUID).
//
// When LinkSession returns written=true (claim succeeded — either fresh
// or idempotent re-claim is filtered out earlier), this function:
//   - Saves the ticket via globalStore.Save (LinkSession itself doesn't
//     save the requester — caller's responsibility per ticketsvc contract).
//   - For claude agents only: purges the priming-prompt entry openkanban
//     caused claude to write into ~/.claude/history.jsonl at spawn-time,
//     via the injected historyPurger.
//
// When LinkSession returns written=false (conflict or idempotent
// already-claimed), neither save nor purge fires. The returned UUID
// is empty so the caller's apiSessionID stays empty for this poll
// tick, and status detection falls back to terminal-content scraping.
// This is the load-bearing gate that prevents two tickets from racing
// on the same UUID's history purge.
func backfillAgentSession(
	globalStore *project.GlobalTicketStore,
	ticketID board.TicketID,
	agentType, worktreePath, homeDir string,
	findOpencode, findClaude func(string) string,
	purger historyPurger,
) string {
	if globalStore == nil || worktreePath == "" {
		return ""
	}
	var id string
	var isClaude bool
	switch agentType {
	case "opencode":
		id = findOpencode(worktreePath)
	case "claude":
		id = findClaude(worktreePath)
		isClaude = true
	default:
		return ""
	}
	if id == "" {
		return ""
	}
	ticket, _ := globalStore.Get(ticketID)
	if ticket == nil {
		return ""
	}
	written, _ := ticketsvc.LinkSession(globalStore, ticket, id, ticketsvc.LinkOpts{BestEffort: true})
	if !written {
		return ""
	}
	_ = globalStore.Save(ticket)
	if isClaude && homeDir != "" && purger != nil {
		historyPath := filepath.Join(homeDir, ".claude", "history.jsonl")
		_ = purger(historyPath, id, agent.ClaudePrimingPrefixes...)
	}
	return id
}
