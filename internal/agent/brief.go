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

// MergeTicketBrief writes/updates the ticket's brief file inside the
// worktree at tickets/<slug>.md, syncing the openkanban card's
// Description into a managed block. Returns the path relative to the
// worktree (forward-slashed for template embedding), whether the file
// exists after the call, and any non-fatal error.
//
// Matrix:
//   - empty worktreePath OR empty slug                              → ("", false, nil)
//   - file absent + description blank                                → ("", false, nil)
//   - file absent + description non-blank                            → CREATE with title+block; return (rel, true, nil)
//   - file present + description blank                               → no write; return (rel, true, nil)
//   - file present + description non-blank, block exists             → REPLACE block contents; write only if content changed
//   - file present + description non-blank, block missing            → APPEND block; write
//
// Errors from os.MkdirAll / os.WriteFile are returned wrapped. Read
// errors other than IsNotExist are returned wrapped (caller decides
// whether to ignore).
func MergeTicketBrief(ticket *board.Ticket, worktreePath string) (string, bool, error) {
	if worktreePath == "" || ticket == nil {
		return "", false, nil
	}
	slug := BranchSlug(ticket.BranchName)
	if slug == "" {
		return "", false, nil
	}

	relPath := briefSubdir + "/" + slug + ".md"
	fullPath := filepath.Join(worktreePath, briefSubdir, slug+".md")
	desc := strings.TrimSpace(ticket.Description)

	existing, err := os.ReadFile(fullPath)
	fileAbsent := err != nil && os.IsNotExist(err)
	if err != nil && !fileAbsent {
		return relPath, false, fmt.Errorf("read brief %s: %w", fullPath, err)
	}

	switch {
	case fileAbsent && desc == "":
		return "", false, nil

	case fileAbsent && desc != "":
		content := initialBriefContent(ticket.Title, desc)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			return relPath, false, fmt.Errorf("mkdir brief dir: %w", err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
			return relPath, false, fmt.Errorf("write brief %s: %w", fullPath, err)
		}
		return relPath, true, nil

	case !fileAbsent && desc == "":
		return relPath, true, nil

	default: // !fileAbsent && desc != ""
		updated := upsertManagedBlock(string(existing), desc)
		if updated == string(existing) {
			return relPath, true, nil
		}
		if err := os.WriteFile(fullPath, []byte(updated), 0o644); err != nil {
			return relPath, false, fmt.Errorf("write brief %s: %w", fullPath, err)
		}
		return relPath, true, nil
	}
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
