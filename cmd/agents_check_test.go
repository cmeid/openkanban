package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestWarnMissingAgentsIfNeeded(t *testing.T) {
	// resolver that reports a fixed set of agents as present.
	resolverFor := func(present ...string) func(string) bool {
		set := make(map[string]bool, len(present))
		for _, p := range present {
			set[p] = true
		}
		return func(name string) bool { return set[name] }
	}

	tests := []struct {
		name        string
		expected    []string
		resolve     func(string) bool
		isTTY       bool
		wantWarn    bool
		wantContain []string
	}{
		{
			name:     "all present, no warning",
			expected: []string{"code-reviewer", "validator"},
			resolve:  resolverFor("code-reviewer", "validator"),
			isTTY:    true,
			wantWarn: false,
		},
		{
			name:        "one missing, warns and names it",
			expected:    []string{"code-reviewer", "validator"},
			resolve:     resolverFor("code-reviewer"),
			isTTY:       true,
			wantWarn:    true,
			wantContain: []string{"validator"},
		},
		{
			name:        "all missing, warns and names all",
			expected:    []string{"code-reviewer", "validator"},
			resolve:     resolverFor(),
			isTTY:       true,
			wantWarn:    true,
			wantContain: []string{"code-reviewer", "validator"},
		},
		{
			name:     "missing but non-TTY stays silent",
			expected: []string{"validator"},
			resolve:  resolverFor(),
			isTTY:    false,
			wantWarn: false,
		},
		{
			name:     "nil resolver stays silent",
			expected: []string{"validator"},
			resolve:  nil,
			isTTY:    true,
			wantWarn: false,
		},
		{
			name:     "empty expected stays silent",
			expected: nil,
			resolve:  resolverFor(),
			isTTY:    true,
			wantWarn: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			warnMissingAgentsIfNeeded(tt.expected, tt.resolve, tt.isTTY, &buf)
			out := buf.String()
			if tt.wantWarn && out == "" {
				t.Fatalf("expected a warning, got none")
			}
			if !tt.wantWarn && out != "" {
				t.Fatalf("expected no warning, got %q", out)
			}
			for _, want := range tt.wantContain {
				if !strings.Contains(out, want) {
					t.Errorf("warning %q missing expected substring %q", out, want)
				}
			}
		})
	}
}
