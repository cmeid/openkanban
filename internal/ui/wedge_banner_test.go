package ui

import (
	"strings"
	"testing"

	"github.com/techdufus/openkanban/internal/daemon"
)

// TestApplyDaemonSessionEvent_WedgeBannerToggles verifies the daemon-global
// wedge signal flips the banner flag: daemon_wedged sets it, daemon_unwedged
// clears it. These events carry no TicketID and must be handled before the
// per-ticket block.
func TestApplyDaemonSessionEvent_WedgeBannerToggles(t *testing.T) {
	m := newTakeoverTestModel(t)
	if m.daemonWedged {
		t.Fatal("precondition: daemonWedged should start false")
	}

	m.applyDaemonSessionEvent(daemon.SessionEvent{Event: "daemon_wedged", Reason: "no dispatch completion"})
	if !m.daemonWedged {
		t.Fatal("daemon_wedged did not set m.daemonWedged")
	}

	m.applyDaemonSessionEvent(daemon.SessionEvent{Event: "daemon_unwedged"})
	if m.daemonWedged {
		t.Fatal("daemon_unwedged did not clear m.daemonWedged")
	}
}

// TestRenderHeader_ShowsWedgeBanner: when wedged, the header surfaces the
// warning + recovery hint (in place of the help cluster, same line height).
func TestRenderHeader_ShowsWedgeBanner(t *testing.T) {
	m := newTakeoverTestModel(t)
	m.mode = ModeNormal

	if got := m.renderHeader(); strings.Contains(got, "daemon wedged") {
		t.Fatal("header showed the wedge banner while not wedged")
	}

	m.daemonWedged = true
	got := m.renderHeader()
	if !strings.Contains(got, "daemon wedged") {
		t.Errorf("header missing wedge banner when wedged; got:\n%s", got)
	}
	if !strings.Contains(got, "daemon restart") {
		t.Errorf("wedge banner missing the recovery hint; got:\n%s", got)
	}
}
