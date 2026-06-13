package agent

import (
	"bytes"
	"strings"
	"text/template"

	"github.com/techdufus/openkanban/internal/board"
)

// ContextData is the template payload for the agent priming prompt.
// Fields are populated from a board.Ticket plus spawn-time computed
// extras (brief file presence, resume mode). All fields are exported
// so they can appear in Go template expressions like {{.Title}}.
type ContextData struct {
	Title            string
	Description      string
	BranchName       string
	BaseBranch       string
	TicketID         string
	Status           string
	WorktreePath     string
	Slug             string
	HasBrief         bool
	BriefPath        string
	IsExternalResume bool
}

// NewContextData assembles a ContextData from a ticket plus the
// spawn-time extras. Slug is derived from ticket.BranchName via
// BranchSlug.
func NewContextData(ticket *board.Ticket, briefRelPath string, hasBrief, isExternalResume bool) ContextData {
	if ticket == nil {
		return ContextData{
			BriefPath:        briefRelPath,
			HasBrief:         hasBrief,
			IsExternalResume: isExternalResume,
		}
	}
	return ContextData{
		Title:            ticket.Title,
		Description:      ticket.Description,
		BranchName:       ticket.BranchName,
		BaseBranch:       ticket.BaseBranch,
		TicketID:         string(ticket.ID),
		Status:           string(ticket.Status),
		WorktreePath:     ticket.WorktreePath,
		Slug:             BranchSlug(ticket.BranchName),
		HasBrief:         hasBrief,
		BriefPath:        briefRelPath,
		IsExternalResume: isExternalResume,
	}
}

// BuildContextPrompt renders the Go template against data. On parse or
// execute error, falls back to a minimal Title+Description prompt so
// the agent never receives an empty string when there's content to
// communicate.
func BuildContextPrompt(promptTemplate string, data ContextData) string {
	if promptTemplate == "" {
		return ""
	}

	tmpl, err := template.New("prompt").Parse(promptTemplate)
	if err != nil {
		return buildFallbackPrompt(data)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return buildFallbackPrompt(data)
	}

	return buf.String()
}

func buildFallbackPrompt(data ContextData) string {
	var sb strings.Builder
	sb.WriteString("Task: ")
	sb.WriteString(data.Title)
	if data.Description != "" {
		sb.WriteString("\n\n")
		sb.WriteString(data.Description)
	}
	return sb.String()
}
