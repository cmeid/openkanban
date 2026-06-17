package app

import "testing"

// TestShortID covers the truncation idiom used when printing project IDs in
// ListProjects. A short ID (< 8 chars) must be returned verbatim rather than
// panicking on a bare id[:8] slice-bounds-out-of-range.
func TestShortID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want string
	}{
		{name: "short id returned verbatim", id: "proj-1", want: "proj-1"},
		{name: "empty id", id: "", want: ""},
		{name: "exactly 8 chars unchanged", id: "12345678", want: "12345678"},
		{name: "long uuid truncated to 8", id: "60d7d8e2-aaaa-bbbb", want: "60d7d8e2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shortID(tt.id); got != tt.want {
				t.Errorf("shortID(%q) = %q, want %q", tt.id, got, tt.want)
			}
		})
	}
}
