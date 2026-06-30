package app

import (
	"bytes"
	"strings"
	"testing"
)

func TestHostTerminalModeReset(t *testing.T) {
	tests := []struct {
		name    string
		wantSeq string
	}{
		{
			name:    "emits all five DEC reset sequences",
			wantSeq: "\x1b[?1007l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			n, err := writeHostTerminalModeReset(&buf)
			got := buf.String()

			if err != nil {
				t.Fatalf("writeHostTerminalModeReset returned error: %v", err)
			}
			if n != len(tt.wantSeq) {
				t.Errorf("returned n=%d, want %d", n, len(tt.wantSeq))
			}
			// Load-bearing assertion: ?1007l must be present — this is the
			// alt-scroll reset bubbletea never emits, and the reason this
			// guard exists.
			if !strings.Contains(got, "\x1b[?1007l") {
				t.Errorf("output missing \\x1b[?1007l (alt-scroll reset); got %q", got)
			}
			if got != tt.wantSeq {
				t.Errorf("output = %q, want %q", got, tt.wantSeq)
			}
		})
	}
}
