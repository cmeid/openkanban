package project

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/google/uuid"
)

func TestIsValidColor(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"red", "red", true},
		{"green", "green", true},
		{"yellow", "yellow", true},
		{"blue", "blue", true},
		{"magenta", "magenta", true},
		{"cyan", "cyan", true},
		{"brightred", "brightred", true},
		{"brightgreen", "brightgreen", true},
		{"brightyellow", "brightyellow", true},
		{"brightblue", "brightblue", true},
		{"brightmagenta", "brightmagenta", true},
		{"brightcyan", "brightcyan", true},
		{"empty string invalid", "", false},
		{"uppercase invalid", "RED", false},
		{"unknown name invalid", "fuchsia", false},
		{"hex code invalid", "#ff0000", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsValidColor(tt.input)
			if got != tt.want {
				t.Errorf("IsValidColor(%q) = %v; want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestProject_GetColor_Explicit(t *testing.T) {
	p := &Project{ID: "some-id", Color: "blue"}

	got := p.GetColor()
	if got != "blue" {
		t.Errorf("GetColor() = %q; want %q", got, "blue")
	}
}

func TestProject_GetColor_AutoDerived_Deterministic(t *testing.T) {
	p := &Project{ID: "deterministic-id-1234"}

	first := p.GetColor()
	if !IsValidColor(first) {
		t.Fatalf("GetColor() = %q; not in ColorPalette", first)
	}

	for i := 0; i < 100; i++ {
		got := p.GetColor()
		if got != first {
			t.Errorf("GetColor() iter %d = %q; want %q (deterministic)", i, got, first)
		}
	}
}

func TestProject_GetColor_AutoDerived_DistributesAcrossPalette(t *testing.T) {
	hit := make(map[string]bool, len(ColorPalette))

	for i := 0; i < 1000; i++ {
		p := &Project{ID: uuid.New().String()}
		c := p.GetColor()
		if !IsValidColor(c) {
			t.Fatalf("GetColor() = %q; not in ColorPalette", c)
		}
		hit[c] = true
	}

	if len(hit) < 10 {
		t.Errorf("GetColor() distribution hit %d/%d palette entries; want at least 10. hits=%v",
			len(hit), len(ColorPalette), hit)
	}
}

func TestProject_GetColor_InvalidExplicitFallsBack(t *testing.T) {
	p := &Project{ID: "some-id", Color: "invalid"}

	got := p.GetColor()
	if got == "invalid" {
		t.Fatalf("GetColor() = %q; should not return invalid explicit value", got)
	}
	if !IsValidColor(got) {
		t.Errorf("GetColor() = %q; should fall back to a palette member", got)
	}

	// Auto-derivation should match what we'd get with no Color set.
	pNoColor := &Project{ID: p.ID}
	want := pNoColor.GetColor()
	if got != want {
		t.Errorf("GetColor() = %q; want auto-derived %q", got, want)
	}
}

func TestResolveLipglossColor(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  lipgloss.Color
	}{
		{"red", "red", lipgloss.Color("1")},
		{"green", "green", lipgloss.Color("2")},
		{"yellow", "yellow", lipgloss.Color("3")},
		{"blue", "blue", lipgloss.Color("4")},
		{"magenta", "magenta", lipgloss.Color("5")},
		{"cyan", "cyan", lipgloss.Color("6")},
		{"brightred", "brightred", lipgloss.Color("9")},
		{"brightgreen", "brightgreen", lipgloss.Color("10")},
		{"brightyellow", "brightyellow", lipgloss.Color("11")},
		{"brightblue", "brightblue", lipgloss.Color("12")},
		{"brightmagenta", "brightmagenta", lipgloss.Color("13")},
		{"brightcyan", "brightcyan", lipgloss.Color("14")},
		{"unknown falls back to white", "fuchsia", lipgloss.Color("7")},
		{"empty falls back to white", "", lipgloss.Color("7")},
		{"uppercase falls back to white", "RED", lipgloss.Color("7")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveLipglossColor(tt.input)
			if got != tt.want {
				t.Errorf("ResolveLipglossColor(%q) = %q; want %q", tt.input, got, tt.want)
			}
		})
	}
}
