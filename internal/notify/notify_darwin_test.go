//go:build darwin

package notify

import (
	"os"
	"testing"
)

// TestSend_E2E actually fires a real macOS user notification, so it's gated
// behind OPENKANBAN_NOTIFY_E2E=1 to keep `go test ./...` from spamming
// notifications on the test runner.
func TestSend_E2E(t *testing.T) {
	if os.Getenv("OPENKANBAN_NOTIFY_E2E") != "1" {
		t.Skip("skipping; set OPENKANBAN_NOTIFY_E2E=1 to fire a real notification")
	}
	if err := Send("notify package test"); err != nil {
		t.Fatalf("Send returned error: %v", err)
	}
}
