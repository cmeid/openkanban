package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/techdufus/openkanban/internal/board"
)

const (
	briefSubdir     = "tickets"
	briefBlockStart = "<!-- openkanban:card-notes start -->"
	briefBlockEnd   = "<!-- openkanban:card-notes end -->"
	briefBlockTitle = "## Notes (from openkanban card)"
)

// briefBlockPattern matches the managed block, including the fence comments
// and any content between them. Non-greedy so multiple blocks in a file
// don't merge into one match (though we only ever write one block).
var briefBlockPattern = regexp.MustCompile(`(?s)<!-- openkanban:card-notes start -->.*?<!-- openkanban:card-notes end -->`)

// BranchSlug returns the ticket slug derived from a branch name by
// stripping the "task/" prefix. Returns "" if the branch name is empty or
// is exactly "task/" (no slug content). Branch names without the prefix
// are returned as-is.
func BranchSlug(branchName string) string {
	s := strings.TrimPrefix(branchName, "task/")
	return s
}

// PreviewBriefMerge computes what MergeTicketBrief WOULD write without
// touching disk. Returns:
//   - briefRelPath: the worktree-relative path (e.g. "tickets/foo.md")
//   - hasBrief: whether the file would exist after the (hypothetical) write
//   - wouldChange: whether the on-disk bytes would differ after merge.
//     Defined strictly as "the bytes on disk would not equal the current bytes."
//   - content: the bytes that MergeTicketBrief would write (only meaningful
//     when wouldChange == true)
//   - err: only set for unexpected read errors (not for IsNotExist)
//
// The function performs the same 4-case switch as MergeTicketBrief but
// is strictly read-only.
func PreviewBriefMerge(ticket *board.Ticket, worktreePath string) (briefRelPath string, hasBrief bool, wouldChange bool, content string, err error) {
	if worktreePath == "" || ticket == nil {
		return "", false, false, "", nil
	}
	slug := BranchSlug(ticket.BranchName)
	if slug == "" {
		return "", false, false, "", nil
	}

	relPath := briefSubdir + "/" + slug + ".md"
	fullPath := filepath.Join(worktreePath, briefSubdir, slug+".md")
	desc := strings.TrimSpace(ticket.Description)

	existing, err := os.ReadFile(fullPath)
	fileAbsent := err != nil && os.IsNotExist(err)
	if err != nil && !fileAbsent {
		return relPath, false, false, "", fmt.Errorf("read brief %s: %w", fullPath, err)
	}

	switch {
	case fileAbsent && desc == "":
		return "", false, false, "", nil
	case fileAbsent && desc != "":
		return relPath, true, true, initialBriefContent(ticket.Title, desc), nil
	case !fileAbsent && desc == "":
		return relPath, true, false, string(existing), nil
	default: // !fileAbsent && desc != ""
		updated := upsertManagedBlock(string(existing), desc)
		if updated == string(existing) {
			return relPath, true, false, updated, nil
		}
		return relPath, true, true, updated, nil
	}
}

