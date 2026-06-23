package ui

import (
	"testing"

	"github.com/techdufus/openkanban/internal/board"
)

// TestNextStatusChainIncludesNext asserts the forward quick-move chain
// threads through the new Next status: Backlog → Next → In Progress →
// In Review → Done (and Done is terminal). nextStatus reads no Model
// state, so a zero Model is sufficient.
func TestNextStatusChainIncludesNext(t *testing.T) {
	m := &Model{}
	cases := []struct {
		from, want board.TicketStatus
	}{
		{board.StatusBacklog, board.StatusNext},
		{board.StatusNext, board.StatusInProgress},
		{board.StatusInProgress, board.StatusInReview},
		{board.StatusInReview, board.StatusDone},
		{board.StatusDone, board.StatusDone}, // terminal
	}
	for _, c := range cases {
		if got := m.nextStatus(c.from); got != c.want {
			t.Errorf("nextStatus(%q) = %q; want %q", c.from, got, c.want)
		}
	}
}

// TestPreviousStatusChainIncludesNext asserts the backward chain is the
// exact inverse: Done → In Review → In Progress → Next → Backlog (and
// Backlog is the floor).
func TestPreviousStatusChainIncludesNext(t *testing.T) {
	m := &Model{}
	cases := []struct {
		from, want board.TicketStatus
	}{
		{board.StatusDone, board.StatusInReview},
		{board.StatusInReview, board.StatusInProgress},
		{board.StatusInProgress, board.StatusNext},
		{board.StatusNext, board.StatusBacklog},
		{board.StatusBacklog, board.StatusBacklog}, // floor
	}
	for _, c := range cases {
		if got := m.previousStatus(c.from); got != c.want {
			t.Errorf("previousStatus(%q) = %q; want %q", c.from, got, c.want)
		}
	}
}
