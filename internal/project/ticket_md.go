package project

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/techdufus/openkanban/internal/board"
)

// validStatuses is the closed set of TicketStatus values openkanban
// understands. Anything else in frontmatter is a hand-edit typo.
var validStatuses = map[board.TicketStatus]bool{
	board.StatusBacklog:    true,
	board.StatusInProgress: true,
	board.StatusDone:       true,
	board.StatusArchived:   true,
}

// validAgentStatuses mirrors board.AgentStatus enum.
var validAgentStatuses = map[board.AgentStatus]bool{
	board.AgentNone:      true,
	board.AgentIdle:      true,
	board.AgentWorking:   true,
	board.AgentWaiting:   true,
	board.AgentCompleted: true,
	board.AgentError:     true,
}

// validAgentTypes are the agent flavours the spawn flow knows.
// Empty is allowed (legacy / unset).
var validAgentTypes = map[string]bool{
	"":         true,
	"claude":   true,
	"opencode": true,
	"aider":    true,
	"gemini":   true,
	"codex":    true,
}

const (
	frontmatterFence    = "---\n"
	frontmatterFenceLen = len(frontmatterFence)
	fenceClose          = "\n---\n"
	fenceCloseLen       = len(fenceClose)
)

// ticketFrontmatter mirrors board.Ticket but with yaml tags and a
// deterministic field order chosen for diff-friendliness:
// identity → status → temporal → worktree → agent.
// Description lives in the body, not the frontmatter.
type ticketFrontmatter struct {
	ID        board.TicketID     `yaml:"id"`
	ProjectID string             `yaml:"project_id"`
	Title     string             `yaml:"title"`
	Status    board.TicketStatus `yaml:"status"`
	Priority  int                `yaml:"priority"`
	Labels    []string           `yaml:"labels"`

	CreatedAt   time.Time  `yaml:"created_at"`
	UpdatedAt   time.Time  `yaml:"updated_at"`
	StartedAt   *time.Time `yaml:"started_at,omitempty"`
	CompletedAt *time.Time `yaml:"completed_at,omitempty"`

	UseWorktree  bool   `yaml:"use_worktree"`
	WorktreePath string `yaml:"worktree_path,omitempty"`
	BranchName   string `yaml:"branch_name,omitempty"`
	BaseBranch   string `yaml:"base_branch,omitempty"`

	AgentType        string            `yaml:"agent_type,omitempty"`
	AgentStatus      board.AgentStatus `yaml:"agent_status"`
	AgentSpawnedAt   *time.Time        `yaml:"agent_spawned_at,omitempty"`
	AgentPort        int               `yaml:"agent_port,omitempty"`
	AgentSessionID   string            `yaml:"agent_session_id,omitempty"`
	SessionOwned     bool              `yaml:"session_owned,omitempty"`
	CreatedBySession string            `yaml:"created_by_session,omitempty"`

	BlockedBy []board.TicketID  `yaml:"blocked_by"`
	Meta      map[string]string `yaml:"meta"`
}

func toFrontmatter(t *board.Ticket) ticketFrontmatter {
	labels := t.Labels
	if labels == nil {
		labels = []string{}
	}
	meta := t.Meta
	if meta == nil {
		meta = map[string]string{}
	}
	blocked := t.BlockedBy
	if blocked == nil {
		blocked = []board.TicketID{}
	}

	return ticketFrontmatter{
		ID:               t.ID,
		ProjectID:        t.ProjectID,
		Title:            t.Title,
		Status:           t.Status,
		Priority:         t.Priority,
		Labels:           labels,
		CreatedAt:        t.CreatedAt,
		UpdatedAt:        t.UpdatedAt,
		StartedAt:        t.StartedAt,
		CompletedAt:      t.CompletedAt,
		UseWorktree:      t.UseWorktree,
		WorktreePath:     t.WorktreePath,
		BranchName:       t.BranchName,
		BaseBranch:       t.BaseBranch,
		AgentType:        t.AgentType,
		AgentStatus:      t.AgentStatus,
		AgentSpawnedAt:   t.AgentSpawnedAt,
		AgentPort:        t.AgentPort,
		AgentSessionID:   t.AgentSessionID,
		SessionOwned:     t.SessionOwned,
		CreatedBySession: t.CreatedBySession,
		BlockedBy:        blocked,
		Meta:             meta,
	}
}

func fromFrontmatter(fm ticketFrontmatter, body string) *board.Ticket {
	labels := fm.Labels
	if labels == nil {
		labels = []string{}
	}
	meta := fm.Meta
	if meta == nil {
		meta = map[string]string{}
	}
	blocked := fm.BlockedBy
	if blocked == nil {
		blocked = []board.TicketID{}
	}

	return &board.Ticket{
		ID:               fm.ID,
		ProjectID:        fm.ProjectID,
		Title:            fm.Title,
		Description:      body,
		Status:           fm.Status,
		UseWorktree:      fm.UseWorktree,
		WorktreePath:     fm.WorktreePath,
		BranchName:       fm.BranchName,
		BaseBranch:       fm.BaseBranch,
		AgentType:        fm.AgentType,
		AgentStatus:      fm.AgentStatus,
		AgentSpawnedAt:   fm.AgentSpawnedAt,
		AgentPort:        fm.AgentPort,
		AgentSessionID:   fm.AgentSessionID,
		SessionOwned:     fm.SessionOwned,
		CreatedBySession: fm.CreatedBySession,
		CreatedAt:        fm.CreatedAt,
		UpdatedAt:        fm.UpdatedAt,
		StartedAt:        fm.StartedAt,
		CompletedAt:      fm.CompletedAt,
		Labels:           labels,
		Priority:         fm.Priority,
		Meta:             meta,
		BlockedBy:        blocked,
	}
}