// MergeTicketBrief writes/updates the ticket's brief file inside the
// worktree at tickets/<slug>.md, syncing the openkanban card's
// Description into a managed block. Returns the path relative to the
// worktree (forward-slashed for template embedding), whether the file
// exists after the call, and any non-fatal error.
//
// Delegates the merge computation to PreviewBriefMerge and only performs
// I/O when the on-disk bytes would actually change.
//
// Concurrency contract: the openkanban store's ticket.Description is the
// source of truth; the brief is a one-way generated view of it. This
// function only ever rewrites the bytes between the managed-block fences
// (see upsertManagedBlock), so any content the agent writes outside the
// block is preserved — and is worktree-only state the store has no copy
// of. The write is atomic (temp+rename), so a concurrent reader always
// observes a complete brief (old or new, never torn).
//
// Matrix:
//   - empty worktreePath OR empty slug                              → ("", false, nil)
//   - file absent + description blank                                → ("", false, nil)
//   - file absent + description non-blank                            → CREATE with title+block; return (rel, true, nil)
//   - file present + description blank                               → no write; return (rel, true, nil)
//   - file present + description non-blank, block exists             → REPLACE block contents; write only if content changed
//   - file present + description non-blank, block missing            → APPEND block; write
//
// Errors from os.MkdirAll and the temp-file write/rename are returned
// wrapped. Read errors other than IsNotExist are returned wrapped (caller
// decides whether to ignore).
func MergeTicketBrief(ticket *board.Ticket, worktreePath string) (string, bool, error) {
	relPath, hasBrief, wouldChange, content, err := PreviewBriefMerge(ticket, worktreePath)
	if err != nil {
		return relPath, hasBrief, err
	}
	if !wouldChange {
		return relPath, hasBrief, nil
	}
	fullPath := filepath.Join(worktreePath, briefSubdir, BranchSlug(ticket.BranchName)+".md")
	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return relPath, hasBrief, fmt.Errorf("mkdir brief dir: %w", err)
	}

	// Atomic write via temp+rename so a concurrent reader — the spawned
	// agent, or a second TUI re-syncing the same worktree — never sees a
	// torn/partial brief, and two writers can't corrupt each other through
	// a truncate window. The temp file lives in the target's own dir so the
	// rename stays on one filesystem. Mirrors TicketStore.SaveTicket
	// (internal/project/tickets.go), the established pattern for safe
	// concurrent writes from multiple openkanban processes.
	tmp, err := os.CreateTemp(dir, filepath.Base(fullPath)+".tmp-*")
	if err != nil {
		return relPath, hasBrief, fmt.Errorf("create tmp brief: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write([]byte(content)); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return relPath, hasBrief, fmt.Errorf("write tmp brief: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return relPath, hasBrief, fmt.Errorf("close tmp brief: %w", err)
	}
	// CreateTemp yields 0600; restore the 0644 the brief used before this
	// path was atomic.
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		os.Remove(tmpPath)
		return relPath, hasBrief, fmt.Errorf("chmod tmp brief: %w", err)
	}
	if err := os.Rename(tmpPath, fullPath); err != nil {
		os.Remove(tmpPath)
		return relPath, hasBrief, fmt.Errorf("rename brief %s: %w", fullPath, err)
	}
	return relPath, hasBrief, nil
}

// initialBriefContent is the bootstrap layout when the brief file
// doesn't exist yet: H1 title + the managed block carrying the
// description. The user is free to add structure around the block on
// subsequent edits.
func initialBriefContent(title, desc string) string {
	var sb strings.Builder
	if t := strings.TrimSpace(title); t != "" {
		sb.WriteString("# ")
		sb.WriteString(t)
		sb.WriteString("\n\n")
	}
	sb.WriteString(briefBlockStart)
	sb.WriteString("\n")
	sb.WriteString(briefBlockTitle)
	sb.WriteString("\n\n")
	sb.WriteString(desc)
	sb.WriteString("\n")
	sb.WriteString(briefBlockEnd)
	sb.WriteString("\n")
	return sb.String()
}

// upsertManagedBlock replaces the contents of an existing
// <!-- openkanban:card-notes ... --> fence pair, or appends a new block
// at the end if no fences are present. Trailing newlines are normalized
// so repeated calls converge.
func upsertManagedBlock(existing, desc string) string {
	block := briefBlockStart + "\n" + briefBlockTitle + "\n\n" + desc + "\n" + briefBlockEnd
	if briefBlockPattern.MatchString(existing) {
		return briefBlockPattern.ReplaceAllString(existing, block)
	}
	sep := "\n\n"
	if strings.HasSuffix(existing, "\n\n") {
		sep = ""
	} else if strings.HasSuffix(existing, "\n") {
		sep = "\n"
	} else if existing == "" {
		sep = ""
	}
	return existing + sep + block + "\n"
}
