package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"github.com/techdufus/openkanban/internal/board"
)

type ContextData struct {
	Title        string
	Description  string
	BranchName   string
	BaseBranch   string
	TicketID     string
	Status       string
	WorktreePath string
}

func BuildContextPrompt(promptTemplate string, ticket *board.Ticket) string {
	if promptTemplate == "" {
		return ""
	}

	data := ContextData{
		Title:        ticket.Title,
		Description:  ticket.Description,
		BranchName:   ticket.BranchName,
		BaseBranch:   ticket.BaseBranch,
		TicketID:     string(ticket.ID),
		Status:       string(ticket.Status),
		WorktreePath: ticket.WorktreePath,
	}

	tmpl, err := template.New("prompt").Parse(promptTemplate)
	if err != nil {
		return buildFallbackPrompt(ticket)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return buildFallbackPrompt(ticket)
	}

	return scrubMissingFileReferences(buf.String(), ticket.WorktreePath)
}

// mdPathPattern matches markdown-style path tokens that end in `.md`.
// Path-like characters: word chars, dots, slashes, dashes, tildes, plus
// angle brackets so literal placeholders like `<slug>` are captured (and
// will resolve to a missing file, which is the desired signal).
var mdPathPattern = regexp.MustCompile(`[\w./~<>-]+\.md`)

// scrubMissingFileReferences removes paragraphs and orphaned `## ` headers
// that reference `.md` files which don't exist on disk. Relative paths
// resolve against worktreePath; `~/` is expanded; absolute paths are used
// as-is. Paragraphs without `.md` references are preserved verbatim.
func scrubMissingFileReferences(rendered, worktreePath string) string {
	if !strings.Contains(rendered, ".md") {
		return rendered
	}

	homeDir, _ := os.UserHomeDir()
	missing := func(p string) bool {
		_, err := os.Stat(p)
		return os.IsNotExist(err)
	}
	resolve := func(p string) string {
		switch {
		case strings.HasPrefix(p, "~/"):
			if homeDir == "" {
				return p
			}
			return filepath.Join(homeDir, p[2:])
		case filepath.IsAbs(p):
			return p
		default:
			if worktreePath == "" {
				return p
			}
			return filepath.Join(worktreePath, p)
		}
	}

	paragraphs := splitParagraphs(rendered)
	kept := make([]string, 0, len(paragraphs))
	for _, para := range paragraphs {
		paths := mdPathPattern.FindAllString(para, -1)
		drop := false
		for _, p := range paths {
			if missing(resolve(p)) {
				drop = true
				break
			}
		}
		if !drop {
			kept = append(kept, para)
		}
	}

	kept = dropOrphanedHeaders(kept)
	return strings.Join(kept, "\n\n")
}

// splitParagraphs splits text on runs of blank lines and trims each block.
// Empty blocks are dropped so the output joins cleanly with "\n\n".
func splitParagraphs(s string) []string {
	parts := regexp.MustCompile(`\n[ \t]*\n+`).Split(s, -1)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.Trim(p, "\n")
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// dropOrphanedHeaders removes `## `-style headers that are immediately
// followed by another header or end-of-document (i.e. their body was
// scrubbed away).
func dropOrphanedHeaders(paragraphs []string) []string {
	isHeader := func(p string) bool {
		first := strings.SplitN(p, "\n", 2)[0]
		return strings.HasPrefix(strings.TrimLeft(first, " "), "#")
	}
	// A paragraph is "header-only" if every line is a header line.
	isHeaderOnly := func(p string) bool {
		for _, line := range strings.Split(p, "\n") {
			if !strings.HasPrefix(strings.TrimLeft(line, " "), "#") {
				return false
			}
		}
		return true
	}
	out := make([]string, 0, len(paragraphs))
	for i, p := range paragraphs {
		if isHeaderOnly(p) {
			// Drop if no following paragraph, or next paragraph is also a header.
			if i == len(paragraphs)-1 || isHeader(paragraphs[i+1]) {
				continue
			}
		}
		out = append(out, p)
	}
	return out
}

func buildFallbackPrompt(ticket *board.Ticket) string {
	var sb strings.Builder
	sb.WriteString("Task: ")
	sb.WriteString(ticket.Title)
	if ticket.Description != "" {
		sb.WriteString("\n\n")
		sb.WriteString(ticket.Description)
	}
	return sb.String()
}

func ShouldInjectContext(ticket *board.Ticket) bool {
	return ticket.AgentSpawnedAt == nil
}