// MarshalTicket renders a ticket as Markdown with YAML frontmatter.
//
// Format:
//
//	---
//	<yaml fields, deterministic order>
//	---
//
//	<Description body, verbatim>
//
// Description always lives in the body so callers editing the file in
// $EDITOR can use markdown without escaping concerns.
func MarshalTicket(t *board.Ticket) ([]byte, error) {
	if t == nil {
		return nil, errors.New("MarshalTicket: nil ticket")
	}

	fm := toFrontmatter(t)
	yml, err := yaml.Marshal(&fm)
	if err != nil {
		return nil, fmt.Errorf("marshal frontmatter: %w", err)
	}

	// Canonical Description: no trailing newlines. Marshal adds exactly
	// one trailing newline to the on-disk file (POSIX-clean). Unmarshal
	// strips trailing newlines. Round-trip is stable: f(Marshal, Unmarshal)
	// returns the input Description with trailing \n stripped.
	body := strings.TrimRight(t.Description, "\n")

	var buf bytes.Buffer
	buf.Grow(len(yml) + len(body) + 16)
	buf.WriteString(frontmatterFence)
	buf.Write(yml)
	buf.WriteString("---\n\n")
	buf.WriteString(body)
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// UnmarshalTicket parses a ticket file. Returns a wrapped error if
// the frontmatter delimiters are missing or the YAML is malformed.
func UnmarshalTicket(data []byte) (*board.Ticket, error) {
	s := string(data)
	if !strings.HasPrefix(s, frontmatterFence) {
		return nil, errors.New("missing leading frontmatter delimiter (---)")
	}
	rest := s[frontmatterFenceLen:]

	end := strings.Index(rest, fenceClose)
	if end == -1 {
		return nil, errors.New("missing closing frontmatter delimiter (---)")
	}

	yamlPart := rest[:end+1] // include the trailing \n before ---
	body := rest[end+fenceCloseLen:]
	body = strings.TrimLeft(body, "\n")
	body = strings.TrimRight(body, "\n")

	var fm ticketFrontmatter
	if err := yaml.Unmarshal([]byte(yamlPart), &fm); err != nil {
		return nil, fmt.Errorf("parse frontmatter: %w", err)
	}

	if err := validateFrontmatter(&fm); err != nil {
		return nil, err
	}

	return fromFrontmatter(fm, body), nil
}

// validateFrontmatter checks load-bearing fields against the
// allowlists and backfills sensible defaults for missing optional
// values. Mutates fm in place for backfills; returns an error for
// hard-required failures.
//
// Policy: lenient on missing values (so hand-edited or partially-
// migrated files still load), strict on present-but-invalid values
// (so typos in enums surface as errors instead of silent loss).
func validateFrontmatter(fm *ticketFrontmatter) error {
	if strings.TrimSpace(string(fm.ID)) == "" {
		return fmt.Errorf("required field id is empty or missing")
	}
	if strings.TrimSpace(fm.Title) == "" {
		return fmt.Errorf("required field title is empty or missing")
	}

	if fm.Status == "" {
		fm.Status = board.StatusBacklog
	} else if !validStatuses[fm.Status] {
		return fmt.Errorf("invalid status %q (allowed: backlog, in_progress, done, archived)", fm.Status)
	}

	if fm.AgentStatus == "" {
		fm.AgentStatus = board.AgentNone
	} else if !validAgentStatuses[fm.AgentStatus] {
		return fmt.Errorf("invalid agent_status %q (allowed: none, idle, working, waiting, completed, error)", fm.AgentStatus)
	}

	if !validAgentTypes[fm.AgentType] {
		return fmt.Errorf("invalid agent_type %q (allowed: claude, opencode, aider, gemini, codex, or empty)", fm.AgentType)
	}

	now := time.Now()
	if fm.CreatedAt.IsZero() {
		fm.CreatedAt = now
	}
	if fm.UpdatedAt.IsZero() {
		fm.UpdatedAt = now
	}

	if fm.Priority == 0 {
		fm.Priority = 3
	}

	return nil
}

// TicketFilename returns the canonical on-disk filename for a ticket.
// Format: <slug>-<uuid8>.md. The UUID prefix disambiguates tickets
// with identical slugs and makes minor title tweaks rename-stable
// when paired with the rename safety in SaveTicket.
//
// Identity is always the frontmatter id field, never the filename;
// callers MUST NOT parse identity out of the filename.
func TicketFilename(t *board.Ticket) string {
	slug := board.Slugify(t.Title, 40)
	if slug == "" {
		slug = "untitled"
	}
	uuid := string(t.ID)
	uuidTail := uuid
	if len(uuidTail) > 8 {
		uuidTail = uuidTail[:8]
	}
	return slug + "-" + uuidTail + ".md"
}
